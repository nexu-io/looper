package depgraph

import (
	"fmt"
	"sort"
	"strings"
)

type IssueRef struct {
	Repo   string
	Number int64
}

func (r IssueRef) String() string {
	r = normalizeRef(r)
	if r.Number <= 0 {
		return ""
	}
	if r.Repo == "" {
		return fmt.Sprintf("#%d", r.Number)
	}
	return fmt.Sprintf("%s#%d", r.Repo, r.Number)
}

type IssueState struct {
	State       string
	StateReason string
}

type Snapshot struct {
	BlockedBy   map[IssueRef][]IssueRef
	Issues      map[IssueRef]IssueState
	Unreachable []IssueRef
}

type Blocker struct {
	Issue            IssueRef
	State            string
	StateReason      string
	Satisfied        bool
	RequiresReTriage bool
	Unreachable      bool

	Number    int64
	Repo      string
	Reachable bool
}

type Cycle []IssueRef

type DependencyGraph struct {
	readySet    []IssueRef
	cycles      []Cycle
	unreachable []IssueRef
	blockers    map[IssueRef][]Blocker
}

func Build(tracked []IssueRef, snapshot Snapshot) DependencyGraph {
	tracked = uniqueSortedRefs(tracked)
	trackedSet := make(map[IssueRef]struct{}, len(tracked))
	for _, ref := range tracked {
		trackedSet[ref] = struct{}{}
	}

	blockedBy := normalizeBlockedBy(snapshot.BlockedBy)
	issueStates := normalizeIssueStates(snapshot.Issues)
	unreachableSet := make(map[IssueRef]struct{}, len(snapshot.Unreachable))
	for _, ref := range snapshot.Unreachable {
		ref = normalizeRef(ref)
		if ref.Number > 0 {
			unreachableSet[ref] = struct{}{}
		}
	}

	readySet := make([]IssueRef, 0, len(tracked))
	blockers := make(map[IssueRef][]Blocker, len(tracked))
	edges := make(map[IssueRef][]IssueRef, len(tracked))

	for _, issue := range tracked {
		deps := uniqueSortedRefs(blockedBy[issue])
		unsatisfied := make([]Blocker, 0, len(deps))
		trackedDeps := make([]IssueRef, 0, len(deps))
		for _, dep := range deps {
			blocker := newBlocker(dep, issueStates)
			if blocker.Satisfied {
				continue
			}
			unsatisfied = append(unsatisfied, blocker)
			if blocker.Unreachable {
				unreachableSet[blocker.Issue] = struct{}{}
				continue
			}
			if _, ok := trackedSet[blocker.Issue]; ok {
				trackedDeps = append(trackedDeps, blocker.Issue)
			}
		}
		if len(unsatisfied) == 0 {
			readySet = append(readySet, issue)
			continue
		}
		blockers[issue] = unsatisfied
		if len(trackedDeps) > 0 {
			edges[issue] = trackedDeps
		}
	}

	return DependencyGraph{
		readySet:    append([]IssueRef(nil), readySet...),
		cycles:      detectCycles(tracked, edges),
		unreachable: refsFromSet(unreachableSet),
		blockers:    blockers,
	}
}

func (g DependencyGraph) ReadySet() []IssueRef {
	return append([]IssueRef(nil), g.readySet...)
}

func (g DependencyGraph) Cycles() []Cycle {
	out := make([]Cycle, 0, len(g.cycles))
	for _, cycle := range g.cycles {
		out = append(out, append(Cycle(nil), cycle...))
	}
	return out
}

func (g DependencyGraph) UnreachableDeps() []IssueRef {
	return append([]IssueRef(nil), g.unreachable...)
}

func (g DependencyGraph) BlockersOf(issue IssueRef) []Blocker {
	rows := g.blockers[normalizeRef(issue)]
	if len(rows) == 0 {
		return nil
	}
	out := make([]Blocker, len(rows))
	copy(out, rows)
	return out
}

func (g *DependencyGraph) Unsatisfied(issueNumber int64) []Blocker {
	if g == nil || issueNumber <= 0 {
		return nil
	}
	for issue, blockers := range g.blockers {
		if issue.Number != issueNumber || len(blockers) == 0 {
			continue
		}
		out := make([]Blocker, len(blockers))
		for index, blocker := range blockers {
			blocker.Number = blocker.Issue.Number
			blocker.Repo = blocker.Issue.Repo
			blocker.Reachable = !blocker.Unreachable
			out[index] = blocker
		}
		return out
	}
	return nil
}

type blockerDisposition struct {
	Satisfied        bool
	RequiresReTriage bool
}

func classifyBlockerState(state IssueState) blockerDisposition {
	normalizedState := strings.ToLower(strings.TrimSpace(state.State))
	normalizedReason := strings.ToLower(strings.TrimSpace(state.StateReason))
	if normalizedState == "closed" && normalizedReason == "completed" {
		return blockerDisposition{Satisfied: true}
	}
	if normalizedState == "closed" && (normalizedReason == "not_planned" || normalizedReason == "duplicate") {
		return blockerDisposition{RequiresReTriage: true}
	}
	return blockerDisposition{}
}

func newBlocker(dep IssueRef, issues map[IssueRef]IssueState) Blocker {
	dep = normalizeRef(dep)
	state, ok := issues[dep]
	if !ok {
		return Blocker{Issue: dep, Unreachable: true}
	}
	state.State = strings.ToLower(strings.TrimSpace(state.State))
	state.StateReason = strings.ToLower(strings.TrimSpace(state.StateReason))
	classification := classifyBlockerState(state)
	return Blocker{
		Issue:            dep,
		State:            state.State,
		StateReason:      state.StateReason,
		Satisfied:        classification.Satisfied,
		RequiresReTriage: classification.RequiresReTriage,
	}
}

func detectCycles(tracked []IssueRef, edges map[IssueRef][]IssueRef) []Cycle {
	state := make(map[IssueRef]int, len(tracked))
	stack := make([]IssueRef, 0, len(tracked))
	stackIndex := map[IssueRef]int{}
	seen := map[string]Cycle{}

	var visit func(IssueRef)
	visit = func(node IssueRef) {
		state[node] = 1
		stackIndex[node] = len(stack)
		stack = append(stack, node)
		for _, next := range edges[node] {
			if idx, ok := stackIndex[next]; ok {
				cycle := append(append([]IssueRef(nil), stack[idx:]...), next)
				normalized := canonicalizeCycle(cycle)
				seen[cycleKey(normalized)] = normalized
				continue
			}
			if state[next] == 0 {
				visit(next)
			}
		}
		stack = stack[:len(stack)-1]
		delete(stackIndex, node)
		state[node] = 2
	}

	for _, node := range tracked {
		if state[node] == 0 {
			visit(node)
		}
	}

	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Cycle, 0, len(keys))
	for _, key := range keys {
		out = append(out, append(Cycle(nil), seen[key]...))
	}
	return out
}

func canonicalizeCycle(cycle []IssueRef) Cycle {
	if len(cycle) < 2 {
		return nil
	}
	nodes := append([]IssueRef(nil), cycle[:len(cycle)-1]...)
	best := nodes
	for index := 1; index < len(nodes); index++ {
		candidate := append(append([]IssueRef(nil), nodes[index:]...), nodes[:index]...)
		if refsSliceLess(candidate, best) {
			best = candidate
		}
	}
	best = append(best, best[0])
	return Cycle(best)
}

func refsSliceLess(left []IssueRef, right []IssueRef) bool {
	for index := 0; index < len(left) && index < len(right); index++ {
		if refLess(left[index], right[index]) {
			return true
		}
		if refLess(right[index], left[index]) {
			return false
		}
	}
	return len(left) < len(right)
}

func cycleKey(cycle Cycle) string {
	parts := make([]string, 0, len(cycle))
	for _, ref := range cycle {
		parts = append(parts, ref.String())
	}
	return strings.Join(parts, "->")
}

func normalizeBlockedBy(input map[IssueRef][]IssueRef) map[IssueRef][]IssueRef {
	out := make(map[IssueRef][]IssueRef, len(input))
	for issue, deps := range input {
		issue = normalizeRef(issue)
		if issue.Number <= 0 {
			continue
		}
		for _, dep := range deps {
			dep = normalizeRef(dep)
			if dep.Number <= 0 {
				continue
			}
			out[issue] = append(out[issue], dep)
		}
	}
	return out
}

func normalizeIssueStates(input map[IssueRef]IssueState) map[IssueRef]IssueState {
	out := make(map[IssueRef]IssueState, len(input))
	for ref, state := range input {
		ref = normalizeRef(ref)
		if ref.Number <= 0 {
			continue
		}
		out[ref] = IssueState{State: strings.TrimSpace(state.State), StateReason: strings.TrimSpace(state.StateReason)}
	}
	return out
}

func normalizeRef(ref IssueRef) IssueRef {
	ref.Repo = strings.ToLower(strings.TrimSpace(ref.Repo))
	return ref
}

func uniqueSortedRefs(input []IssueRef) []IssueRef {
	set := make(map[IssueRef]struct{}, len(input))
	for _, ref := range input {
		ref = normalizeRef(ref)
		if ref.Number <= 0 {
			continue
		}
		set[ref] = struct{}{}
	}
	return refsFromSet(set)
}

func refsFromSet(set map[IssueRef]struct{}) []IssueRef {
	out := make([]IssueRef, 0, len(set))
	for ref := range set {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		return refLess(out[i], out[j])
	})
	return out
}

func refLess(left IssueRef, right IssueRef) bool {
	left = normalizeRef(left)
	right = normalizeRef(right)
	if left.Repo != right.Repo {
		return left.Repo < right.Repo
	}
	return left.Number < right.Number
}

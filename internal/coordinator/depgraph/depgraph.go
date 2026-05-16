package depgraph

import "sort"

type Snapshot struct {
	Issues map[int64]IssueSnapshot
}

type IssueSnapshot struct {
	Number    int64
	BlockedBy []BlockerSnapshot
}

type BlockerSnapshot struct {
	Number      int64
	Repo        string
	State       string
	StateReason string
	Reachable   bool
}

type DependencyGraph struct {
	issues map[int64][]Blocker
}

type Blocker struct {
	Number      int64
	Repo        string
	State       string
	StateReason string
	Reachable   bool
}

func Build(snapshot Snapshot) *DependencyGraph {
	graph := &DependencyGraph{issues: map[int64][]Blocker{}}
	for issueNumber, issue := range snapshot.Issues {
		unsatisfied := make([]Blocker, 0, len(issue.BlockedBy))
		for _, blocker := range issue.BlockedBy {
			if blockerSatisfied(blocker) {
				continue
			}
			unsatisfied = append(unsatisfied, Blocker{
				Number:      blocker.Number,
				Repo:        blocker.Repo,
				State:       blocker.State,
				StateReason: blocker.StateReason,
				Reachable:   blocker.Reachable,
			})
		}
		sort.Slice(unsatisfied, func(i, j int) bool {
			if unsatisfied[i].Number == unsatisfied[j].Number {
				return unsatisfied[i].Repo < unsatisfied[j].Repo
			}
			return unsatisfied[i].Number < unsatisfied[j].Number
		})
		graph.issues[issueNumber] = unsatisfied
	}
	return graph
}

func (g *DependencyGraph) Unsatisfied(issueNumber int64) []Blocker {
	if g == nil {
		return nil
	}
	blockers := g.issues[issueNumber]
	if len(blockers) == 0 {
		return nil
	}
	return append([]Blocker(nil), blockers...)
}

func blockerSatisfied(blocker BlockerSnapshot) bool {
	return blocker.Reachable && blocker.State == "closed" && blocker.StateReason == "completed"
}

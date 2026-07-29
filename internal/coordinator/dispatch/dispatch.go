package dispatch

import (
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/coordinator/depgraph"
	"github.com/nexu-io/looper/internal/domain"
)

const (
	ModeHumanGated = "human-gated"
	ModeAutonomous = "autonomous"

	DispatchPlan      = "dispatch/plan"
	DispatchImplement = "dispatch/implement"

	// AutoLabel is the per-issue opt-in to full autonomy (plan §C/§D): a human
	// labels the issue once and looper carries it plan → implement on its own,
	// without a /plan or /implement command, even when the coordinator is otherwise
	// human-gated.
	AutoLabel = "looper:auto"

	ReactionSuccess = "+1"
	ReactionFailure = "confused"
)

type Comment struct {
	ID                int64
	Author            string
	AuthorAssociation string
	HasWriteAccess    bool
	Body              string
	CreatedAt         time.Time
}

type Issue struct {
	Number    int64
	Labels    []string
	Comments  []Comment
	TriagedAt time.Time
	// HasProductSpec, when set, is whether this work item already has a product spec
	// linked (fed in by the runner from Plane). nil = unknown/not-checked, so the
	// product-spec gate (flowchart node D) is a no-op. Only consulted for features.
	HasProductSpec *bool
}

// KindFeature / KindBug are the triage kind labels the flowchart's node B splits on.
const (
	KindFeature = "kind/feature"
	KindBug     = "kind/bug"
)

type Config struct {
	Mode                 string
	TriagedLabel         string
	HoldLabel            string
	AutonomousDelay      time.Duration
	AllowedUsers         []string
	SlashCommands        []string
	AssignTo             string
	PlannerTriggerLabels []string
	WorkerTriggerLabels  []string
	// RequireProductSpecForPlan gates a feature from reaching the planner until it
	// has a product spec (flowchart node D). Off → today's behaviour.
	RequireProductSpecForPlan bool
}

type Action struct {
	NoOp               bool
	TriggerLabels      []string
	AssignTo           string
	ReactionCommentID  int64
	ReactionContent    string
	FailureCommentBody string
	// RequestProductSpec means the feature can't be planned yet — it has no product
	// spec, so the caller should @product and hold rather than dispatch (node D/E).
	RequestProductSpec bool
}

// productSpecGateHolds reports whether a feature about to be planned must wait for a
// product spec first (flowchart node D). A bug heading to a tech spec is exempt —
// bugs don't need a product spec. A no-op unless the gate is enabled and the
// product-spec status is known to be absent.
func productSpecGateHolds(issue Issue, cfg Config, dispatchLabel string) bool {
	if !cfg.RequireProductSpecForPlan || dispatchLabel != DispatchPlan {
		return false
	}
	if !hasLabel(issue.Labels, KindFeature) {
		return false
	}
	return issue.HasProductSpec != nil && !*issue.HasProductSpec
}

func Decide(issue Issue, cfg Config, now time.Time, graph *depgraph.DependencyGraph) Action {
	// looper:auto opts a single issue into full autonomy regardless of the
	// coordinator's mode: the human labelled it to run on its own, so it takes the
	// autonomous path with no dispatch delay (they've already committed to it). It
	// still respects triage state and dependency gates.
	if hasLabel(issue.Labels, AutoLabel) {
		return decideAutonomous(issue, withImmediateDispatch(cfg), now, graph)
	}
	if cfg.Mode == ModeAutonomous {
		return decideAutonomous(issue, cfg, now, graph)
	}
	return decideHumanGated(issue, cfg, graph)
}

// withImmediateDispatch drops the autonomous cool-off for a looper:auto issue: the
// human explicitly opted in by labelling it, so there's no need to wait out the
// "changed my mind" delay that guards the fully-autonomous mode.
func withImmediateDispatch(cfg Config) Config {
	cfg.AutonomousDelay = 0
	return cfg
}

func NeedsDependencyGate(issue Issue, cfg Config, now time.Time) bool {
	if cfg.Mode == ModeAutonomous {
		return autonomousNeedsDependencyGate(issue, cfg, now)
	}
	return humanNeedsDependencyGate(issue, cfg)
}

func humanNeedsDependencyGate(issue Issue, cfg Config) bool {
	_, command, ok := latestCommandAttempt(issue.Comments, cfg.SlashCommands, cfg.AllowedUsers)
	if !ok || !hasLabel(issue.Labels, cfg.TriagedLabel) {
		return false
	}
	dispatchLabel, ok := singleDispatchLabel(issue.Labels)
	if !ok || dispatchLabel != commandDispatchLabel(command) {
		return false
	}
	triggerLabels := triggerLabelsForDispatch(dispatchLabel, cfg)
	if len(triggerLabels) == 0 {
		return false
	}
	return len(missingLabels(issue.Labels, triggerLabels)) > 0
}

func autonomousNeedsDependencyGate(issue Issue, cfg Config, now time.Time) bool {
	if !hasLabel(issue.Labels, cfg.TriagedLabel) {
		return false
	}
	dispatchLabel, ok := singleDispatchLabel(issue.Labels)
	if !ok {
		return false
	}
	triggerLabels := triggerLabelsForDispatch(dispatchLabel, cfg)
	if len(triggerLabels) == 0 || autonomousDispatchHeld(issue.Labels, cfg.HoldLabel) || len(missingLabels(issue.Labels, triggerLabels)) == 0 {
		return false
	}
	if issue.TriagedAt.IsZero() || now.UTC().Before(issue.TriagedAt.UTC().Add(cfg.AutonomousDelay)) {
		return false
	}
	return true
}

func decideHumanGated(issue Issue, cfg Config, graph *depgraph.DependencyGraph) Action {
	comment, command, ok := latestCommandAttempt(issue.Comments, cfg.SlashCommands, cfg.AllowedUsers)
	if !ok {
		return Action{NoOp: true}
	}

	action := Action{ReactionCommentID: comment.ID}
	if !hasLabel(issue.Labels, cfg.TriagedLabel) {
		return fail(action, "Coordinator can't dispatch until triage finishes.")
	}

	dispatchLabel, ok := singleDispatchLabel(issue.Labels)
	if !ok {
		return fail(action, "Coordinator can't dispatch because triage did not set a dispatch label.")
	}
	if dispatchLabel != commandDispatchLabel(command) {
		return fail(action, "Coordinator can't dispatch because the slash command does not match triage.")
	}

	triggerLabels := triggerLabelsForDispatch(dispatchLabel, cfg)
	if len(triggerLabels) == 0 {
		return fail(action, "Coordinator can't dispatch because the trigger label is not configured.")
	}
	missingLabels := missingLabels(issue.Labels, triggerLabels)
	if len(missingLabels) == 0 {
		action.NoOp = true
		action.ReactionContent = ReactionSuccess
		return action
	}
	if blockers := graph.Unsatisfied(issue.Number); len(blockers) > 0 {
		return fail(action, dependencyFailureBody(blockers))
	}

	action.AssignTo = strings.TrimSpace(cfg.AssignTo)
	action.TriggerLabels = missingLabels
	action.ReactionContent = ReactionSuccess
	return action
}

func decideAutonomous(issue Issue, cfg Config, now time.Time, graph *depgraph.DependencyGraph) Action {
	if !hasLabel(issue.Labels, cfg.TriagedLabel) {
		return Action{NoOp: true}
	}
	dispatchLabel, ok := singleDispatchLabel(issue.Labels)
	if !ok {
		return Action{NoOp: true}
	}
	triggerLabels := triggerLabelsForDispatch(dispatchLabel, cfg)
	if len(triggerLabels) == 0 {
		return Action{NoOp: true}
	}
	if autonomousDispatchHeld(issue.Labels, cfg.HoldLabel) || len(missingLabels(issue.Labels, triggerLabels)) == 0 {
		return Action{NoOp: true}
	}
	if issue.TriagedAt.IsZero() || now.UTC().Before(issue.TriagedAt.UTC().Add(cfg.AutonomousDelay)) {
		return Action{NoOp: true}
	}
	if len(graph.Unsatisfied(issue.Number)) > 0 {
		return Action{NoOp: true}
	}
	// Node D: a feature can't be planned until it has a product spec. Everything else
	// is satisfied, but there's no spec → ask product for one and hold instead.
	if productSpecGateHolds(issue, cfg, dispatchLabel) {
		return Action{RequestProductSpec: true}
	}
	return Action{AssignTo: strings.TrimSpace(cfg.AssignTo), TriggerLabels: missingLabels(issue.Labels, triggerLabels)}
}

func fail(action Action, body string) Action {
	action.NoOp = true
	action.ReactionContent = ReactionFailure
	action.FailureCommentBody = strings.TrimSpace(body)
	return action
}

func latestCommandAttempt(comments []Comment, slashCommands []string, allowedUsers []string) (Comment, string, bool) {
	for index := len(comments) - 1; index >= 0; index-- {
		comment := comments[index]
		command, ok := ParseSlashCommand(comment.Body, slashCommands)
		if !ok {
			continue
		}
		if !isAllowedUser(comment, allowedUsers) {
			continue
		}
		return comment, command, true
	}
	return Comment{}, "", false
}
func ParseSlashCommand(body string, configured []string) (string, bool) {
	allowed := configuredCommands(configured)
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || strings.HasPrefix(trimmed, ">") {
			continue
		}
		switch {
		case allowed["/plan"] && strings.HasPrefix(trimmed, "/plan") && commandBoundary(trimmed, len("/plan")):
			return "/plan", true
		case allowed["/implement"] && strings.HasPrefix(trimmed, "/implement") && commandBoundary(trimmed, len("/implement")):
			return "/implement", true
		}
	}
	return "", false
}

func configuredCommands(configured []string) map[string]bool {
	allowed := map[string]bool{"/plan": false, "/implement": false}
	for _, command := range configured {
		command = strings.TrimSpace(command)
		if _, ok := allowed[command]; ok {
			allowed[command] = true
		}
	}
	return allowed
}

func commandBoundary(value string, index int) bool {
	if len(value) == index {
		return true
	}
	switch value[index] {
	case ' ', '\t', '\r':
		return true
	default:
		return false
	}
}

func isAllowedUser(comment Comment, allowedUsers []string) bool {
	for _, user := range allowedUsers {
		if strings.EqualFold(strings.TrimSpace(user), comment.Author) {
			return true
		}
	}
	return comment.HasWriteAccess
}

func singleDispatchLabel(labels []string) (string, bool) {
	match := ""
	for _, label := range labels {
		if !strings.HasPrefix(label, "dispatch/") {
			continue
		}
		if match != "" {
			return "", false
		}
		match = label
	}
	return match, match != ""
}

func triggerLabelsForDispatch(dispatchLabel string, cfg Config) []string {
	switch dispatchLabel {
	case DispatchPlan:
		return compactLabels(cfg.PlannerTriggerLabels)
	case DispatchImplement:
		return compactLabels(cfg.WorkerTriggerLabels)
	default:
		return nil
	}
}

func compactLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label != "" {
			out = append(out, label)
		}
	}
	return out
}

func missingLabels(existing []string, want []string) []string {
	missing := make([]string, 0, len(want))
	for _, label := range want {
		if !hasLabel(existing, label) {
			missing = append(missing, label)
		}
	}
	return missing
}

func commandDispatchLabel(command string) string {
	switch command {
	case "/plan":
		return DispatchPlan
	case "/implement":
		return DispatchImplement
	default:
		return ""
	}
}

func hasLabel(labels []string, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func autonomousDispatchHeld(labels []string, legacyHoldLabel string) bool {
	// Coordinator autonomous dispatch is only blocked by the official global hold.
	// Lane-specific official holds apply later in each lane's own discovery gate.
	if hasLabel(labels, domain.HoldLabelGlobal) {
		return true
	}
	return hasLabel(labels, strings.TrimSpace(legacyHoldLabel))
}

func dependencyFailureBody(blockers []depgraph.Blocker) string {
	lines := []string{"Coordinator can't dispatch because blocked_by is still unsatisfied:"}
	for _, blocker := range blockers {
		lines = append(lines, fmt.Sprintf("- %s (%s)", blockerReference(blocker), blockerStateReason(blocker)))
	}
	return strings.Join(lines, "\n")
}

func blockerReference(blocker depgraph.Blocker) string {
	repo := strings.TrimSpace(blocker.Repo)
	if repo != "" {
		return fmt.Sprintf("%s#%d", repo, blocker.Number)
	}
	return fmt.Sprintf("#%d", blocker.Number)
}

func blockerStateReason(blocker depgraph.Blocker) string {
	if !blocker.Reachable {
		return "unreachable"
	}
	if reason := strings.TrimSpace(blocker.StateReason); reason != "" {
		return reason
	}
	if state := strings.TrimSpace(blocker.State); state != "" {
		return state
	}
	return "unknown"
}

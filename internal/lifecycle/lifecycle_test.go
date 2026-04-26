package lifecycle

import (
	"strings"
	"testing"
)

func TestFromMapNormalizesAgentLifecycleState(t *testing.T) {
	state, err := FromMap(map[string]any{
		"branch":      " looper/test ",
		"base_branch": "main",
		"commit_shas": []any{"abc123", "", "abc123"},
		"pushed":      true,
		"pr_number":   float64(84),
		"actions": map[string]any{
			"commit": "agent",
			"push":   "agent",
			"pr":     "agent",
		},
	})
	if err != nil {
		t.Fatalf("FromMap() error = %v", err)
	}
	if state.Policy != PolicyAgentManagedWithFallback || state.PolicyVersion != PolicyVersion {
		t.Fatalf("policy = %q/%d, want default policy", state.Policy, state.PolicyVersion)
	}
	if state.Branch != "looper/test" || len(state.CommitSHAs) != 1 || state.CommitSHAs[0] != "abc123" || state.Actions.Push != ActionSourceAgent {
		t.Fatalf("state = %#v, want normalized lifecycle", state)
	}
}

func TestMergeAgentPreservesFallbackMetadata(t *testing.T) {
	state := NewState(AgentManagedWithFallbackPolicy("worker", true), "looper/test", "main")
	state.Actions.PR = ActionSourceFallback
	state.PRNumber = 84

	agent := &State{CommitSHAs: []string{"abc123"}, Pushed: true, Actions: Actions{Commit: ActionSourceAgent, Push: ActionSourceAgent}}
	state.MergeAgent(agent, "2026-04-26T00:00:00.000Z")

	if state.Actions.PR != ActionSourceFallback || state.PRNumber != 84 {
		t.Fatalf("merged PR metadata = %#v, want fallback PR preserved", state)
	}
	if state.Actions.Commit != ActionSourceAgent || state.Actions.Push != ActionSourceAgent || state.AgentIngestedAt == "" {
		t.Fatalf("merged agent metadata = %#v, want agent commit/push", state)
	}
}

func TestPromptInstructionDocumentsLifecycleContract(t *testing.T) {
	prompt := PromptInstruction("worker", "looper/test", "main", true, true)
	for _, want := range []string{PolicyAgentManagedWithFallback, "git_pr_lifecycle", "commitShas", "actions {commit,push,pr}", "fallbackAllowed=true"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("PromptInstruction missing %q in:\n%s", want, prompt)
		}
	}
}

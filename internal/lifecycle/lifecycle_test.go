package lifecycle

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
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

	agent := &State{Branch: "fix/test", BaseBranch: "main", CommitSHAs: []string{"abc123"}, Pushed: true, Actions: Actions{Commit: ActionSourceAgent, Push: ActionSourceAgent}}
	state.MergeAgent(agent, "2026-04-26T00:00:00.000Z")

	if state.Actions.PR != ActionSourceFallback || state.PRNumber != 84 {
		t.Fatalf("merged PR metadata = %#v, want fallback PR preserved", state)
	}
	if state.Actions.Commit != ActionSourceAgent || state.Actions.Push != ActionSourceAgent || state.AgentIngestedAt == "" {
		t.Fatalf("merged agent metadata = %#v, want agent commit/push", state)
	}
	if state.PlannedBranch != "looper/test" || state.AgentBranch != "fix/test" || state.ActiveBranch != "looper/test" || state.BranchProvenance != BranchProvenancePlanned {
		t.Fatalf("merged branch provenance = %#v, want planned branch preserved alongside agent branch", state)
	}
}

func TestStateSetActiveBranchMarksAgentMigration(t *testing.T) {
	state := NewState(AgentManagedWithFallbackPolicy("worker", true), "looper/test", "main")
	state.RecordAgentBranch("fix/test", "main")
	state.SetActiveBranch("fix/test", "main", BranchProvenanceAgentMigrated)

	if state.PlannedBranch != "looper/test" || state.AgentBranch != "fix/test" || state.ActiveBranch != "fix/test" || state.BranchProvenance != BranchProvenanceAgentMigrated {
		t.Fatalf("state = %#v, want migrated active branch with provenance", state)
	}
}

func TestPromptInstructionDocumentsLifecycleContract(t *testing.T) {
	prompt := PromptInstruction("worker", "looper/test", "main", true, true, config.DefaultDisclosureConfig(), "opencode", "")
	for _, want := range []string{PolicyAgentManagedWithFallback, "git_pr_lifecycle", "commitShas", "actions {commit,push,pr}", "fallbackAllowed=true"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("PromptInstruction missing %q in:\n%s", want, prompt)
		}
	}
}

func TestPromptInstructionDocumentsAgentModelWhenConfigured(t *testing.T) {
	prompt := PromptInstruction("worker", "looper/test", "main", true, true, config.DefaultDisclosureConfig(), "opencode", "openai/gpt-5.5")
	for _, want := range []string{"agent=opencode", "model=openai/gpt-5.5", "Generated-By: looper", `🔁 Powered by <a href="https://github.com/nexu-io/looper">Looper</a>`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("PromptInstruction missing %q in:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{"agent=<agent-runtime>", "model=<agent-model>", "agent=gpt-5.5", "agent=openai/gpt-5.5"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("PromptInstruction contains %q in:\n%s", unwanted, prompt)
		}
	}

}

func TestPromptInstructionOmitsMissingAgentRuntime(t *testing.T) {
	prompt := PromptInstruction("worker", "looper/test", "main", true, true, config.DefaultDisclosureConfig(), "", "openai/gpt-5.5")
	if strings.Contains(prompt, "agent=") {
		t.Fatalf("PromptInstruction should omit agent when runtime is missing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "model=openai/gpt-5.5") {
		t.Fatalf("PromptInstruction should still include configured model:\n%s", prompt)
	}
}

func TestPromptInstructionRespectsDisabledDisclosure(t *testing.T) {
	cfg := config.DefaultDisclosureConfig()
	cfg.Enabled = false
	prompt := PromptInstruction("fixer", "", "", true, false, cfg, "opencode", "")
	if strings.Contains(prompt, "add a commit body trailer") || strings.Contains(prompt, "add this Markdown footer") {
		t.Fatalf("PromptInstruction should not request disclosure trailers/footers when disabled:\n%s", prompt)
	}
	if !strings.Contains(prompt, "disclosure stamping is disabled") || !strings.Contains(prompt, "do not add looper") {
		t.Fatalf("PromptInstruction missing disabled disclosure guard:\n%s", prompt)
	}
}

func TestPromptInstructionRespectsDisclosureChannelOptOuts(t *testing.T) {
	cfg := config.DefaultDisclosureConfig()
	cfg.Channels.GitCommit = false
	cfg.Channels.PullRequest = false
	cfg.Channels.ReviewComment = false
	prompt := PromptInstruction("worker", "looper/test", "main", true, true, cfg, "opencode", "")
	for _, want := range []string{"Do not add looper Generated-By trailers", "Do not add looper Markdown disclosure footers to PR bodies", "Do not add looper disclosure footers or hidden looper stamp markers to inline review comments"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("PromptInstruction missing %q in:\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, "issue bodies or normal comments") {
		t.Fatalf("PromptInstruction should still mention enabled issue-comment disclosure:\n%s", prompt)
	}
}

func TestPromptInstructionLeavesPRDisclosureStampingToLooper(t *testing.T) {
	prompt := PromptInstruction("worker", "looper/test", "main", true, true, config.DefaultDisclosureConfig(), "opencode", "")
	if !strings.Contains(prompt, "For PR bodies, do not add looper Markdown disclosure footers yourself; Looper will append or normalize the PR disclosure footer during PR creation or update.") {
		t.Fatalf("PromptInstruction missing PR stamping guidance:\n%s", prompt)
	}
	if !strings.Contains(prompt, "For generated issue bodies or normal comments, add this Markdown footer") {
		t.Fatalf("PromptInstruction should preserve issue comment disclosure guidance:\n%s", prompt)
	}
}

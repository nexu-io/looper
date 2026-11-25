package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestProcessClaimedItemCustomReplacementDoesNotNeedBuiltinTempDir(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: team-review\ndescription: team method\n---\nReview the change.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HITL.Enabled = false
	cfg.Roles.Reviewer.Skills = config.ReviewerSkillsConfig{Mode: config.ReviewerSkillsModeReplace, Required: []string{skillPath}}
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = config.ReviewerReviewEventComment
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", ParseStatus: "parsed", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[]}`}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventComment}, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, LoopConfig: testReviewerLoopConfig()})
	repo, prNumber := "acme/looper", int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "custom_skills", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber}); err != nil {
		t.Fatal(err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "custom-skills-worker", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	// All runtime fixtures already exist; builtin materialization cannot succeed.
	t.Setenv("TMPDIR", filepath.Join(dir, "missing-temp-directory"))
	t.Setenv("TMP", filepath.Join(dir, "missing-temp-directory"))
	t.Setenv("TEMP", filepath.Join(dir, "missing-temp-directory"))
	result, err := runner.ProcessClaimedItem(ctx, *claim)
	if err != nil || result.Status != "success" || len(agent.starts) != 1 {
		t.Fatalf("custom-only review = (%#v, %v), starts = %d", result, err, len(agent.starts))
	}
	if !strings.Contains(agent.starts[0].Prompt, skillPath) {
		t.Fatal("custom review method was not provided to the agent")
	}
}

func TestSandboxPersonalReviewerReadsDaemonInputs(t *testing.T) {
	f := newRunnerFixture(t)
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.HITL.Enabled = false
	cfg.Roles.Reviewer.Nono = &config.ReviewerNonoConfig{Profile: "/operator/profile.json"}
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = config.ReviewerReviewEventComment
	gateway := &fakeGitHubGateway{issueComments: []map[string]any{{"body": "conversation evidence"}}, reviewThreads: []ReviewThread{{ID: "inline-thread-id"}}, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventComment}
	var inputs string
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", ParseStatus: "parsed", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[]}`}}, onStart: func(run AgentRunInput) {
		inputs = run.SandboxInputDir
		path := filepath.Join(inputs, "pull-request.json")
		data, err := os.ReadFile(path)
		if inputs == "" || err != nil || !strings.Contains(run.Prompt, path) || strings.Contains(run.Prompt, "Before acting and again before final conclusions") {
			t.Fatalf("invalid personal review inputs %q: %v", inputs, err)
		}
		for _, want := range []string{"abc123", "inline-thread-id", "conversation evidence"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("missing %q", want)
			}
		}
		entries, err := filepath.Glob(filepath.Join(inputs, "looper-review-skills-*", "SKILL.md"))
		if err != nil || len(entries) != 1 || !strings.Contains(run.Prompt, entries[0]) {
			t.Errorf("missing sandboxed skill: %v %v", entries, err)
		}
	}}
	runner := New(Options{DB: f.coordinator.DB(), Repos: f.repos, GitHub: gateway, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: f.logger, Now: f.now, CustomInstructions: &cfg, LoopConfig: testReviewerLoopConfig()})
	ctx := context.Background()
	repo, num := "acme/looper", int64(42)
	loop := storage.LoopRecord{ID: "personal-review", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &num, Status: "queued", CreatedAt: f.nowISO(), UpdatedAt: f.nowISO()}
	if err := f.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.enqueue(ctx, enqueueInput{ProjectID: loop.ProjectID, LoopID: loop.ID, Repo: repo, PRNumber: num}); err != nil {
		t.Fatal(err)
	}
	claim, err := f.repos.Queue.ClaimNextOfType(ctx, f.nowISO(), "review-worker", "reviewer")
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	result, err := runner.ProcessClaimedItem(ctx, *claim)
	if err != nil || result.Status != "success" || inputs == "" {
		t.Fatalf("review: %#v %v", result, err)
	}
	if _, err := os.Stat(inputs); !os.IsNotExist(err) {
		t.Fatalf("review inputs survived completion: %v", err)
	}
}

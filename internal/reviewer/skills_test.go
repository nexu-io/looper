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

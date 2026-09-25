package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/storage"
)

func groupedReviewLifecycleFixture(t *testing.T) (*Runner, stepInput, *fakeGitHubGateway, *fakeAgentExecutor) {
	t.Helper()
	fixture := newRunnerFixture(t)
	worktree, base, _ := groupingTestRepoWithRename(t)
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("second group\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var head string
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-qm", "docs: add second group"}, {"rev-parse", "HEAD"}} {
		out, err := exec.Command("git", append([]string{"-C", worktree}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		head = strings.TrimSpace(string(out))
	}
	ctx := context.Background()
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("load project: %#v, %v", project, err)
	}
	metadata, err := json.Marshal(map[string]string{"worktreeRoot": filepath.Dir(worktree)})
	if err != nil {
		t.Fatal(err)
	}
	project.MetadataJSON = stringPtr(string(metadata))
	if err := fixture.repos.Projects.Upsert(ctx, *project); err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: "grouped_loop", Seq: 1, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", Repo: stringPtr("acme/looper"), PRNumber: int64Ptr(42), Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	run := storage.RunRecord{ID: "grouped_run", LoopID: loop.ID, Status: "running", StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Reviewer.Behavior.RelatedFileGroups = config.ReviewerRelatedFileGroupsConfig{Enabled: true, MinChangedFiles: 1}
	cfg.Roles.Reviewer.Behavior.PublishMode = config.ReviewerPublishModeSummaryComment
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = config.ReviewerReviewEventComment
	completed := AgentResult{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[]}`}
	agent := &fakeAgentExecutor{results: []AgentResult{completed, completed, completed}}
	github := &fakeGitHubGateway{viewHeadSHA: head}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AgentRuntime: string(config.AgentVendorCodex), CustomInstructions: &cfg})
	input := stepInput{Project: *project, Loop: loop, Run: run, Repo: "acme/looper", PRNumber: 42, Checkpoint: reviewerCheckpoint{
		Detail: &checkpointDetail{HeadRefName: "feature", BaseRefName: "main"}, Snapshot: &checkpointSnapshot{BaseSHA: base, HeadSHA: head},
		Worktree: &checkpointWorktree{Path: worktree, Branch: "feature", PreparedAt: fixture.nowISO()},
	}}
	return runner, input, github, agent
}

func TestGroupedReviewRestartsOnRemoteHeadDrift(t *testing.T) {
	for _, afterStarts := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("after_%d_groups", afterStarts), func(t *testing.T) {
			t.Parallel()
			runner, input, github, agent := groupedReviewLifecycleFixture(t)
			if afterStarts == 0 {
				github.viewHeadSHA = "new-head"
			}
			agent.onStart = func(AgentRunInput) {
				if len(agent.starts) == afterStarts {
					github.viewHeadSHA = "new-head"
				}
			}
			checkpoint, err := runner.executeStep(context.Background(), stepReview, input)
			var failure *loopError
			if !errors.As(err, &failure) || !failure.interrupted || checkpoint.ResumePolicy != "restart_from_discover" {
				t.Fatalf("checkpoint policy = %q, err = %v; want interrupted rediscovery", checkpoint.ResumePolicy, err)
			}
			if len(agent.starts) != afterStarts {
				t.Fatalf("started %d agents after remote drift, want only %d groups", len(agent.starts), afterStarts)
			}
			for _, start := range agent.starts {
				if start.Metadata["phase"] != "review-group" {
					t.Fatal("publish-capable final reviewer started on a stale head")
				}
			}
		})
	}
}

func TestGroupedReviewRenewsAgentBudget(t *testing.T) {
	t.Parallel()
	runner, input, _, agent := groupedReviewLifecycleFixture(t)
	runner.agentTimeout = time.Second
	agent.wait = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
			return nil
		}
	}
	checkpoint, err := runner.executeStep(context.Background(), stepReview, input)
	if err != nil || checkpoint.PendingReview == nil || len(agent.starts) != 3 {
		t.Fatalf("grouped pass = %v, pending = %#v, starts = %d; want two groups and final review", err, checkpoint.PendingReview, len(agent.starts))
	}
}

func TestGroupedReviewHonorsParentCancellation(t *testing.T) {
	t.Parallel()
	runner, input, _, agent := groupedReviewLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var contextPath string
	agent.onStart = func(start AgentRunInput) {
		var err error
		contextPath, _, err = groupedContextPayload(start.Prompt)
		if err != nil {
			t.Error(err)
		}
		cancel()
	}
	agent.wait = func(ctx context.Context) error { return ctx.Err() }
	_, err := runner.executeStep(ctx, stepReview, input)
	if !errors.Is(err, context.Canceled) || len(agent.starts) != 1 {
		t.Fatalf("canceled grouped pass: err=%v, starts=%d", err, len(agent.starts))
	}
	if contextPath != "" {
		if _, err := os.Stat(filepath.Dir(contextPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("group context directory remains after cancellation: %v", err)
		}
	}
}

func TestGroupedReviewRetainsResumeContextAfterRetry(t *testing.T) {
	t.Parallel()
	runner, input, _, agent := groupedReviewLifecycleFixture(t)
	ctx := context.Background()
	input.Loop.MetadataJSON = stringPtr(fmt.Sprintf(`{"lastPublishedHeadSha":%q}`, input.Checkpoint.Snapshot.BaseSHA))
	for i, phase := range []string{"review", "review-group"} {
		record := storage.AgentExecutionRecord{
			ID: fmt.Sprintf("previous_%d", i), ProjectID: &input.Project.ID, LoopID: &input.Loop.ID, RunID: &input.Run.ID,
			Vendor: string(config.AgentVendorCodex), Status: "completed", MetadataJSON: stringPtr(fmt.Sprintf(`{"metadata":{"phase":%q}}`, phase)),
			StartedAt: fmt.Sprintf("2026-04-11T11:00:0%d.000Z", i), CreatedAt: input.Run.CreatedAt, UpdatedAt: input.Run.UpdatedAt,
		}
		if phase == "review" {
			record.NativeSessionID, record.NativeResumeStatus = stringPtr("pending-review"), stringPtr("pending")
		}
		if err := runner.repos.AgentExecutions.Upsert(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	agent.results[0] = AgentResult{Status: "completed", Summary: "Grouped contract finding", Stdout: `__LOOPER_RESULT__={"summary":"Grouped contract finding","outcome":"blocking","findings":[{"title":"Grouped contract finding","body":"Introduced broken contract","disposition":"must_fix","severity":"blocking","scopeBasis":"introduced_regression","scopeEvidence":"changed call site"}]}`}
	var finalContexts [][]byte
	agent.onStart = func(start AgentRunInput) {
		if start.Metadata["phase"] != "review" {
			return
		}
		for _, prompt := range []string{start.Prompt, start.NativeResumePrompt} {
			_, payload, err := groupedContextPayload(prompt)
			if err != nil {
				t.Error(err)
				continue
			}
			finalContexts = append(finalContexts, payload)
		}
	}
	if _, err := runner.executeStep(ctx, stepReview, input); err != nil {
		t.Fatal(err)
	}
	if len(agent.starts) != 3 {
		t.Fatalf("starts = %d, want two groups and final review", len(agent.starts))
	}
	for _, start := range agent.starts[:2] {
		if !strings.Contains(start.Prompt, "Repair frontier contract") {
			t.Fatal("grouped retry lost the repair-frontier scope")
		}
	}
	final := agent.starts[2]
	for _, prompt := range []string{final.Prompt, final.NativeResumePrompt} {
		if !strings.Contains(prompt, "Related-file group plan") {
			t.Fatalf("final review lost grouped context: %s", prompt)
		}
	}
	if len(finalContexts) != 2 {
		t.Fatalf("readable final contexts = %d, want full and resumed", len(finalContexts))
	}
	for _, payload := range finalContexts {
		if !strings.Contains(string(payload), "Grouped contract finding") {
			t.Fatal("final review context lost group findings")
		}
	}
	if !strings.Contains(final.NativeResumePrompt, "Continue the existing Looper reviewer review task") {
		t.Fatalf("lost pending session after a persisted group: %s", final.NativeResumePrompt)
	}
}

func TestGroupedReviewUsesConfiguredGuidance(t *testing.T) {
	for _, tc := range []struct {
		phase       string
		labels      []string
		instruction string
	}{
		{phase: "implementation", instruction: "Focus on code correctness, safety, tests, and maintainability."},
		{phase: "spec", labels: []string{specpr.ReviewingLabel}, instruction: "Focus on scope, correctness, feasibility, risks, and validation."},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			t.Parallel()
			runner, input, github, agent := groupedReviewLifecycleFixture(t)
			skillPath := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(skillPath, []byte("---\nname: project-review\ndescription: Project review method\n---\nCheck the project contract.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runner.projectRoleConfig.Roles.Reviewer.Skills = config.ReviewerSkillsConfig{Mode: config.ReviewerSkillsModeReplace, Required: []string{skillPath}}
			custom := "Audit project-specific compatibility requirements."
			runner.customInstructions.Instructions.Enabled = true
			runner.customInstructions.Roles.Reviewer.Instructions = "Superseded global review guidance."
			runner.customInstructions.Projects = []config.ProjectRefConfig{{ID: input.Project.ID, Roles: &config.PartialRoleConfigs{Reviewer: &config.PartialReviewerRoleConfig{Instructions: &custom}}}}
			runner.scope = config.ReviewerScopeChangedFiles
			input.Checkpoint.Detail.Labels = tc.labels
			github.labels = tc.labels
			if _, err := runner.executeStep(context.Background(), stepReview, input); err != nil {
				t.Fatal(err)
			}
			if len(agent.starts) != 3 {
				t.Fatalf("starts = %d, want two groups and final review", len(agent.starts))
			}
			for _, start := range agent.starts[:2] {
				for _, want := range []string{skillPath, "required: true", "Every listed skill MUST be read", custom, "Phase: " + tc.phase, tc.instruction, "Review scope: changed_files", "Do not publish", "Review only these paths"} {
					if !strings.Contains(start.Prompt, want) {
						t.Errorf("group prompt missing %q", want)
					}
				}
				if strings.Contains(start.Prompt, "Superseded global review guidance") || strings.Contains(start.Prompt, "name: looper-review") {
					t.Error("group prompt ignored configured instruction/skill replacement")
				}
			}
		})
	}
}

func groupedContextPayload(prompt string) (string, []byte, error) {
	const prefix = "Related-file group context (JSON file): "
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, prefix) {
			var path string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &path); err != nil {
				return "", nil, err
			}
			payload, err := os.ReadFile(path)
			return path, payload, err
		}
	}
	return "", nil, fmt.Errorf("group context file reference missing")
}

func TestGroupedReviewBoundsLargePathContext(t *testing.T) {
	t.Parallel()
	runner, input, github, agent := groupedReviewLifecycleFixture(t)
	worktree := input.Checkpoint.Worktree.Path
	for i := 0; i < 800; i++ {
		path := filepath.Join(worktree, fmt.Sprintf("%04d_%s.go", i, strings.Repeat("p", 180)))
		if err := os.WriteFile(path, []byte("package old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oddDir := filepath.Join(worktree, "nested\ncontrol\x01")
	if err := os.Mkdir(oddDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oddDir, "file.go"), []byte("package nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "test: large path context"}, {"rev-parse", "HEAD"}} {
		out, err := exec.Command("git", append([]string{"-C", worktree}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		input.Checkpoint.Snapshot.HeadSHA = strings.TrimSpace(string(out))
	}
	github.viewHeadSHA = input.Checkpoint.Snapshot.HeadSHA
	expected, err := listChangedPaths(context.Background(), "git", worktree, input.Checkpoint.Snapshot.BaseSHA, github.viewHeadSHA, false)
	if err != nil {
		t.Fatal(err)
	}
	agent.results = append(agent.results, agent.results[0])
	var contextPath string
	agent.onStart = func(start AgentRunInput) {
		if len(start.Prompt) > 32*1024 {
			t.Errorf("large path list expanded %s prompt to %d bytes", start.Metadata["phase"], len(start.Prompt))
		}
		path, payload, err := groupedContextPayload(start.Prompt)
		if err != nil {
			t.Error(err)
			return
		}
		contextPath = path
		var data struct {
			Groups       []fileGroup   `json:"groups"`
			ChangedFiles []changedFile `json:"changedFiles"`
		}
		if err := json.Unmarshal(payload, &data); err != nil {
			t.Error(err)
			return
		}
		if !reflect.DeepEqual(data.ChangedFiles, expected) {
			t.Error("context file lost or changed source paths")
		}
		assigned := map[string]bool{}
		for _, group := range data.Groups {
			for _, path := range group.Paths {
				if assigned[path] {
					t.Errorf("path assigned twice: %q", path)
				}
				assigned[path] = true
			}
		}
		if len(assigned) != len(expected) {
			t.Errorf("assigned %d paths, want %d", len(assigned), len(expected))
		}
		for _, file := range expected {
			if !assigned[file.Path] {
				t.Errorf("unassigned path in context: %q", file.Path)
			}
		}
	}
	if _, err := runner.executeStep(context.Background(), stepReview, input); err != nil {
		t.Fatal(err)
	}
	if len(agent.starts) != 4 {
		t.Fatalf("starts = %d, want three groups and final review", len(agent.starts))
	}
	if contextPath != "" {
		if _, err := os.Stat(filepath.Dir(contextPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("group context directory remains after completion: %v", err)
		}
	}
}

func TestGroupedReviewDiffsRewrittenRepairFrontierDirectly(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprintf("repair_%t", repair), func(t *testing.T) {
			t.Parallel()
			runner, input, github, agent := groupedReviewLifecycleFixture(t)
			worktree := input.Checkpoint.Worktree.Path
			git := func(args ...string) string {
				t.Helper()
				out, err := exec.Command("git", append([]string{"-C", worktree}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			if err := os.WriteFile(filepath.Join(worktree, "removed-after-rewrite.go"), []byte("package removed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-qm", "feat: previously reviewed file")
			previous := git("rev-parse", "HEAD")
			git("checkout", "--detach", input.Checkpoint.Snapshot.BaseSHA)
			if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("rewritten head\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-qm", "feat: rewritten branch")
			head := git("rev-parse", "HEAD")
			input.Checkpoint.Snapshot.HeadSHA, github.viewHeadSHA = head, head
			if repair {
				input.Loop.MetadataJSON = stringPtr(fmt.Sprintf(`{"lastPublishedHeadSha":%q}`, previous))
			}
			agent.onStart = func(start AgentRunInput) {
				_, payload, err := groupedContextPayload(start.Prompt)
				if err != nil {
					t.Error(err)
					return
				}
				var data struct {
					ChangedFiles []changedFile `json:"changedFiles"`
				}
				if err := json.Unmarshal(payload, &data); err != nil {
					t.Error(err)
					return
				}
				found := false
				for _, file := range data.ChangedFiles {
					if file.Path == "removed-after-rewrite.go" && file.Status == "D" {
						found = true
					}
				}
				if found != repair {
					t.Errorf("prior-head-only deletion present=%t, repair=%t", found, repair)
				}
				if repair && start.Metadata["phase"] == "review-group" && !strings.Contains(start.Prompt, "git diff "+previous+" "+head+" -- <path>") {
					t.Error("repair group missing direct endpoint diff instruction")
				}
			}
			if _, err := runner.executeStep(context.Background(), stepReview, input); err != nil {
				t.Fatal(err)
			}
			wantStarts := 2
			if repair {
				wantStarts = 3
			}
			if len(agent.starts) != wantStarts {
				t.Fatalf("starts=%d, want %d", len(agent.starts), wantStarts)
			}
		})
	}
}

func TestGroupedReviewUsesRefreshedPhaseLabels(t *testing.T) {
	t.Parallel()
	runner, input, github, agent := groupedReviewLifecycleFixture(t)
	github.labels = []string{specpr.ReviewingLabel}
	agent.onStart = func(AgentRunInput) {
		if len(agent.starts) == 1 {
			github.labels = nil
		}
		if len(agent.starts) == 2 {
			github.labels = []string{specpr.ReviewingLabel}
		}
	}
	if _, err := runner.executeStep(context.Background(), stepReview, input); err != nil {
		t.Fatal(err)
	}
	if len(agent.starts) != 3 {
		t.Fatalf("starts=%d", len(agent.starts))
	}
	for i, phase := range []string{"spec", "implementation", "spec"} {
		if !strings.Contains(agent.starts[i].Prompt, "Phase: "+phase) {
			t.Errorf("execution %d did not use live %s phase", i, phase)
		}
	}
}

func TestGroupedReviewIncludesProviderContext(t *testing.T) {
	for _, mode := range []string{"github", "forgejo", "hosted-github", "hosted-forgejo"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			runner, input, _, agent := groupedReviewLifecycleFixture(t)
			ctx := context.Background()
			isForgejo := strings.Contains(mode, "forgejo")
			kind, identityKind, baseURL := config.ProviderKindGitHub, config.HostingIdentityGitHubApp, "https://github.com"
			if isForgejo {
				kind, identityKind, baseURL = config.ProviderKindForgejo, config.HostingIdentityForgejoToken, "https://forge.example"
				runner.customInstructions.Providers = []config.ProviderConfig{{ID: "forge", Kind: kind, BaseURL: baseURL, Auth: config.ProviderAuthTea, TeaLogin: stringPtr("review-test")}}
				runner.customInstructions.Projects = []config.ProjectRefConfig{{ID: input.Project.ID, Provider: "forge", Repo: input.Repo}}
			}
			if strings.HasPrefix(mode, "hosted-") {
				var err error
				ctx, err = hostingidentity.BindResolved(ctx, config.ResolvedHostingIdentity{Name: "test", ProjectID: input.Project.ID, Role: "reviewer", Definition: config.HostingIdentityConfig{Kind: identityKind, BaseURL: baseURL, AppID: 1, InstallationID: 2, PrivateKeyFile: "unused.pem", TokenEnv: "UNUSED_TEST_TOKEN"}, Target: config.RepositoryIdentity{Kind: kind, BaseURL: baseURL, Repo: input.Repo}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := runner.executeStep(ctx, stepReview, input); err != nil {
				t.Fatal(err)
			}
			for _, start := range agent.starts[:2] {
				for _, want := range []string{"Minimal PR seed", `"pr_number": 42`, input.Checkpoint.Snapshot.HeadSHA, "Do not publish"} {
					if !strings.Contains(start.Prompt, want) {
						t.Errorf("group missing %q", want)
					}
				}
				if strings.HasPrefix(mode, "hosted-") {
					if !strings.Contains(start.Prompt, `"$LOOPER_HOST_CLI" host api pulls/42`) || strings.Contains(start.Prompt, "gh pr view") || strings.Contains(start.Prompt, "tea' api") {
						t.Error("hosted group did not get exclusive host transport")
					}
				} else if isForgejo {
					if !strings.Contains(start.Prompt, "https://forge.example/acme/looper/pulls/42") || !strings.Contains(start.Prompt, "'tea' api --login 'review-test'") || strings.Contains(start.Prompt, "gh pr view") {
						t.Error("Forgejo group did not get configured PR/read transport")
					}
				} else if !strings.Contains(start.Prompt, "gh pr view") {
					t.Error("GitHub group missing read context")
				}
			}
		})
	}
}

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/e2e/harness"
	"github.com/nexu-io/looper/internal/storage"
)

func TestInvariantWorkerUsesIsolatedWorktreeAndLeavesUserRepoClean(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	if err := os.WriteFile(filepath.Join(repo.Path, "dirty-sentinel.txt"), []byte("keep me dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty sentinel: %v", err)
	}
	before := harness.SnapshotRepo(t, "git", repo.Path)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	cfg := configWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port)
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "prompt": "write a file in the worktree", "repo": "acme/looper", "baseBranch": "main"}, &created)
	run := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success (error=%v checkpoint=%v)", run.Status, run.ErrorMessage, run.CheckpointJSON)
	}
	evidence := harness.LoadCWDEvidence(t, fakeAgent.EvidencePath())
	harness.AssertCWDInsideWorktree(t, evidence.CWD, home.WorktreeRoot)
	harness.AssertCWDNotRepoPath(t, evidence.CWD, repo.Path)
	harness.AssertCWDNotRepoPath(t, evidence.CWD, home.WorkingDir)
	after := harness.SnapshotRepo(t, "git", repo.Path)
	harness.AssertRepoUnchanged(t, before, after)
	if _, err := os.Stat(filepath.Join(repo.Path, "agent-output.txt")); !os.IsNotExist(err) {
		t.Fatalf("agent output leaked into user repo: %v", err)
	}
	requirePathExists(t, filepath.Join(evidence.CWD, "agent-output.txt"))
	loop := loadSingleLoop(t, client, created.ID)
	metadata := parseJSONObject(t, loop.MetadataJSON)
	if got, _ := metadata["worktreePath"].(string); got == "" {
		t.Fatalf("loop metadata missing worktreePath: %#v", metadata)
	}
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	if worktree, _ := checkpoint["worktree"].(map[string]any); worktree == nil || worktree["path"] == nil {
		t.Fatalf("checkpoint missing worktree path: %#v", checkpoint)
	}
	proc.Stop(context.Background())
}

func TestInvariantWorkerCommitStaysOffUserBranch(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	originPath := harness.CreateBareOrigin(t, "git", repo.Path)
	_ = originPath
	before := harness.SnapshotRepo(t, "git", repo.Path)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	vendor, command, agentEnv := fakeAgent.AgentConfig("commit", "git")
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: fakeGH.Path, Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.OpenPRStrategy = "manual"
	cfg.Defaults.AllowAutoPush = false
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "prompt": "commit from fake agent", "repo": "acme/looper", "baseBranch": "main"}, &created)
	run := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success (error=%v checkpoint=%v)", run.Status, run.ErrorMessage, run.CheckpointJSON)
	}
	after := harness.SnapshotRepo(t, "git", repo.Path)
	harness.AssertRepoUnchanged(t, before, after)
	_, repos := openRepos(t, home.DBPath)
	worktrees, err := repos.Worktrees.ListByProject(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Worktrees.List() error = %v", err)
	}
	if len(worktrees) == 0 {
		t.Fatal("expected recorded worktree")
	}
	worktreePath := worktrees[0].WorktreePath
	if worktreePath == "" {
		t.Fatalf("worktree path empty: %#v", worktrees[0])
	}
	worktreeSnapshot := harness.SnapshotRepo(t, "git", worktreePath)
	if worktreeSnapshot.Head == before.Head {
		t.Fatalf("worktree HEAD = %s, want agent-created commit distinct from user branch %s", worktreeSnapshot.Head, before.Head)
	}
	proc.Stop(context.Background())
}

func TestInvariantWorkerRejectsCheckpointWorktreePathAtUserRepo(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	before := harness.SnapshotRepo(t, "git", repo.Path)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	vendor, command, agentEnv := fakeAgent.AgentConfig("write-file", bins.LooperPath)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: fakeGH.Path, Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.OpenPRStrategy = "manual"
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops", map[string]any{
		"projectId":  "project_1",
		"type":       "worker",
		"targetType": "project",
		"targetId":   "project:project:project_1",
		"status":     "paused",
		"metadata": map[string]any{
			"worker": map[string]any{
				"title":      "Reject unsafe worktree checkpoint",
				"prompt":     "do not run in user repo",
				"repo":       "acme/looper",
				"baseBranch": "main",
			},
		},
	}, &created)
	checkpointJSON := mustMarshal(t, map[string]any{
		"resumePolicy": "advance_from_checkpoint",
		"work": map[string]any{
			"title":         "Reject unsafe worktree checkpoint",
			"prompt":        "do not run in user repo",
			"repo":          "acme/looper",
			"baseBranch":    "main",
			"executionMode": "create-pr",
		},
		"worktree": map[string]any{
			"id":         "worktree_bad",
			"path":       repo.Path,
			"branch":     "looper/bad-checkpoint",
			"baseBranch": "main",
			"headSha":    before.Head,
		},
		"plan": map[string]any{
			"summary": "Reject unsafe worktree checkpoint",
			"items":   []string{"Never use the user repo as worker cwd"},
		},
		"execution": map[string]any{
			"status":      "completed",
			"summary":     "prior execution completed",
			"parseStatus": "parsed",
		},
	})
	_, repos := openRepos(t, home.DBPath)
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_failed_bad_checkpoint", LoopID: created.ID, Status: "failed", CurrentStep: stringPtr("validate"), LastCompletedStep: stringPtr("execute"), CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	var started struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops/"+created.ID+"/start", nil, &started)
	run := waitForNewTerminalRun(t, client, created.ID, map[string]struct{}{"run_failed_bad_checkpoint": {}}, 30*time.Second)
	if run.Status != "failed" {
		t.Fatalf("run status = %s, want failed unsafe-checkpoint rejection (error=%q checkpoint=%s)", run.Status, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
	}
	if run.ErrorMessage == nil || !strings.Contains(*run.ErrorMessage, "Worker worktree path") {
		t.Fatalf("error message = %q, want stale/unsafe worktree rejection", stringValue(run.ErrorMessage))
	}
	if _, err := os.Stat(fakeAgent.EvidencePath()); !os.IsNotExist(err) {
		t.Fatalf("fake agent evidence = %v, want no agent execution", err)
	}
	after := harness.SnapshotRepo(t, "git", repo.Path)
	harness.AssertRepoUnchanged(t, before, after)
	proc.Stop(context.Background())
}

func TestInvariantReusedActiveWorkerUsesIsolatedWorktreeAndAvoidsDuplicateLoop(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	if err := os.WriteFile(filepath.Join(repo.Path, "dirty-sentinel.txt"), []byte("keep me dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty sentinel: %v", err)
	}
	before := harness.SnapshotRepo(t, "git", repo.Path)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	cfg := configWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port)
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	_, repos := openRepos(t, home.DBPath)
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	targetID := "issue:acme/looper:77"
	metadataJSON := mustMarshal(t, map[string]any{
		"worker": map[string]any{
			"title":       "Existing issue worker",
			"prompt":      "write a file in the worktree",
			"repo":        "acme/looper",
			"baseBranch":  "main",
			"issueNumber": 77,
		},
	})
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:           "loop_existing_issue_worker",
		Seq:          1,
		ProjectID:    "project_1",
		Type:         "worker",
		TargetType:   "issue",
		TargetID:     &targetID,
		Repo:         stringPtr("acme/looper"),
		Status:       "paused",
		MetadataJSON: &metadataJSON,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Reused bool   `json:"reused"`
	}
	client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "repo": "acme/looper", "issueNumber": 77, "baseBranch": "main"}, &created)
	if !created.Reused {
		t.Fatalf("reused = %v, want true", created.Reused)
	}
	if created.ID != "loop_existing_issue_worker" {
		t.Fatalf("id = %q, want existing loop id", created.ID)
	}
	if created.Status != "queued" {
		t.Fatalf("status = %q, want queued reused loop", created.Status)
	}
	run := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success (error=%v checkpoint=%v)", run.Status, run.ErrorMessage, run.CheckpointJSON)
	}
	evidence := harness.LoadCWDEvidence(t, fakeAgent.EvidencePath())
	harness.AssertCWDInsideWorktree(t, evidence.CWD, home.WorktreeRoot)
	harness.AssertCWDNotRepoPath(t, evidence.CWD, repo.Path)
	harness.AssertCWDNotRepoPath(t, evidence.CWD, home.WorkingDir)
	after := harness.SnapshotRepo(t, "git", repo.Path)
	harness.AssertRepoUnchanged(t, before, after)
	if _, err := os.Stat(filepath.Join(repo.Path, "agent-output.txt")); !os.IsNotExist(err) {
		t.Fatalf("agent output leaked into user repo: %v", err)
	}
	requirePathExists(t, filepath.Join(evidence.CWD, "agent-output.txt"))
	var loops loopsListResponse
	client.get(t, "/api/v1/loops", &loops)
	matching := 0
	for _, loop := range loops.Items {
		if loop.ID == created.ID {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("matching reused loops = %d, want 1", matching)
	}
	proc.Stop(context.Background())
}

func TestInvariantFixerUsesIsolatedWorktreeAndLeavesUserRepoClean(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	originPath := harness.CreateBareOrigin(t, "git", repo.Path)
	featureHead := harness.CreateBranchCommitAndPush(t, "git", repo.Path, "feature/fix-42", "fix-target.txt", "needs fix\n")
	before := harness.SnapshotRepo(t, "git", repo.Path)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, loadGHSchemaFixture(t))
	fakeGH.WriteState(t, harness.GHState{
		CurrentUserLogin: "looper",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {
				Number:           42,
				Repo:             "acme/looper",
				Title:            "Fix review feedback",
				Author:           "looper",
				State:            "OPEN",
				HeadRefName:      "feature/fix-42",
				BaseRefName:      "main",
				HeadRef:          "refs/heads/feature/fix-42",
				BaseRef:          "refs/heads/main",
				GitDir:           originPath,
				MergeStateStatus: "CLEAN",
				Threads: []harness.GHThread{{
					ID:         "thread-1",
					IsResolved: false,
					Path:       "fix-target.txt",
					Line:       1,
					Comments: []harness.GHThreadComment{{
						ID:                "comment-1",
						Body:              "please fix this",
						Author:            "alice",
						Path:              "fix-target.txt",
						Line:              1,
						CommitOID:         featureHead,
						OriginalCommitOID: featureHead,
						URL:               "https://example.test/thread-1",
					}},
				}},
			},
		},
	})
	cfg := fixerConfigWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port, "commit")
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops", map[string]any{"projectId": "project_1", "type": "fixer", "targetType": "pull_request", "repo": "acme/looper", "prNumber": 42}, &created)
	run := waitForRunTerminal(t, client, created.ID, 60*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success (error=%q checkpoint=%s)", run.Status, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
	}
	evidence := harness.LoadCWDEvidence(t, fakeAgent.EvidencePath())
	harness.AssertCWDInsideWorktree(t, evidence.CWD, home.WorktreeRoot)
	harness.AssertCWDNotRepoPath(t, evidence.CWD, repo.Path)
	after := harness.SnapshotRepo(t, "git", repo.Path)
	harness.AssertRepoUnchanged(t, before, after)
	if _, err := os.Stat(filepath.Join(repo.Path, "agent-commit.txt")); !os.IsNotExist(err) {
		t.Fatalf("agent commit artifact leaked into user repo: %v", err)
	}
	proc.Stop(context.Background())
}

var _ = storage.WorktreeRecord{}

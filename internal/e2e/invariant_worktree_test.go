package e2e

import (
	"context"
	"os"
	"path/filepath"
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

var _ = storage.WorktreeRecord{}

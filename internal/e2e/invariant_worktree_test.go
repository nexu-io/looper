package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/e2e/harness"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
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

func TestInvariantWorkerRecoversAfterAgentExecutableReappears(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	vendor, _, agentEnv := fakeAgent.AgentConfig("write-file", "git", fakeGH.Path)
	missingAgent := filepath.Join(home.Root, "recovered-tools", "fake-agent")
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: fakeGH.Path, Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      missingAgent,
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
	client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "prompt": "recover after the executable returns", "repo": "acme/looper", "baseBranch": "main"}, &created)
	first := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if first.Status != "failed" || first.ErrorMessage == nil || !strings.Contains(*first.ErrorMessage, "no such file or directory") {
		t.Fatalf("first run = %#v, want missing-executable failure", first)
	}
	_, repos := openRepos(t, home.DBPath)
	items, err := repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	var retry *storage.QueueItemRecord
	for i := range items {
		if items[i].LoopID != nil && *items[i].LoopID == created.ID {
			retry = &items[i]
			break
		}
	}
	if retry == nil || retry.Status != "queued" || retry.LastErrorKind == nil || *retry.LastErrorKind != "recoverable_infra" {
		t.Fatalf("queue after missing executable = %#v, want queued recoverable_infra", retry)
	}
	loop := loadSingleLoop(t, client, created.ID)
	if loop.Status != "queued" {
		t.Fatalf("loop status after recoverable fault = %q, want queued", loop.Status)
	}

	binary, err := os.ReadFile(fakeAgent.Path)
	if err != nil {
		t.Fatalf("read fake agent: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(missingAgent), 0o755); err != nil {
		t.Fatalf("mkdir recovered tool dir: %v", err)
	}
	if err := os.WriteFile(missingAgent, binary, 0o755); err != nil {
		t.Fatalf("restore fake agent: %v", err)
	}
	second := waitForNewTerminalRun(t, client, created.ID, map[string]struct{}{first.ID: {}}, 40*time.Second)
	if second.Status != "success" {
		t.Fatalf("second run status = %s, want success after infrastructure recovery (error=%q)", second.Status, stringValue(second.ErrorMessage))
	}
	proc.Stop(context.Background())
}

func TestInvariantInfraRetryBudgetBlocksWithoutRespawningAgent(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	vendor, _, agentEnv := fakeAgent.AgentConfig("write-file", "git", fakeGH.Path)
	missingAgent := filepath.Join(home.Root, "missing-tools", "fake-agent")
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: fakeGH.Path, Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      missingAgent,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.RetryBaseDelayMS = 100
	cfg.Scheduler.InfraRetryBudgetSeconds = 1
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
	client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "prompt": "do not respawn while infrastructure is absent", "repo": "acme/looper", "baseBranch": "main"}, &created)
	first := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if first.Status != "failed" {
		t.Fatalf("first run status = %s, want failed", first.Status)
	}
	_, repositories := openRepos(t, home.DBPath)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, err := repositories.Queue.List(context.Background())
		if err != nil {
			t.Fatalf("Queue.List() error = %v", err)
		}
		for i := range items {
			if items[i].LoopID == nil || *items[i].LoopID != created.ID || items[i].Status != "manual_intervention" {
				continue
			}
			if items[i].LastErrorKind == nil || *items[i].LastErrorKind != "recoverable_infra" {
				t.Fatalf("blocked queue = %#v", items[i])
			}
			loop, err := repositories.Loops.GetByID(context.Background(), created.ID)
			if err != nil || loop == nil || loop.Status != "paused" {
				t.Fatalf("blocked loop = %#v, %v", loop, err)
			}
			condition, ok := loopcondition.Read(loop.MetadataJSON)
			if !ok || condition.Kind != loopcondition.InfraRecovered {
				t.Fatalf("blocked condition = %#v, %v", condition, ok)
			}
			var runs runsListResponse
			client.get(t, "/api/v1/runs?loopId="+created.ID, &runs)
			if len(runs.Items) != 1 {
				t.Fatalf("run count = %d, want 1; cheap gate must not respawn the role/agent", len(runs.Items))
			}
			var health struct {
				Healthy   bool `json:"healthy"`
				Scheduler struct {
					Healthy      bool  `json:"healthy"`
					BlockedInfra int64 `json:"blockedInfra"`
				} `json:"scheduler"`
			}
			client.get(t, "/api/v1/healthz", &health)
			if health.Healthy || health.Scheduler.Healthy || health.Scheduler.BlockedInfra != 1 {
				t.Fatalf("health = %#v, want explicit infrastructure degradation", health)
			}
			proc.Stop(context.Background())
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	proc.Stop(context.Background())
	t.Fatal("recoverable infrastructure retry did not escalate after wall-clock budget")
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
	vendor, command, agentEnv := fakeAgent.AgentConfig("commit", "git", "")
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

func TestInvariantWorkerRecreatesUnsafeCheckpointOutsideUserRepo(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	before := harness.SnapshotRepo(t, "git", repo.Path)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	vendor, command, agentEnv := fakeAgent.AgentConfig("write-file", bins.LooperPath, "")
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
	if run.Status != "success" {
		t.Fatalf("run status = %s, want successful safe recreation (error=%q checkpoint=%s)", run.Status, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
	}
	var checkpoint struct {
		Worktree struct {
			Path string `json:"path"`
		} `json:"worktree"`
	}
	if run.CheckpointJSON == nil || json.Unmarshal([]byte(*run.CheckpointJSON), &checkpoint) != nil {
		t.Fatalf("checkpoint = %s, want valid recreated worktree checkpoint", stringValue(run.CheckpointJSON))
	}
	if checkpoint.Worktree.Path == repo.Path || !strings.HasPrefix(checkpoint.Worktree.Path, home.WorktreeRoot+string(os.PathSeparator)) {
		t.Fatalf("recreated worktree path = %q, want isolated under %q", checkpoint.Worktree.Path, home.WorktreeRoot)
	}
	if _, err := os.Stat(fakeAgent.EvidencePath()); !os.IsNotExist(err) {
		t.Fatalf("fake agent evidence = %v, want no agent execution", err)
	}
	after := harness.SnapshotRepo(t, "git", repo.Path)
	harness.AssertRepoUnchanged(t, before, after)
	proc.Stop(context.Background())
}

func TestInvariantWorkerResumeAdoptsExistingPRAndEntersShepherding(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	branch := "looper/a4-existing-pr"
	fakeGH.WriteState(t, harness.GHState{Routes: map[string]any{
		"repos/acme/looper/pulls": json.RawMessage(`[{"number":201,"title":"Existing PR","html_url":"https://example.test/acme/looper/pull/201","state":"open","head":{"ref":"looper/a4-existing-pr"},"base":{"ref":"main"}}]`),
	}})
	cfg := configWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port)
	cfg.Defaults.OpenPRStrategy = "all_done"
	cfg.Defaults.AllowAutoPush = true
	cfg.Defaults.WorkerShepherd = true
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
		"metadata": map[string]any{"worker": map[string]any{
			"title":         "Adopt existing PR after interruption",
			"repo":          "acme/looper",
			"baseBranch":    "main",
			"branch":        branch,
			"executionMode": "create-pr",
		}},
	}, &created)
	checkpointJSON := mustMarshal(t, map[string]any{
		"resumePolicy": "advance_from_checkpoint",
		"work": map[string]any{
			"title":         "Adopt existing PR after interruption",
			"repo":          "acme/looper",
			"baseBranch":    "main",
			"branch":        branch,
			"executionMode": "create-pr",
		},
		"worktree": map[string]any{
			"id":         "worktree_deleted",
			"path":       filepath.Join(home.WorktreeRoot, "deleted-a4-worktree"),
			"branch":     branch,
			"baseBranch": "main",
			"headSha":    repo.InitialCommit,
		},
		"execution":  map[string]any{"status": "completed", "summary": "implemented", "parseStatus": "parsed"},
		"validation": map[string]any{"passed": true, "summary": "passed"},
	})
	_, repos := openRepos(t, home.DBPath)
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_interrupted_before_pr_persist", LoopID: created.ID, Status: "failed", CurrentStep: stringPtr("open-pr"), LastCompletedStep: stringPtr("validate"), CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	var started struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops/"+created.ID+"/start", nil, &started)
	run := waitForNewTerminalRun(t, client, created.ID, map[string]struct{}{"run_interrupted_before_pr_persist": {}}, 30*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want successful PR adoption (error=%q checkpoint=%s)", run.Status, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
	}
	loop, err := repos.Loops.GetByID(context.Background(), created.ID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}
	if loop.Status != "shepherding" || loop.PRNumber == nil || *loop.PRNumber != 201 {
		t.Fatalf("loop = %#v, want PR 201 in shepherding", loop)
	}
	if _, err := os.Stat(fakeAgent.EvidencePath()); !os.IsNotExist(err) {
		t.Fatalf("fake agent evidence = %v, want no agent execution during PR adoption", err)
	}
	invocations := readInvocationLog(t, fakeGH.InvocationLog)
	assertInvocationContainsOrdered(t, invocations, []string{"api", "--method", "GET", "repos/acme/looper/pulls"})
	assertNoInvocationStartsWith(t, invocations, []string{"pr", "create"})
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

package collab

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// Exercise the real worker, executor, SQLite and git worktree through the
// independent control socket. Only the model is a local deterministic process;
// no daemon, Friday, network registration or external messaging is involved.
func TestStandaloneHostProcessesTwoTasksWithoutRestart(t *testing.T) {
	t.Parallel()
	cfg, dir := hostFixture(t, "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"native-test-session\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"__LOOPER_RESULT__={\\\"summary\\\":\\\"Verified local fixture\\\"}\"}}'\n")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Options{Config: cfg, StateDir: dir, NetworkStatePath: filepath.Join(dir, "network.json")})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("host did not drain")
		}
	}()
	request := func(method, path string, input any) json.RawMessage {
		t.Helper()
		data, err := Request(ctx, dir, method, path, input)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for {
		if _, err := Request(ctx, dir, "GET", "/status", nil); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("host never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := Serve(ctx, Options{Config: cfg, StateDir: dir, NetworkStatePath: filepath.Join(dir, "network.json")}); err == nil {
		t.Fatal("second host acquired the same state directory")
	}
	if _, err := Request(ctx, dir, "POST", "/run", worker.EnqueueInput{ProjectID: "local", Repo: "acme/wrong", Prompt: "test"}); err == nil {
		t.Fatal("wrong repo was accepted")
	}
	for _, title := range []string{"first task", "second task"} {
		var created storage.LoopRecord
		if err := json.Unmarshal(request("POST", "/run", worker.EnqueueInput{ProjectID: "local", Repo: "acme/local", Title: title, Prompt: "Verify fixture; do not modify anything"}), &created); err != nil {
			t.Fatal(err)
		}
		for {
			var status struct {
				Tasks []storage.LoopRecord `json:"tasks"`
			}
			if err := json.Unmarshal(request("GET", "/status", nil), &status); err != nil {
				t.Fatal(err)
			}
			finished := false
			for _, item := range status.Tasks {
				if item.ID == created.ID {
					if item.Status == "failed" {
						t.Fatalf("worker task failed: %+v", item)
					}
					finished = item.Status == "completed"
				}
			}
			if finished {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("task never completed")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	request("POST", "/stop", nil)
}

func TestStopDuringExecutionFinalizesClaimBeforeReleasingHost(t *testing.T) {
	t.Parallel()
	// Readiness must follow signal-handler setup, without a subsequent exec
	// transition racing the host's group signal on macOS.
	cfg, dir := hostFixture(t, "#!/bin/sh\ntrap 'exit 0' TERM INT\nprintf active > \"$COLLAB_TEST_STARTED\"\nwhile :; do sleep 1; done\n")
	started := filepath.Join(dir, "model-started")
	cfg.Agent.Env = map[string]string{"COLLAB_TEST_STARTED": started}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Options{Config: cfg, StateDir: dir, NetworkStatePath: filepath.Join(dir, "network.json")})
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("host failed to stop")
			}
		}
	}()
	for {
		if _, err := Request(ctx, dir, "GET", "/status", nil); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("host never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := Request(ctx, dir, "POST", "/run", worker.EnqueueInput{ProjectID: "local", Repo: "acme/local", Prompt: "Wait for host stop"}); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("agent subprocess never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := Request(ctx, dir, "POST", "/stop", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("host did not drain its active subprocess")
	}
	db, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(dir, "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, err := storage.NewRepositories(db.DB()).Queue.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status == "running" || items[0].Status == "completed" {
		t.Fatalf("stop must durably release the claim without claiming delivery: %+v", items)
	}
}

func hostFixture(t *testing.T, script string) (config.Config, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "lc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Cleanup(func() {
		if t.Failed() {
			db, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(dir, "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
			if err == nil {
				defer db.Close()
				repos := storage.NewRepositories(db.DB())
				runs, _ := repos.Runs.List(context.Background())
				for _, run := range runs {
					b, _ := json.Marshal(run)
					t.Log(string(b))
				}
			}
		}
	})
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	git("init", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	fake := filepath.Join(dir, "model.sh")
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	vendor := config.AgentVendor("codex")
	cfg.Agent.Vendor = &vendor
	cfg.Agent.Params = map[string]any{"command": fake}
	cfg.Agent.NativeResume.Enabled = true
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyManual
	cfg.Defaults.AllowAutoPush = false
	cfg.Notifications.Osascript.Enabled = false
	cfg.Notifications.Webhook.Enabled = false
	root := filepath.Join(dir, "worktrees")
	base := "main"
	cfg.Projects = []config.ProjectRefConfig{{ID: "local", Name: "local", Repo: "acme/local", RepoPath: repo, BaseBranch: &base, WorktreeRoot: &root}}

	return cfg, dir
}

package workerhost

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestShepherdWakesChangedCIWithoutPassCapAndCompletesOnlyOnMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "state.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatal(err)
	}
	repos := storage.NewRepositories(db.DB())
	now := time.Now().UTC()
	stamp := now.Format("2006-01-02T15:04:05.000Z")
	base, repo, target, number := "main", "acme/code", "pr:acme/code:7", int64(7)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project", Name: "test", RepoPath: t.TempDir(), BaseBranch: &base, CreatedAt: stamp, UpdatedAt: stamp}); err != nil {
		t.Fatal(err)
	}
	metadata, _ := loops.WriteShepherd(nil, loops.Shepherd{Active: true, PassCount: 30})
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "task", Seq: 1, ProjectID: "project", Type: "worker", TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &number, Status: "shepherding", MetadataJSON: &metadata, CreatedAt: stamp, UpdatedAt: stamp}); err != nil {
		t.Fatal(err)
	}
	state, check := "OPEN", "FAILURE"
	gh := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, opts shell.Options) (shell.Result, error) {
		args := strings.Join(opts.Args, " ")
		switch {
		case strings.HasPrefix(args, "pr view"):
			data, _ := json.Marshal(map[string]any{"number": 7, "state": state, "headRefOid": "head", "baseRefOid": "base", "reviewDecision": "APPROVED", "reviews": []any{map[string]any{"state": "APPROVED", "commit": map[string]any{"oid": "head"}}}, "statusCheckRollup": []any{map[string]any{"status": "COMPLETED", "conclusion": check}}, "mergeStateStatus": "CLEAN"})
			return shell.Result{Stdout: string(data)}, nil
		case strings.HasPrefix(args, "api graphql"):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`}, nil
		case strings.HasPrefix(args, "api --paginate"):
			return shell.Result{Stdout: "[]"}, nil
		default:
			t.Fatalf("unexpected GitHub command: %s", args)
			return shell.Result{}, nil
		}
	}})
	cfg, _ := config.DefaultConfig(t.TempDir())
	host := Shepherd{Config: cfg, Repos: repos, GitHub: gh}
	host.Reconcile(ctx)
	queue, err := repos.Queue.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].Status != "queued" {
		t.Fatalf("changed failing CI must wake even after 30 passes: %+v", queue)
	}
	host.Reconcile(ctx)
	again, _ := repos.Queue.List(ctx)
	if len(again) != 1 {
		t.Fatal("unchanged signal enqueued another pass")
	}
	// Emulate the existing worker consuming this pass; the observer must rest
	// after approval/green CI, keeping the task live until a human merges.
	if _, err := repos.Queue.CancelByLoop(ctx, "task", stamp, nil); err != nil {
		t.Fatal(err)
	}
	check = "SUCCESS"
	host.Reconcile(ctx)
	item, _ := repos.Loops.GetByID(ctx, "task")
	marker, _ := loops.ReadShepherd(item.MetadataJSON)
	if item.Status != "shepherding" || !marker.Active || marker.Phase != "awaiting_merge" {
		t.Fatalf("green CI prematurely ended task: %+v %+v", item, marker)
	}
	state = "MERGED"
	host.Reconcile(ctx)
	item, _ = repos.Loops.GetByID(ctx, "task")
	marker, _ = loops.ReadShepherd(item.MetadataJSON)
	if item.Status != "completed" || marker.Active || marker.Outcome != "merged" {
		t.Fatalf("merge was not finalized: %+v %+v", item, marker)
	}
}

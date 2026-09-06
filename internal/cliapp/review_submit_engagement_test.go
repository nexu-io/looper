package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/nexu-io/looper/internal/forge"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// Exercise the trusted CLI boundary with real SQLite identity checks and the
// actual gh adapter. No agent payload or review-submit flag can invent ownership.
func TestTrustedReviewSubmitRecoversMissingEngagement(t *testing.T) {
	for _, tc := range []struct {
		name, runID, author, markerID, markerHead, commit, state, outcome string
		want                                                              bool
	}{
		{"explicit run", "run", "bob", "reviewer:loop", "old", "old", "CHANGES_REQUESTED", "blocking", true},
		{"implicit current run", "", "bob", "reviewer:loop:legacy", "old", "old", "CHANGES_REQUESTED", "blocking", true},
		{"wrong run", "missing", "bob", "reviewer:loop", "old", "old", "CHANGES_REQUESTED", "blocking", false},
		{"other author", "run", "alice", "reviewer:loop", "old", "old", "CHANGES_REQUESTED", "blocking", false},
		{"other loop", "run", "bob", "reviewer:loop-other", "old", "old", "CHANGES_REQUESTED", "blocking", false},
		{"commit mismatch", "run", "bob", "reviewer:loop", "old", "different", "CHANGES_REQUESTED", "blocking", false},
		{"same head", "run", "bob", "reviewer:loop", "new", "new", "CHANGES_REQUESTED", "blocking", false},
		{"unsubmitted", "run", "bob", "reviewer:loop", "old", "old", "PENDING", "blocking", false},
		{"rejected event", "run", "bob", "reviewer:loop", "old", "old", "COMMENTED", "blocking", false},
		{"noncanonical outcome", "run", "bob", "reviewer:loop", "old", "old", "CHANGES_REQUESTED", "BLOCKING", false},
		{"human review", "run", "bob", "human", "old", "old", "CHANGES_REQUESTED", "blocking", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root := t.TempDir()
			cfg, err := config.DefaultConfig(root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Storage.DBPath = filepath.Join(root, "looper.sqlite")
			coordinator, err := storage.OpenSQLiteCoordinator(ctx, cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })
			if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
				t.Fatal(err)
			}
			repos := storage.NewRepositories(coordinator.DB())
			now, repo := "2026-09-06T00:00:00.000Z", "acme/looper"
			pr := int64(42)
			meta := `{"followUpdates":true,"loop":{"enabled":true},"reviewEvents":{"clean":"COMMENT","blocking":"REQUEST_CHANGES"}}`
			if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project", Name: "Project", RepoPath: root, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop", Seq: 1, ProjectID: "project", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running", MetadataJSON: &meta, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run", LoopID: "loop", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			body := "<!-- looper:review id=" + tc.markerID + " head=" + tc.markerHead + " outcome=" + tc.outcome + " -->"
			detail := map[string]any{"number": 42, "state": "OPEN", "headRefOid": "new", "author": map[string]any{"login": "alice"}, "reviews": []any{map[string]any{"author": map[string]any{"login": tc.author}, "state": tc.state, "body": body, "commit": map[string]any{"oid": tc.commit}}}}
			data, _ := json.Marshal(detail)
			if err := os.WriteFile(filepath.Join(root, "detail.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			ghPath := filepath.Join(root, "gh")
			script := `#!/bin/sh
set -eu
case "$1 $2" in
 "pr view") cat "$(dirname "$0")/detail.json" ;;
 "api user") printf 'bob\n' ;;
 "api graphql") printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}' ;;
 *) printf '[]' ;;
esac
`
			if err := os.WriteFile(ghPath, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			cfg.Tools.GHPath = &ghPath
			runtime := newCommandRuntime(New(Deps{Getwd: func() (string, error) { return root, nil }}), nil)
			cmd := newReviewSubmitTestCommand(&bytes.Buffer{}, &bytes.Buffer{})
			cmd.SetContext(ctx)
			if err := cmd.Flags().Set("reviewer-run-id", tc.runID); err != nil {
				t.Fatal(err)
			}
			got, err := runtime.trustedReviewRequestSubmitBypass(cmd, cfg, repo, pr, "new")
			if err != nil || got != tc.want {
				t.Fatalf("bypass = %v, %v; want %v", got, err, tc.want)
			}
			// CLI recovery is read-only; daemon remains the metadata writer.
			loop, err := repos.Loops.GetByID(ctx, "loop")
			if err != nil || loop == nil || strings.Contains(*loop.MetadataJSON, "lastPublishedHeadSha") {
				t.Fatalf("CLI changed loop: %#v, %v", loop, err)
			}
		})
	}
}

func TestForgejoReviewSubmitRecoversNativeEngagement(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`))
		case "/api/v1/user":
			_, _ = w.Write([]byte(`{"login":"bob"}`))
		case "/api/v1/repos/acme/looper/pulls/42":
			_, _ = w.Write([]byte(`{"number":42,"state":"open","user":{"login":"alice"},"head":{"sha":"new"}}`))
		case "/api/v1/repos/acme/looper/pulls/42/reviews/1/comments":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v1/repos/acme/looper/pulls/42/reviews":
			_, _ = w.Write([]byte(`[{"id":1,"user":{"login":"bob"},"state":"REQUEST_CHANGES","commit_id":"old","body":"<!-- looper:review id=reviewer:loop head=old outcome=blocking -->"}]`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo", Kind: forge.ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "token")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.DBPath = filepath.Join(t.TempDir(), "absent.sqlite")
	meta := `{"followUpdates":true,"loop":{"enabled":true},"reviewEvents":{"blocking":"REQUEST_CHANGES"}}`
	loop := storage.LoopRecord{ID: "loop", ProjectID: "project", MetadataJSON: &meta}
	got, err := recoverReviewSubmitEngagement(context.Background(), forgejoReviewSubmitGateway{client: client}, cfg, loop, "acme/looper", 42, "new", t.TempDir())
	if err != nil || got != "old" {
		t.Fatalf("recovery = %q, %v", got, err)
	}
}

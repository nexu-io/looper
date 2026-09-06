package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHandlerCreateForgejoManualLoopsUsesProviderAndPreservesHolds(t *testing.T) {
	for _, tc := range []struct {
		name           string
		role           string
		configured     bool
		held           bool
		force          bool
		missingRepo    bool
		missingGH      bool
		issue          bool
		providerStatus int
	}{
		{name: "configured reviewer", role: "reviewer", configured: true},
		{name: "configured fixer", role: "fixer", configured: true},
		{name: "registered reviewer without gh", role: "reviewer", missingGH: true},
		{name: "registered fixer without checkout", role: "fixer", missingRepo: true},
		{name: "reviewer hold", role: "reviewer", configured: true, held: true},
		{name: "fixer hold", role: "fixer", held: true},
		{name: "held reviewer without checkout or gh", role: "reviewer", held: true, missingRepo: true, missingGH: true},
		{name: "force reviewer bypasses hold", role: "reviewer", held: true, force: true},
		{name: "force fixer bypasses hold", role: "fixer", configured: true, held: true, force: true},
		{name: "provider failure does not enqueue", role: "fixer", providerStatus: http.StatusForbidden},
		{name: "planner issue hold", role: "planner", held: true, issue: true},
		{name: "worker issue hold", role: "worker", held: true, issue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTestFixture(t)
			var reads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/api/v1/repos/acme/looper/pulls/42"
				if tc.issue {
					wantPath = "/api/v1/repos/acme/looper/issues/77"
				}
				if r.Method != http.MethodGet || r.URL.Path != wantPath {
					t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				reads.Add(1)
				if r.Header.Get("Authorization") != "token manual-loop-token" {
					t.Errorf("provider request did not use its configured authentication")
				}
				if tc.providerStatus != 0 {
					w.WriteHeader(tc.providerStatus)
					_, _ = w.Write([]byte(`{"message":"permission denied"}`))
					return
				}
				labels := []map[string]string{}
				if tc.held {
					labels = append(labels, map[string]string{"name": domain.HoldLabelGlobal})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "labels": labels})
			}))
			defer server.Close()
			t.Setenv("LOOPER_TEST_MANUAL_FORGEJO_TOKEN", "manual-loop-token")
			fixture.config.Providers = []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("LOOPER_TEST_MANUAL_FORGEJO_TOKEN")}}
			// Any accidental gh dispatch must fail; Forgejo should use only HTTP.
			fixture.config.Tools.GHPath = stringPtr(filepath.Join(fixture.rootDir, "gh-must-not-run"))
			if tc.missingGH {
				fixture.config.Tools.GHPath = nil
			}
			repoPath := fixture.rootDir
			if tc.missingRepo {
				repoPath = filepath.Join(fixture.rootDir, "not-checked-out")
			}
			metadata := `{"repo":"acme/looper","provider":"forgejo","source":"api"}`
			if tc.configured {
				fixture.config.Projects = []config.ProjectRefConfig{{ID: "project_1", Repo: "acme/looper", RepoPath: repoPath, Provider: "forgejo"}}
				// The coherent runtime catalog binding takes precedence over a stale
				// record snapshot supplied by compatibility callers.
				metadata = `{"repo":"acme/looper","provider":"stale-provider"}`
			}
			now := fixture.now.UTC().Format(javaScriptISOString)
			if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			// Supply the binding in the current runtime snapshot, not the handler's
			// startup config, to cover dynamically registered / reloaded providers.
			startupConfig := fixture.config
			startupConfig.Providers = nil
			startupConfig.Projects = nil
			h := NewHandler(Context{Config: startupConfig, Runtime: runtimeWithConfig(fixture.runtime, fixture.config), Now: func() time.Time { return fixture.now.Add(time.Minute) }})
			body := fmt.Sprintf(`{"projectId":"project_1","type":%q,"targetType":"pull_request","repo":"acme/looper","prNumber":42,"force":%t}`, tc.role, tc.force)
			path := "/api/v1/loops"
			if tc.issue {
				path = "/api/v1/" + tc.role + "s"
				body = `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("content-type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			wantStatus, wantQueue := http.StatusOK, 1
			if (tc.held && !tc.force) || tc.providerStatus != 0 {
				wantStatus, wantQueue = http.StatusBadRequest, 0
			}
			if rec.Code != wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
			}
			if tc.held && !tc.force && !strings.Contains(rec.Body.String(), "--force") {
				t.Fatalf("held response omitted force guidance: %s", rec.Body.String())
			}
			wantReads := int64(1)
			if tc.force {
				wantReads = 0
			}
			if got := reads.Load(); got != wantReads {
				t.Fatalf("provider reads = %d, want %d", got, wantReads)
			}
			queued, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
			if err != nil || len(queued) != wantQueue {
				t.Fatalf("queue = %#v, %v; want %d items", queued, err, wantQueue)
			}
		})
	}
}

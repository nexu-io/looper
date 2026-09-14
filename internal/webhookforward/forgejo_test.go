package webhookforward

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

// Payload shapes and exact event names are pinned to Forgejo v15.0.8/v16.0.4:
// services/webhook/notifier.go and modules/structs/{hook,action}.go.
func TestForgejoNativeEventRouting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event, payload, object string
		number                 int64
		lanes                  []string
	}{
		{"issues", `{"action":"opened","issue":{"number":7}}`, "issues", 7, []string{"planner", "worker"}},
		{"issue_label", `{"action":"label_updated","issue":{"number":7,"labels":[{"name":"looper:plan"}]}}`, "issues", 7, []string{"planner", "worker"}},
		{"issue_assign", `{"action":"assigned","issue":{"number":7,"assignees":[{"login":"bot"}]}}`, "issues", 7, []string{"planner", "worker"}},
		{"pull_request_sync", `{"action":"synchronized","pull_request":{"number":8}}`, "pull_request", 8, []string{"fixer", "reviewer"}},
		{"pull_request_label", `{"action":"label_cleared","pull_request":{"number":8}}`, "pull_request", 8, []string{"fixer", "reviewer"}},
		{"pull_request_review_request", `{"action":"review_requested","pull_request":{"number":8}}`, "pull_request", 8, []string{"fixer", "reviewer"}},
		{"pull_request_comment", `{"action":"created","issue":{"number":8},"pull_request":{"number":8},"is_pull":true,"comment":{"id":31,"body":"fix this"}}`, "pull_request", 8, []string{"fixer", "reviewer"}},
		{"pull_request_review_comment", `{"action":"reviewed","pull_request":{"number":8},"review":{"type":"pull_request_review_comment","content":"fix this"}}`, "pull_request", 8, []string{"fixer", "reviewer"}},
		{"pull_request_review_rejected", `{"action":"reviewed","pull_request":{"number":8},"review":{"type":"pull_request_review_rejected","content":"fix this"}}`, "pull_request", 8, []string{"fixer", "reviewer"}},
		{"pull_request_review_approved", `{"action":"reviewed","pull_request":{"number":8}}`, "pull_request", 8, []string{"fixer", "reviewer"}},
		{"issue_comment", `{"action":"created","issue":{"number":8},"is_pull":true}`, "pull_request", 8, []string{"fixer", "reviewer"}},
		{"push", `{"ref":"refs/heads/main","after":"abc"}`, "base_branch", 0, []string{"fixer"}},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			payload := []byte(tc.payload[:len(tc.payload)-1] + `,"repository":{"full_name":"acme/app"}}`)
			got, ok, err := routeForgejoDelivery(tc.event, payload)
			if err != nil || !ok || got.repo != "acme/app" || got.objectType != tc.object || !reflect.DeepEqual(sortedLaneStrings(got.lanes), tc.lanes) {
				t.Fatalf("route = %#v, %t, %v", got, ok, err)
			}
			if tc.number > 0 && !reflect.DeepEqual(got.numbers, []int64{tc.number}) {
				t.Fatalf("numbers = %v", got.numbers)
			}
		})
	}
	for _, event := range []string{"action_run_success", "action_run_recover", "action_run_failure"} {
		got, ok, err := routeForgejoDelivery(event, []byte(`{"action":"success","run":{"repository":{"full_name":"acme/app"},"commit_sha":"abc","prettyref":"#8","event_payload":"{}"}}`))
		if err != nil || !ok || got.repo != "acme/app" || got.objectType != "repository" {
			t.Fatalf("Actions route = %#v, %t, %v", got, ok, err)
		}
		got, ok, err = routeForgejoDelivery(event, []byte(`{"run":{"repository":{"full_name":"acme/app"},"prettyref":"#999","event_payload":"{\"pull_request\":{\"number\":8}}"}}`))
		if err != nil || !ok || got.objectType != "pull_request" || !reflect.DeepEqual(got.numbers, []int64{8}) {
			t.Fatalf("Actions PR trigger route = %#v, %t, %v", got, ok, err)
		}
	}
	for _, payload := range []string{`{"ref":"refs/tags/v1","repository":{"full_name":"acme/app"}}`, `{"ref":"refs/heads/old","after":"000000","repository":{"full_name":"acme/app"}}`} {
		if _, ok, err := routeForgejoDelivery("push", []byte(payload)); ok || err != nil {
			t.Fatalf("deleted/tag push = %t, %v", ok, err)
		}
	}
}

func TestForgejoForwardIsolationDedupeAndEligibility(t *testing.T) {
	repos := newTestRepositories(t)
	cfg := testConfig(t)
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Roles.Planner.AutoDiscovery, cfg.Roles.Worker.AutoDiscovery = true, true
	for _, id := range []string{"one", "two", "archived"} {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{ID: id, Kind: config.ProviderKindForgejo, BaseURL: "https://" + id + ".example"})
		cfg.Projects = append(cfg.Projects, config.ProjectRefConfig{ID: id, Provider: id, Repo: "acme/app"})
		seedProject(t, repos, id, "acme/app")
	}
	seedProject(t, repos, "github", "acme/app")
	cfg.Projects = append(cfg.Projects, config.ProjectRefConfig{ID: "github", Repo: "acme/app"})
	archived, _ := repos.Projects.GetByID(context.Background(), "archived")
	archived.Archived = true
	if err := repos.Projects.Upsert(context.Background(), *archived); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := map[string][]Lane{}
	f := New(Options{Repos: repos, Config: cfg, DiscoverProject: func(_ context.Context, request ProjectDiscovery) error {
		mu.Lock()
		defer mu.Unlock()
		calls[request.ProjectID] = append(calls[request.ProjectID], request.Lanes...)
		return nil
	}})
	defer f.Close()
	for _, id := range []string{"one", "two", "archived"} {
		project, _ := configuredProjectByID(cfg, id)
		identity, _ := config.ProjectRepositoryIdentity(cfg, project)
		request := DeliveryRequest{Repository: &identity, DeliveryID: "same-delivery-id", EventType: "issue_label", Payload: []byte(`{"action":"label_updated","issue":{"number":7},"repository":{"full_name":"acme/app"}}`)}
		result, err := f.Forward(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if id == "archived" {
			want = 0
		}
		if result.WorkItems != want {
			t.Fatalf("%s = %#v", id, result)
		}
		duplicate, err := f.Forward(context.Background(), request)
		if err != nil || duplicate.Status != "duplicate" {
			t.Fatalf("duplicate = %#v, %v", duplicate, err)
		}
	}
	waitForgejoForwarder(t, f, 2)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !reflect.DeepEqual(calls["one"], []Lane{LanePlanner, LaneWorker}) || !reflect.DeepEqual(calls["two"], []Lane{LanePlanner, LaneWorker}) {
		t.Fatalf("cross-instance calls = %#v", calls)
	}
}

func waitForgejoForwarder(t *testing.T, f Forwarder, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stats := f.Stats()
		if stats.ExecutionsFailed > 0 {
			t.Fatalf("discovery failed: %#v", stats.RecentOutcomes)
		}
		if stats.ExecutionsSucceeded >= want && stats.InFlight == 0 && stats.Queued == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("discovery did not finish: %#v", f.Stats()))
}

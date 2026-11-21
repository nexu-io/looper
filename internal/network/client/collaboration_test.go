package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/network/protocol"
)

func TestCollaborationManagerUsesNodeAuthAndExcludesConcurrentHosts(t *testing.T) {
	t.Parallel()
	heartbeats := make(chan protocol.HeartbeatRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-node-token" {
			t.Error("existing node credential was not used")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/heartbeat" {
			var hb protocol.HeartbeatRequest
			if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
				t.Error(err)
			}
			heartbeats <- hb
			_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{RecordedAt: time.Now()})
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.NodeStatusResponse{})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "network.json")
	if err := SaveState(path, LocalState{URL: server.URL, NodeID: "fixture-node", NodeName: "collab", NodeToken: "fixture-node-token"}); err != nil {
		t.Fatal(err)
	}
	workspace, planeProject := "fixture", "plane-project"
	cfg := config.Config{Providers: []config.ProviderConfig{{ID: "plane", Kind: config.ProviderKindPlane, Workspace: &workspace, ProjectID: &planeProject, StrictDispatch: &config.PlaneStrictDispatchConfig{Enabled: true}}}, Projects: []config.ProjectRefConfig{{ID: "local", Provider: "plane", Repo: "acme/code", Network: config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}}}}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Reviewer.Discovery.AutoDiscovery = true
	manager := NewCollaborationManager(path, cfg, nil, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()
	select {
	case hb := <-heartbeats:
		caps := hb.Capabilities
		if !caps.StrictDispatchV1 || !caps.CollaborationV1 || caps.CoordinatorEligible || len(caps.ReviewerProjects) != 0 || len(caps.Roles) != 1 || caps.Roles[0] != "worker" {
			t.Fatalf("advertised executors that collab does not run: %+v", caps)
		}
		if len(caps.CollaborationProjects) != 1 || caps.CollaborationProjects[0].Repo != "acme/code" || caps.CollaborationProjects[0].PlaneProjectID != planeProject {
			t.Fatalf("explicit code repository was not published: %+v", caps.CollaborationProjects)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("existing heartbeat transport did not run")
	}
	other := NewManager(path, cfg, nil, nil)
	if err := other.Start(context.Background()); err == nil {
		other.Stop()
		t.Fatal("two hosts were allowed to overwrite the same node identity")
	}
	manager.Stop()
	if err := other.Start(context.Background()); err != nil {
		t.Fatalf("stopped host did not release device connection: %v", err)
	}
	other.Stop()
}

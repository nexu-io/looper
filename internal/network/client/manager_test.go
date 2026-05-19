package client

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/network/cloud"
	"github.com/nexu-io/looper/internal/network/protocol"
)

func TestManagerStartWithoutNetworkStateLeavesStatusUnconfigured(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "missing-network.json"), config.Config{}, nil, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer manager.Stop()
	if manager.Status().Configured {
		t.Fatal("Status().Configured = true, want false")
	}
}

func TestManagerReportsIdentityDriftAndRemoteReachability(t *testing.T) {
	ctx := context.Background()
	service, err := cloud.Open(ctx, cloud.Config{DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion, MinimumDaemonVersion: "0.0.0"})
	if err != nil {
		t.Fatalf("cloud.Open() error = %v", err)
	}
	defer service.Close()
	server := cloud.NewServer(cloud.Config{AdminToken: "admin-token"}, service)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	joinKey, err := service.CreateJoinKey(ctx)
	if err != nil {
		t.Fatalf("CreateJoinKey() error = %v", err)
	}
	joinResp, err := New(httpServer.URL, "", httpServer.Client()).Join(ctx, protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "0.0.0", JoinKey: joinKey, NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "stored-user"}})
	if err != nil {
		t.Fatalf("Join() error = %v", err)
	}
	statePath := filepath.Join(t.TempDir(), "network.json")
	if err := SaveState(statePath, LocalState{URL: httpServer.URL, NetworkID: joinResp.NetworkID, NodeID: joinResp.NodeID, NodeName: "worker-1", NodeToken: joinResp.NodeToken, GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "stored-user"}}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	gh := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: `{"login":"current-user","id":202}`}, nil
	}})
	cfg := config.Config{Projects: []config.ProjectRefConfig{{ID: "demo-local", Network: nil}, {ID: "demo-routed", Network: &config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}}}, Roles: config.RoleConfigs{Worker: config.WorkerRoleConfig{AutoDiscovery: true}, Reviewer: config.ReviewerRoleConfig{Discovery: config.ReviewerRoleDiscoveryConfig{AutoDiscovery: true}}}}
	manager := NewManager(statePath, cfg, nil, gh)
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer manager.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := manager.Status()
		if status.CloudReachable {
			if !status.IdentityDrift {
				t.Fatal("Status().IdentityDrift = false, want true")
			}
			if status.RoutedProjects != 1 || status.LocalProjects != 1 {
				t.Fatalf("project counts = %d/%d, want 1/1", status.RoutedProjects, status.LocalProjects)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("manager status did not become cloud-reachable")
}

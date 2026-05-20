package client

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync"
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

func TestManagerStartWithoutRoutedProjectsPreservesProjectCounts(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "network.json")
	if err := SaveState(statePath, LocalState{NetworkID: "net-1", NodeID: "node-1", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "stored-user"}}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	manager := NewManager(statePath, config.Config{Projects: []config.ProjectRefConfig{{ID: "demo-local-1"}, {ID: "demo-local-2"}}}, nil, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer manager.Stop()

	status := manager.Status()
	if got, want := status.RoutedProjects, 0; got != want {
		t.Fatalf("Status().RoutedProjects = %d, want %d", got, want)
	}
	if got, want := status.LocalProjects, 2; got != want {
		t.Fatalf("Status().LocalProjects = %d, want %d", got, want)
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
	cfg := config.Config{Projects: []config.ProjectRefConfig{{ID: "demo-local"}, {ID: "demo-routed", Network: config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}}}, Roles: config.RoleConfigs{Worker: config.WorkerRoleConfig{AutoDiscovery: true}, Reviewer: config.ReviewerRoleConfig{Discovery: config.ReviewerRoleDiscoveryConfig{AutoDiscovery: true}}}}
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

func TestManagerClearsIdentityDriftAfterIdentityMatches(t *testing.T) {
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

	var mu sync.Mutex
	identity := protocol.GitHubIdentity{NumericID: 202, Login: "current-user"}
	gh := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		return shell.Result{Stdout: fmt.Sprintf(`{"login":%q,"id":%d}`, identity.Login, identity.NumericID)}, nil
	}})
	manager := NewManager(statePath, config.Config{Projects: []config.ProjectRefConfig{{ID: "demo-routed", Network: config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}}}}, nil, gh)
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer manager.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Status().IdentityDrift {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !manager.Status().IdentityDrift {
		t.Fatal("Status().IdentityDrift = false, want true before identity recovers")
	}

	mu.Lock()
	identity = protocol.GitHubIdentity{NumericID: 101, Login: "stored-user"}
	mu.Unlock()

	deadline = time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		status := manager.Status()
		if status.CloudReachable && !status.IdentityDrift && status.DriftReason == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	status := manager.Status()
	t.Fatalf("final drift status = %v reason=%q, want cleared", status.IdentityDrift, status.DriftReason)
}

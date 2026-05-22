package cliapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
)

func TestNetworkMembersListsJoinedNodesAsJSON(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	now := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1/status"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer node-token"; got != want {
			t.Fatalf("Authorization = %q, want %q", got, want)
		}
		response := protocol.NodeStatusResponse{
			NetworkID: "net-1",
			Membership: protocol.Membership{
				NodeID:   "node-1",
				NodeName: "worker-1",
				GitHub:   protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
			},
			Memberships: []protocol.Membership{
				{
					NodeID:   "node-1",
					NodeName: "worker-1",
					GitHub:   protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
					Capabilities: protocol.NodeCapabilities{
						Roles:               []string{"worker", "reviewer"},
						CoordinatorEligible: true,
						RoutedProjects:      1,
						LocalProjects:       0,
						DynamicLoad:         2,
					},
					TargetLabels:    []string{"looper:target:worker-1"},
					JoinedAt:        now.Add(-2 * time.Hour),
					LastHeartbeatAt: ptrTime(now),
				},
				{
					NodeID:   "node-2",
					NodeName: "worker-2",
					GitHub:   protocol.GitHubIdentity{Login: "alice", NumericID: 42},
					Capabilities: protocol.NodeCapabilities{
						Roles:          []string{"reviewer"},
						RoutedProjects: 0,
						LocalProjects:  2,
					},
					JoinedAt: now.Add(-1 * time.Hour),
				},
			},
			Lease:    protocol.CoordinatorLease{HolderNodeID: "node-2"},
			Warnings: []string{"duplicate GitHub identity detected elsewhere"},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	if err := client.SaveState(filepath.Join(homeDir, ".looper", "network.json"), client.LocalState{
		URL:       server.URL,
		NetworkID: "net-1",
		NodeID:    "node-1",
		NodeName:  "worker-1",
		NodeToken: "node-token",
		GitHub:    protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
	}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	app := New(Deps{HomeDir: homeDir})
	exitCode, stdout, stderr := runAppWithDeps(t, app, []string{"network", "members", "--json"})
	if exitCode != 0 {
		t.Fatalf("Run(network members --json) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	var payload struct {
		NetworkID         string                 `json:"networkId"`
		CurrentNodeID     string                 `json:"currentNodeId"`
		LeaseHolderNodeID string                 `json:"leaseHolderNodeId"`
		Members           []networkMemberSummary `json:"members"`
		Warnings          []string               `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("Unmarshal(stdout) error = %v\nstdout=%q", err, stdout)
	}
	if payload.NetworkID != "net-1" {
		t.Fatalf("payload.NetworkID = %q, want %q", payload.NetworkID, "net-1")
	}
	if payload.CurrentNodeID != "node-1" {
		t.Fatalf("payload.CurrentNodeID = %q, want %q", payload.CurrentNodeID, "node-1")
	}
	if payload.LeaseHolderNodeID != "node-2" {
		t.Fatalf("payload.LeaseHolderNodeID = %q, want %q", payload.LeaseHolderNodeID, "node-2")
	}
	if len(payload.Members) != 2 {
		t.Fatalf("len(payload.Members) = %d, want 2", len(payload.Members))
	}
	if !payload.Members[0].Current || payload.Members[0].LeaseHolder {
		t.Fatalf("first member flags = %#v, want current=true leaseHolder=false", payload.Members[0])
	}
	if payload.Members[1].Current || !payload.Members[1].LeaseHolder {
		t.Fatalf("second member flags = %#v, want current=false leaseHolder=true", payload.Members[1])
	}
	if len(payload.Warnings) != 1 || payload.Warnings[0] == "" {
		t.Fatalf("payload.Warnings = %#v, want one warning", payload.Warnings)
	}
}

func TestNetworkMembersHumanOutputUsesTable(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	now := time.Date(2026, time.May, 22, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := protocol.NodeStatusResponse{
			NetworkID: "net-1",
			Membership: protocol.Membership{
				NodeID:   "node-1",
				NodeName: "worker-1",
				GitHub:   protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
			},
			Memberships: []protocol.Membership{{
				NodeID:   "node-1",
				NodeName: "worker-1",
				GitHub:   protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
				Capabilities: protocol.NodeCapabilities{
					Roles:          []string{"worker", "reviewer"},
					RoutedProjects: 1,
					LocalProjects:  2,
				},
				LastHeartbeatAt: ptrTime(now),
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	if err := client.SaveState(filepath.Join(homeDir, ".looper", "network.json"), client.LocalState{
		URL:       server.URL,
		NetworkID: "net-1",
		NodeID:    "node-1",
		NodeName:  "worker-1",
		NodeToken: "node-token",
		GitHub:    protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
	}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	app := New(Deps{HomeDir: homeDir})
	exitCode, stdout, stderr := runAppWithDeps(t, app, []string{"network", "members"})
	if exitCode != 0 {
		t.Fatalf("Run(network members) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	for _, want := range []string{"Network members", "nodeId", "node", "github", "roles", "routed", "local", "lastHeartbeat", "current", "lease", "node-1", "worker-1", "mrcfps"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, want to contain %q", stdout, want)
		}
	}
}

func TestNetworkMembersRequiresJoinedNetwork(t *testing.T) {
	t.Parallel()

	app := New(Deps{HomeDir: t.TempDir()})
	exitCode, stdout, stderr := runAppWithDeps(t, app, []string{"network", "members"})
	if exitCode == 0 {
		t.Fatalf("Run(network members) exit code = 0, want non-zero")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if got := stderr; got == "" || !strings.Contains(got, "network members requires a joined network") {
		t.Fatalf("stderr = %q, want joined-network error", got)
	}
}

func TestNetworkMembersRequiresReachableStatusEndpoint(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	if err := client.SaveState(filepath.Join(homeDir, ".looper", "network.json"), client.LocalState{
		URL:       serverURL,
		NetworkID: "net-1",
		NodeID:    "node-1",
		NodeName:  "worker-1",
		NodeToken: "node-token",
		GitHub:    protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
	}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	app := New(Deps{HomeDir: homeDir})
	exitCode, stdout, stderr := runAppWithDeps(t, app, []string{"network", "members"})
	if exitCode == 0 {
		t.Fatalf("Run(network members) exit code = 0, want non-zero")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if got := stderr; got == "" || !strings.Contains(got, "network members requires a reachable loopernet status endpoint") || !strings.Contains(got, "connection refused") {
		t.Fatalf("stderr = %q, want reachable-endpoint error with underlying cause", got)
	}
}

func TestNetworkMembersPreservesStatusEndpointAuthErrors(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"node token revoked"}`))
	}))
	defer server.Close()

	if err := client.SaveState(filepath.Join(homeDir, ".looper", "network.json"), client.LocalState{
		URL:       server.URL,
		NetworkID: "net-1",
		NodeID:    "node-1",
		NodeName:  "worker-1",
		NodeToken: "node-token",
		GitHub:    protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
	}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	app := New(Deps{HomeDir: homeDir})
	exitCode, stdout, stderr := runAppWithDeps(t, app, []string{"network", "members"})
	if exitCode == 0 {
		t.Fatalf("Run(network members) exit code = 0, want non-zero")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if got := stderr; got == "" || !strings.Contains(got, "node token revoked") || strings.Contains(got, "requires a reachable loopernet status endpoint") {
		t.Fatalf("stderr = %q, want preserved auth error without reachability wrapper", got)
	}
}

func TestNetworkMembersIncludesLeaseHolderNodeIDWithoutActiveMembership(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := protocol.NodeStatusResponse{
			NetworkID: "net-1",
			Membership: protocol.Membership{
				NodeID:   "node-1",
				NodeName: "worker-1",
				GitHub:   protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
			},
			Memberships: []protocol.Membership{{
				NodeID:   "node-1",
				NodeName: "worker-1",
				GitHub:   protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
			}},
			Lease: protocol.CoordinatorLease{HolderNodeID: "node-2"},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	if err := client.SaveState(filepath.Join(homeDir, ".looper", "network.json"), client.LocalState{
		URL:       server.URL,
		NetworkID: "net-1",
		NodeID:    "node-1",
		NodeName:  "worker-1",
		NodeToken: "node-token",
		GitHub:    protocol.GitHubIdentity{Login: "mrcfps", NumericID: 23410977},
	}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	app := New(Deps{HomeDir: homeDir})
	exitCode, stdout, stderr := runAppWithDeps(t, app, []string{"network", "members", "--json"})
	if exitCode != 0 {
		t.Fatalf("Run(network members --json) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	var payload struct {
		LeaseHolderNodeID string  `json:"leaseHolderNodeId"`
		LeaseHolderName   *string `json:"leaseHolderName"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("Unmarshal(stdout) error = %v\nstdout=%q", err, stdout)
	}
	if payload.LeaseHolderNodeID != "node-2" {
		t.Fatalf("payload.LeaseHolderNodeID = %q, want %q", payload.LeaseHolderNodeID, "node-2")
	}
	if payload.LeaseHolderName != nil {
		t.Fatalf("payload.LeaseHolderName = %q, want nil", *payload.LeaseHolderName)
	}
}

func TestIsNetworkStatusReachabilityErrorIgnoresContextCancellation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if isNetworkStatusReachabilityError(tc.err) {
				t.Fatalf("isNetworkStatusReachabilityError(%v) = true, want false", tc.err)
			}
		})
	}
}

func ptrTime(value time.Time) *time.Time {
	return &value
}

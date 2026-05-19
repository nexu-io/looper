package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/network/protocol"
)

func TestAdminAndNodeAuthScopingAndJoinKeyConsumption(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/status", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /status status = %d, want 401", resp.StatusCode)
	}

	joinKey := createJoinKey(t, server.URL)
	joinResp := joinNode(t, server.URL, joinKey, "worker-1", 101)

	heartbeatBody, _ := json.Marshal(protocol.HeartbeatRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", NodeName: "worker-1"})
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/heartbeat", bytes.NewReader(heartbeatBody))
	req.Header.Set("Authorization", "Bearer "+joinResp.NodeToken)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/heartbeat status = %d, want 200", resp.StatusCode)
	}

	joinReqBody, _ := json.Marshal(protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey, NodeName: "worker-2", GitHub: protocol.GitHubIdentity{NumericID: 202}})
	resp, _ = http.Post(server.URL+"/v1/join", "application/json", bytes.NewReader(joinReqBody))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reused join key status = %d, want 400", resp.StatusCode)
	}
	status := getStatus(t, server.URL)
	if len(status.Memberships) != 1 {
		t.Fatalf("memberships = %d, want 1", len(status.Memberships))
	}
}

func TestDuplicateGitHubIdentityWarningsAndStatus(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 303)
	resp := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-2", 303)
	if len(resp.Warnings) == 0 {
		t.Fatal("join warnings = empty, want duplicate identity warning")
	}
	status := getStatus(t, server.URL)
	if len(status.Warnings) == 0 {
		t.Fatal("status warnings = empty, want duplicate identity warning")
	}
	found := 0
	for _, member := range status.Memberships {
		if member.DuplicateWarning {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("duplicate warning memberships = %d, want 2", found)
	}
}

func TestNetworkIDPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "net.sqlite")
	service1, err := Open(ctx, Config{DBPath: dbPath, AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion})
	if err != nil {
		t.Fatalf("Open(service1) error = %v", err)
	}
	networkID1, err := service1.NetworkID(ctx)
	if err != nil {
		t.Fatalf("NetworkID(service1) error = %v", err)
	}
	if err := service1.Close(); err != nil {
		t.Fatalf("Close(service1) error = %v", err)
	}
	service2, err := Open(ctx, Config{DBPath: dbPath, AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion})
	if err != nil {
		t.Fatalf("Open(service2) error = %v", err)
	}
	defer service2.Close()
	networkID2, err := service2.NetworkID(ctx)
	if err != nil {
		t.Fatalf("NetworkID(service2) error = %v", err)
	}
	if networkID1 != networkID2 {
		t.Fatalf("network ID after reopen = %q, want %q", networkID2, networkID1)
	}
}

func TestRouterLeaseAtomicityAndStaleToken412(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	node1 := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 401)
	node2 := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-2", 402)

	lease1 := acquireLease(t, server.URL, node1.NodeToken)
	if lease1.FencingToken != 1 {
		t.Fatalf("fencing token = %d, want 1", lease1.FencingToken)
	}
	reqBody, _ := json.Marshal(protocol.RouterLeaseAcquireRequest{})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/router-lease/acquire", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+node2.NodeToken)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("second acquire status = %d, want 400", resp.StatusCode)
	}

	revalidateTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Looper-Router-Fencing-Token") != "1" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer revalidateTarget.Close()

	handoffReq, _ := json.Marshal(protocol.RouterLeaseHandoffRequest{FencingToken: lease1.FencingToken, TargetNodeName: "worker-2"})
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/router-lease/handoff", bytes.NewReader(handoffReq))
	req.Header.Set("Authorization", "Bearer "+node1.NodeToken)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("handoff status = %d, want 200", resp.StatusCode)
	}

	revalidateReq, _ := json.Marshal(protocol.RouterLeaseRevalidateRequest{FencingToken: 1, URL: revalidateTarget.URL})
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/router-lease/revalidate", bytes.NewReader(revalidateReq))
	req.Header.Set("Authorization", "Bearer "+node1.NodeToken)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale revalidate status = %d, want 412", resp.StatusCode)
	}
}

func TestEventsRejectArbitraryBearerToken(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-node")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("events status = %d, want 401", resp.StatusCode)
	}
}

func TestHeartbeatDoesNotMutateStoredIdentity(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	joinResp := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 303)
	body, _ := json.Marshal(protocol.HeartbeatRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 999, Login: "spoofed"}})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/heartbeat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+joinResp.NodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("heartbeat request error = %v", err)
	}
	defer resp.Body.Close()
	status := getNodeStatus(t, server.URL, joinResp.NodeToken)
	if status.Membership.GitHub.NumericID != 303 {
		t.Fatalf("stored github numeric id = %d, want 303", status.Membership.GitHub.NumericID)
	}
}

func TestLeaveAllowsRejoinSameNodeName(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	first := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 101)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/leave", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+first.NodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("leave request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("leave status = %d, want 200", resp.StatusCode)
	}
	second := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 101)
	if second.NodeToken == first.NodeToken {
		t.Fatal("rejoin reused previous node token")
	}
}

func TestMalformedAcquireRequestRejectedAndServerTTLWins(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	node := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 401)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/router-lease/acquire", bytes.NewReader([]byte(`{"ttlSeconds":`)))
	req.Header.Set("Authorization", "Bearer "+node.NodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("malformed acquire request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed acquire status = %d, want 400", resp.StatusCode)
	}

	service.config.LeaseTTLSeconds = 1
	body, _ := json.Marshal(protocol.RouterLeaseAcquireRequest{TTLSeconds: 999})
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/router-lease/acquire", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.NodeToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("acquire request error = %v", err)
	}
	defer resp.Body.Close()
	var lease protocol.RouterLease
	_ = json.NewDecoder(resp.Body).Decode(&lease)
	if lease.ExpiresAt == nil || lease.ExpiresAt.Sub(time.Now().UTC()) > 5*time.Second {
		t.Fatalf("lease expiry = %v, want server-owned short ttl", lease.ExpiresAt)
	}
}

func TestExpiredLeaseCannotBeRenewed(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	service.config.LeaseTTLSeconds = 1
	node := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 401)
	lease := acquireLease(t, server.URL, node.NodeToken)
	time.Sleep(1100 * time.Millisecond)
	body, _ := json.Marshal(protocol.RouterLeaseRenewRequest{FencingToken: lease.FencingToken})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/router-lease/renew", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.NodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("renew request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expired renew status = %d, want 412", resp.StatusCode)
	}
}

func newTestHTTPServer(t *testing.T) (*httptest.Server, *Service) {
	t.Helper()
	ctx := context.Background()
	service, err := Open(ctx, Config{DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion, MinimumDaemonVersion: "1.2.0", ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	server := httptest.NewServer(NewServer(Config{AdminToken: "admin-token"}, service).httpServer.Handler)
	return server, service
}

func createJoinKey(t *testing.T, baseURL string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/join-keys", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create join key request error = %v", err)
	}
	defer resp.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body["joinKey"]
}

func joinNode(t *testing.T, baseURL, joinKey, nodeName string, githubID int64) protocol.JoinResponse {
	t.Helper()
	body, _ := json.Marshal(protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey, NodeName: nodeName, GitHub: protocol.GitHubIdentity{NumericID: githubID, Login: nodeName}, TargetLabels: []string{"linux"}})
	resp, err := http.Post(baseURL+"/v1/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("join request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("join status = %d, want 201: %#v", resp.StatusCode, body)
	}
	var out protocol.JoinResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func getStatus(t *testing.T, baseURL string) protocol.StatusResponse {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/status", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status request error = %v", err)
	}
	defer resp.Body.Close()
	var out protocol.StatusResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func acquireLease(t *testing.T, baseURL, token string) protocol.RouterLease {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/router-lease/acquire", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("acquire lease request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("acquire lease status = %d, want 200", resp.StatusCode)
	}
	var out protocol.RouterLease
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func getNodeStatus(t *testing.T, baseURL, token string) protocol.NodeStatusResponse {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("node status request error = %v", err)
	}
	defer resp.Body.Close()
	var out protocol.NodeStatusResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

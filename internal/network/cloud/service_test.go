package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

func TestDuplicateWarningsIgnoreUnknownGitHubIDs(t *testing.T) {
	ctx := context.Background()
	service, err := Open(ctx, Config{DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion, MinimumDaemonVersion: "1.2.0"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer service.Close()

	for _, nodeName := range []string{"worker-1", "worker-2"} {
		joinKey, err := service.CreateJoinKey(ctx)
		if err != nil {
			t.Fatalf("CreateJoinKey() error = %v", err)
		}
		if _, err := service.Join(ctx, protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey, NodeName: nodeName, GitHub: protocol.GitHubIdentity{Login: nodeName}}); err != nil {
			t.Fatalf("Join(%s) error = %v", nodeName, err)
		}
	}

	warnings, err := service.duplicateWarnings(ctx)
	if err != nil {
		t.Fatalf("duplicateWarnings() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("duplicateWarnings() = %v, want none for unknown numeric IDs", warnings)
	}

	status, err := service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(status.Warnings) != 0 {
		t.Fatalf("Status().Warnings = %v, want none", status.Warnings)
	}
	for _, member := range status.Memberships {
		if member.DuplicateWarning {
			t.Fatalf("member %s duplicate warning = true, want false", member.NodeName)
		}
	}
}

func TestJoinPersistsProvidedTargetLabels(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()

	labels := []string{"linux", "gpu"}
	body, _ := json.Marshal(protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: createJoinKey(t, server.URL), NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-1"}, TargetLabels: labels})
	resp, err := http.Post(server.URL+"/v1/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("join request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("join status = %d, want 201", resp.StatusCode)
	}

	status := getStatus(t, server.URL)
	if got := status.Memberships[0].TargetLabels; len(got) != len(labels) || got[0] != labels[0] || got[1] != labels[1] {
		t.Fatalf("target labels = %v, want %v", got, labels)
	}
}

func TestOpenReturnsEntropyFailure(t *testing.T) {
	restore := stubRandRead(t, func([]byte) (int, error) { return 0, errors.New("entropy unavailable") })
	defer restore()

	_, err := Open(context.Background(), Config{DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion})
	if err == nil || err.Error() != "entropy unavailable" {
		t.Fatalf("Open() error = %v, want entropy unavailable", err)
	}
}

func TestCreateJoinKeyReturnsEntropyFailure(t *testing.T) {
	ctx := context.Background()
	service, err := Open(ctx, Config{DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion, NetworkID: "net-fixed"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer service.Close()

	restore := stubRandRead(t, func([]byte) (int, error) { return 0, errors.New("entropy unavailable") })
	defer restore()

	_, err = service.CreateJoinKey(ctx)
	if err == nil || err.Error() != "entropy unavailable" {
		t.Fatalf("CreateJoinKey() error = %v, want entropy unavailable", err)
	}
}

func TestJoinReturnsEntropyFailure(t *testing.T) {
	ctx := context.Background()
	service, err := Open(ctx, Config{DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion, MinimumDaemonVersion: "1.2.0", NetworkID: "net-fixed"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer service.Close()

	joinKey := "join-fixed"
	if _, err := service.db.ExecContext(ctx, `INSERT INTO join_keys(join_key, created_at) VALUES(?, ?)`, joinKey, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert join key error = %v", err)
	}

	restore := stubRandRead(t, func([]byte) (int, error) { return 0, errors.New("entropy unavailable") })
	defer restore()

	_, err = service.Join(ctx, protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey, NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-1"}})
	if err == nil || err.Error() != "entropy unavailable" {
		t.Fatalf("Join() error = %v, want entropy unavailable", err)
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

func TestCoordinatorLeaseAtomicityAndStaleToken412(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	node1 := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 401)
	node2 := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-2", 402)

	lease1 := acquireLease(t, server.URL, node1.NodeToken)
	if lease1.FencingToken != 1 {
		t.Fatalf("fencing token = %d, want 1", lease1.FencingToken)
	}
	reqBody, _ := json.Marshal(protocol.CoordinatorLeaseAcquireRequest{})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/acquire", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+node2.NodeToken)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("second acquire status = %d, want 400", resp.StatusCode)
	}

	revalidateTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Looper-Coordinator-Fencing-Token") != "1" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer revalidateTarget.Close()

	handoffReq, _ := json.Marshal(protocol.CoordinatorLeaseHandoffRequest{FencingToken: lease1.FencingToken, TargetNodeName: "worker-2"})
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/handoff", bytes.NewReader(handoffReq))
	req.Header.Set("Authorization", "Bearer "+node1.NodeToken)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("handoff status = %d, want 200", resp.StatusCode)
	}

	revalidateReq, _ := json.Marshal(protocol.CoordinatorLeaseRevalidateRequest{FencingToken: 1, URL: revalidateTarget.URL})
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/revalidate", bytes.NewReader(revalidateReq))
	req.Header.Set("Authorization", "Bearer "+node1.NodeToken)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale revalidate status = %d, want 412", resp.StatusCode)
	}
}

func TestCoordinatorLeaseRevalidateProbesTarget(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	node := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 401)
	lease := acquireLease(t, server.URL, node.NodeToken)

	var probeCount int
	revalidateTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCount++
		if got, want := r.Method, http.MethodHead; got != want {
			t.Fatalf("probe method = %s, want %s", got, want)
		}
		if got, want := r.Header.Get("X-Looper-Coordinator-Fencing-Token"), "1"; got != want {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer revalidateTarget.Close()

	body, _ := json.Marshal(protocol.CoordinatorLeaseRevalidateRequest{FencingToken: lease.FencingToken, URL: revalidateTarget.URL, Method: http.MethodHead})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/revalidate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.NodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("revalidate request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revalidate status = %d, want 200", resp.StatusCode)
	}
	if probeCount != 1 {
		t.Fatalf("probe count = %d, want 1", probeCount)
	}

	deadTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	deadURL := deadTarget.URL
	deadTarget.Close()
	body, _ = json.Marshal(protocol.CoordinatorLeaseRevalidateRequest{FencingToken: lease.FencingToken, URL: deadURL})
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/revalidate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.NodeToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed revalidate request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("failed revalidate status = %d, want 412", resp.StatusCode)
	}
}

func TestCoordinatorLeaseRevalidateRejectsRedirectTarget(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	node := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 401)
	lease := acquireLease(t, server.URL, node.NodeToken)

	redirected := false
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer redirector.Close()

	body, _ := json.Marshal(protocol.CoordinatorLeaseRevalidateRequest{FencingToken: lease.FencingToken, URL: redirector.URL})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/revalidate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.NodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("revalidate request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("redirect revalidate status = %d, want 412", resp.StatusCode)
	}
	if redirected {
		t.Fatal("redirect target was probed, want redirect rejected before follow")
	}
}

func TestCoordinatorLeaseRevalidateRechecksLeaseAfterProbe(t *testing.T) {
	ctx := context.Background()
	service, err := Open(ctx, Config{DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion, MinimumDaemonVersion: "1.2.0"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer service.Close()

	joinKey, err := service.CreateJoinKey(ctx)
	if err != nil {
		t.Fatalf("CreateJoinKey(node1) error = %v", err)
	}
	node1, err := service.Join(ctx, protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey, NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 401, Login: "worker-1"}})
	if err != nil {
		t.Fatalf("Join(node1) error = %v", err)
	}
	joinKey, err = service.CreateJoinKey(ctx)
	if err != nil {
		t.Fatalf("CreateJoinKey(node2) error = %v", err)
	}
	node2, err := service.Join(ctx, protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey, NodeName: "worker-2", GitHub: protocol.GitHubIdentity{NumericID: 402, Login: "worker-2"}})
	if err != nil {
		t.Fatalf("Join(node2) error = %v", err)
	}
	lease, err := service.AcquireLease(ctx, node1.NodeToken, 0)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}

	probed := make(chan struct{})
	releaseProbe := make(chan struct{})
	revalidateTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(probed)
		<-releaseProbe
		w.WriteHeader(http.StatusOK)
	}))
	defer revalidateTarget.Close()

	revalidateErr := make(chan error, 1)
	go func() {
		revalidateErr <- service.RevalidateLease(ctx, node1.NodeToken, protocol.CoordinatorLeaseRevalidateRequest{FencingToken: lease.FencingToken, URL: revalidateTarget.URL})
	}()

	<-probed
	if _, err := service.HandoffLease(ctx, node1.NodeToken, lease.FencingToken, "worker-2", 0); err != nil {
		t.Fatalf("HandoffLease() error = %v", err)
	}
	close(releaseProbe)

	if err := <-revalidateErr; err == nil || err.Error() != "stale coordinator lease token; current token is 2" {
		t.Fatalf("RevalidateLease() error = %v, want stale token after probe", err)
	}
	if _, err := service.NodeStatus(ctx, node2.NodeToken); err != nil {
		t.Fatalf("NodeStatus(node2) error = %v", err)
	}
}

func TestCoordinatorLeaseRevalidateTimesOutHungTarget(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	node := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 401)
	lease := acquireLease(t, server.URL, node.NodeToken)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer listener.Close()
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-time.After(3 * time.Second)
	}()

	body, _ := json.Marshal(protocol.CoordinatorLeaseRevalidateRequest{FencingToken: lease.FencingToken, URL: "http://" + listener.Addr().String()})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/revalidate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.NodeToken)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("revalidate request error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("timeout revalidate status = %d, want 412", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed >= 2800*time.Millisecond {
		t.Fatalf("revalidate elapsed = %v, want bounded timeout before hung target returns", elapsed)
	}
	<-acceptDone
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

func TestEventsStopAfterNodeLeaves(t *testing.T) {
	ctx := context.Background()
	service, err := Open(ctx, Config{DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion, MinimumDaemonVersion: "1.2.0"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer service.Close()
	joinKey, err := service.CreateJoinKey(ctx)
	if err != nil {
		t.Fatalf("CreateJoinKey(node1) error = %v", err)
	}
	node1, err := service.Join(ctx, protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey, NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 401, Login: "worker-1"}})
	if err != nil {
		t.Fatalf("Join(node1) error = %v", err)
	}
	joinKey, err = service.CreateJoinKey(ctx)
	if err != nil {
		t.Fatalf("CreateJoinKey(node2) error = %v", err)
	}
	node2, err := service.Join(ctx, protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey, NodeName: "worker-2", GitHub: protocol.GitHubIdentity{NumericID: 402, Login: "worker-2"}})
	if err != nil {
		t.Fatalf("Join(node2) error = %v", err)
	}

	handler := NewServer(Config{AdminToken: "admin-token"}, service)
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+node1.NodeToken)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.handleEvents(recorder, req)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		service.mu.Lock()
		subscribed := len(service.subs) == 1
		service.mu.Unlock()
		if subscribed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	service.mu.Lock()
	subscribed := len(service.subs) == 1
	service.mu.Unlock()
	if !subscribed {
		t.Fatal("event handler did not subscribe")
	}

	if err := service.Leave(ctx, node1.NodeToken); err != nil {
		t.Fatalf("Leave() error = %v", err)
	}

	if _, err := service.AcquireLease(ctx, node2.NodeToken, 0); err != nil {
		t.Fatalf("AcquireLease(node2) error = %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("event handler stayed open after node leave")
	}
	if body := recorder.Body.String(); body != "" {
		t.Fatalf("event stream delivered payload after leave: %q", body)
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
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/acquire", bytes.NewReader([]byte(`{"ttlSeconds":`)))
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
	body, _ := json.Marshal(protocol.CoordinatorLeaseAcquireRequest{TTLSeconds: 999})
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/acquire", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+node.NodeToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("acquire request error = %v", err)
	}
	defer resp.Body.Close()
	var lease protocol.CoordinatorLease
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
	body, _ := json.Marshal(protocol.CoordinatorLeaseRenewRequest{FencingToken: lease.FencingToken})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/coordinator-lease/renew", bytes.NewReader(body))
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

func acquireLease(t *testing.T, baseURL, token string) protocol.CoordinatorLease {
	t.Helper()
	lease, err := acquireLeaseViaService(baseURL, token)
	if err != nil {
		t.Fatalf("acquire lease request error = %v", err)
	}
	return lease
}

func acquireLeaseViaService(baseURL, token string) (protocol.CoordinatorLease, error) {
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/coordinator-lease/acquire", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return protocol.CoordinatorLease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protocol.CoordinatorLease{}, fmt.Errorf("acquire lease status = %d, want 200", resp.StatusCode)
	}
	var out protocol.CoordinatorLease
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, nil
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

func stubRandRead(t *testing.T, fn func([]byte) (int, error)) func() {
	t.Helper()
	prev := randRead
	randRead = fn
	return func() { randRead = prev }
}

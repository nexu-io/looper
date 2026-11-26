package forge

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/planeprotocol"
	"github.com/nexu-io/looper/internal/planestrict"
)

func TestStrictPlannerClaimsOnlySelectedItemAndStartsCurrentAttempt(t *testing.T) {
	creds := planestrict.Credentials{BindingID: planeprotocol.UUID{1}, SessionID: planeprotocol.UUID{2}, PrivateKey: ed25519.NewKeyFromSeed(make([]byte, 32)), NodeID: "mac", KeyRevision: 1}
	attempt := "77777777-8888-4999-8aaa-bbbbbbbbbbbb"
	d := planestrict.Dispatch{ID: "66666666-7777-4888-8999-aaaaaaaaaaaa", IssueID: "wi-1", Revision: 1, StateVersion: 1, ActiveRole: "planner", State: "queued", Health: "ok"}
	claims, starts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/session/"):
			writeJSON(t, w, 200, map[string]string{"session_id": planestrict.UUIDString(creds.SessionID), "state": "active"})
		case strings.HasSuffix(r.URL.Path, "/inbox/"):
			writeJSON(t, w, 200, planestrict.InboxResponse{IntegrationState: "active", Dispatches: []planestrict.Dispatch{d}})
		case strings.HasSuffix(r.URL.Path, "/work-items/wi-1/"):
			writeJSON(t, w, 200, map[string]any{"id": "wi-1", "sequence_id": 42, "name": "Unlabelled bug"})
		case strings.HasSuffix(r.URL.Path, "/labels/"):
			writeJSON(t, w, 200, map[string]any{"results": []any{}})
		case strings.HasSuffix(r.URL.Path, "/claim/"):
			claims++
			d.State, d.ExecutionAttemptID, d.FencingToken = "claimed", &attempt, 1
			d.StateVersion++
			writeJSON(t, w, 200, planestrict.MutationResponse{Dispatch: d, Claimed: true})
		case strings.HasSuffix(r.URL.Path, "/transition/"):
			starts++
			d.State = "running"
			d.StateVersion++
			writeJSON(t, w, 200, planestrict.MutationResponse{Dispatch: d})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := newPlaneTestClient(t, server)
	var err error
	c.strictClient, err = planestrict.NewClient(server.URL, "acme", "proj-123", creds)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if is, err := c.ListOpenIssues(ctx, ListIssuesInput{}); err != nil || len(is) != 0 {
		t.Fatalf("worker discovery: %v %v", is, err)
	}
	if is, err := c.ListPlannerIssues(ctx, ListIssuesInput{}, 43); err != nil || len(is) != 0 || claims != 0 {
		t.Fatalf("filtered planner: %v, %v, claims=%d", is, err, claims)
	}
	for i := 0; i < 2; i++ {
		is, err := c.ListPlannerIssues(ctx, ListIssuesInput{}, 42)
		if err != nil || len(is) != 1 || is[0].StrictAttemptID != attempt || is[0].StrictDispatchID != d.ID || claims != 1 {
			t.Fatalf("planner poll %d: %v, %v, claims=%d", i, is, err, claims)
		}
	}
	if err := c.StartStrictDispatch(ctx, d.ID, "obsolete"); err == nil {
		t.Fatal("started stale attempt")
	}
	expired := time.Now().Add(-time.Minute)
	d.ClaimLeaseExpiresAt = &expired
	if err := c.StartStrictDispatch(ctx, d.ID, attempt); err == nil {
		t.Fatal("started expired lease")
	}
	d.ClaimLeaseExpiresAt = nil
	d.State = "stop_requested"
	if err := c.StartStrictDispatch(ctx, d.ID, attempt); err == nil {
		t.Fatal("started stopped dispatch")
	}
	d.State = "claimed"
	for i := 0; i < 2; i++ {
		if err := c.StartStrictDispatch(ctx, d.ID, attempt); err != nil {
			t.Fatal(err)
		}
	}
	if is, err := c.ListPlannerIssues(ctx, ListIssuesInput{}, 0); err != nil || len(is) != 0 || starts != 1 {
		t.Fatalf("after start: %v %v, starts=%d", is, err, starts)
	}
}

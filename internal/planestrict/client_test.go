package planestrict

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/planeprotocol"
)

func TestClientSignsInboxClaimAndTransition(t *testing.T) {
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	bindingID := mustUUID(t, "55555555-6666-4777-8888-999999999999")
	sessionID := mustUUID(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	var instanceNonce [16]byte
	copy(instanceNonce[:], bytes.Repeat([]byte{0x33}, 16))
	dispatchID := "66666666-7777-4888-8999-aaaaaaaaaaaa"
	attemptID := "77777777-8888-4999-8aaa-bbbbbbbbbbbb"
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestCount++
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var expectedDispatch *Dispatch
		switch request.URL.Path {
		case "/api/workspaces/open-design/projects/project-id/looper/nodes/session/":
			writeJSON(t, response, map[string]any{
				"session_id": UUIDString(sessionID), "state": "active",
			})
		case "/api/workspaces/open-design/projects/project-id/looper/dispatch/inbox/":
			if request.URL.RawQuery != "cursor=&instance_nonce=MzMzMzMzMzMzMzMzMzMzMw&node_id=node-cyan&session_id=aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" {
				t.Fatalf("query = %q", request.URL.RawQuery)
			}
			writeJSON(t, response, InboxResponse{IntegrationState: "active", Dispatches: []Dispatch{{
				ID: dispatchID, IssueID: "88888888-9999-4aaa-8bbb-cccccccccccc", Revision: 3,
				StateVersion: 1, State: "queued", NodeID: "node-cyan",
			}}})
		case "/api/workspaces/open-design/projects/project-id/looper/dispatch/" + dispatchID + "/claim/":
			dispatch := Dispatch{ID: dispatchID, Revision: 3, StateVersion: 1}
			expectedDispatch = &dispatch
			attempt := attemptID
			writeJSON(t, response, MutationResponse{Claimed: true, Dispatch: Dispatch{
				ID: dispatchID, Revision: 3, StateVersion: 2, State: "claimed",
				ExecutionAttemptID: &attempt, FencingToken: 9,
			}})
		case "/api/workspaces/open-design/projects/project-id/looper/dispatch/" + dispatchID + "/transition/":
			var body map[string]any
			if err := json.Unmarshal(rawBody, &body); err != nil {
				t.Fatal(err)
			}
			stateVersion := uint64(2)
			responseState := "running"
			if body["state"] == "awaiting_human" {
				stateVersion = 3
				responseState = "awaiting_human"
				summary, _ := body["termination_summary"].(map[string]any)
				if summary["process_group_empty"] != true || summary["evidence"] != "looper-runner-wait" {
					t.Fatalf("termination summary = %#v", summary)
				}
			}
			dispatch := Dispatch{ID: dispatchID, Revision: 3, StateVersion: stateVersion, ExecutionAttemptID: &attemptID, FencingToken: 9}
			expectedDispatch = &dispatch
			writeJSON(t, response, MutationResponse{Dispatch: Dispatch{
				ID: dispatchID, Revision: 3, StateVersion: stateVersion + 1, State: responseState,
				ExecutionAttemptID: &attemptID, FencingToken: 9,
			}})
		case "/api/workspaces/open-design/projects/project-id/looper/dispatch/" + dispatchID + "/role-requests/":
			dispatch := Dispatch{ID: dispatchID, Revision: 3, StateVersion: 3, ExecutionAttemptID: &attemptID, FencingToken: 9}
			expectedDispatch = &dispatch
			var body map[string]any
			if err := json.Unmarshal(rawBody, &body); err != nil {
				t.Fatal(err)
			}
			if body["role"] != "product" || body["loop_id"] != "loop-1" {
				t.Fatalf("role request body = %#v", body)
			}
			writeJSON(t, response, map[string]any{"role_request": map[string]any{
				"id": "99999999-aaaa-4bbb-8ccc-dddddddddddd", "role": "product", "status": "open",
				"request_comment_id": "comment-1", "eligible_member_id": "aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb",
				"eligible_member_name": "Product", "created_at": now.Format(time.RFC3339Nano), "created": true,
			}})
		case "/api/workspaces/open-design/projects/project-id/looper/dispatch/" + dispatchID + "/role-requests/messages/pending/":
			dispatch := Dispatch{ID: dispatchID, Revision: 3, StateVersion: 4, ExecutionAttemptID: &attemptID, FencingToken: 9}
			expectedDispatch = &dispatch
			writeJSON(t, response, map[string]any{"pending": []any{map[string]any{
				"role_request": map[string]any{"id": "99999999-aaaa-4bbb-8ccc-dddddddddddd", "role": "product", "status": "open", "conversation_state": "waiting_looper", "questions": []any{map[string]any{"id": "PROD-001", "question": "Retry mode?", "context": "Choose behavior."}}},
				"message":      map[string]any{"id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "client_message_id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "kind": "human_reply", "body": "自动重试会等多久？", "delivery_state": "pending", "created_at": now.Format(time.RFC3339Nano)},
				"history":      []any{},
			}}})
		case "/api/workspaces/open-design/projects/project-id/looper/dispatch/" + dispatchID + "/role-requests/99999999-aaaa-4bbb-8ccc-dddddddddddd/messages/":
			dispatch := Dispatch{ID: dispatchID, Revision: 3, StateVersion: 4, ExecutionAttemptID: &attemptID, FencingToken: 9}
			expectedDispatch = &dispatch
			var body map[string]any
			if err := json.Unmarshal(rawBody, &body); err != nil {
				t.Fatal(err)
			}
			if body["reply"] != "最多增加 6 秒。" || len(body["processed_message_ids"].([]any)) != 1 {
				t.Fatalf("role message reply body = %#v", body)
			}
			writeJSON(t, response, map[string]any{"created": true, "message": map[string]any{
				"id": "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "client_message_id": body["client_message_id"], "kind": "looper_reply", "body": body["reply"], "delivery_state": "delivered", "created_at": now.Format(time.RFC3339Nano),
			}})
		case "/api/workspaces/open-design/projects/project-id/looper/dispatch/" + dispatchID + "/handoff/":
			dispatch := Dispatch{ID: dispatchID, Revision: 3, StateVersion: 3, ExecutionAttemptID: &attemptID, FencingToken: 9}
			expectedDispatch = &dispatch
			var body map[string]any
			if err := json.Unmarshal(rawBody, &body); err != nil {
				t.Fatal(err)
			}
			if body["approval_actor_member_id"] != "aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb" || body["spec_content_hash"] != strings.Repeat("a", 64) {
				t.Fatalf("handoff body = %#v", body)
			}
			writeJSON(t, response, MutationResponse{Dispatch: Dispatch{
				ID: dispatchID, Revision: 3, StateVersion: 4, State: "queued", ActiveRole: "worker",
			}})
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		verifyRequestSignature(t, request, rawBody, privateKey.Public().(ed25519.PublicKey), bindingID, expectedDispatch, now)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "open-design", "project-id", Credentials{
		BindingID: bindingID, KeyRevision: 2, PrivateKey: privateKey, NodeID: "node-cyan",
		SessionID: sessionID, InstanceNonce: instanceNonce,
	}, WithClock(func() time.Time { return now }), WithRandom(bytes.NewReader(bytes.Repeat([]byte{0x11}, 144))))
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := client.Inbox(context.Background(), "")
	if err != nil || len(inbox.Dispatches) != 1 {
		t.Fatalf("Inbox() = %#v, %v", inbox, err)
	}
	claimed, err := client.Claim(context.Background(), inbox.Dispatches[0], "99999999-aaaa-4bbb-8ccc-dddddddddddd")
	if err != nil || !claimed.Claimed || claimed.Dispatch.ExecutionAttemptID == nil {
		t.Fatalf("Claim() = %#v, %v", claimed, err)
	}
	transitioned, err := client.Transition(context.Background(), claimed.Dispatch, "running", nil)
	if err != nil || transitioned.Dispatch.State != "running" {
		t.Fatalf("Transition() = %#v, %v", transitioned, err)
	}
	roleRequest, err := client.CreateRoleRequest(context.Background(), transitioned.Dispatch, RoleRequestInput{
		LoopID: "loop-1", DecisionRevision: 1, Role: "product", BriefSummary: "Export retry",
		Questions: []RoleQuestion{{ID: "PROD-001", Question: "Retry mode?", Context: "Choose behavior."}},
	})
	if err != nil || roleRequest.RoleRequest.EligibleMemberName != "Product" {
		t.Fatalf("CreateRoleRequest() = %#v, %v", roleRequest, err)
	}
	waitKind := "technical_spec_approval"
	awaiting, err := client.Transition(context.Background(), transitioned.Dispatch, "awaiting_human", &waitKind)
	if err != nil || awaiting.Dispatch.State != "awaiting_human" {
		t.Fatalf("Transition(awaiting_human) = %#v, %v", awaiting, err)
	}
	pending, err := client.PendingRoleMessages(context.Background(), awaiting.Dispatch)
	if err != nil || len(pending.Pending) != 1 || pending.Pending[0].Message.Body == "" {
		t.Fatalf("PendingRoleMessages() = %#v, %v", pending, err)
	}
	replied, err := client.ReplyRoleMessage(context.Background(), awaiting.Dispatch, roleRequest.RoleRequest.ID, RoleMessageReplyInput{
		ClientMessageID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", InReplyToMessageID: pending.Pending[0].Message.ID,
		ProcessedMessageIDs: []string{pending.Pending[0].Message.ID}, Reply: "最多增加 6 秒。",
		Evaluation: RoleMessageEvaluation{Resolved: false, Questions: []RoleQuestionEvaluation{{ID: "PROD-001", Status: "still_open", Reason: "尚未选择"}}},
	})
	if err != nil || !replied.Created || replied.Message.Kind != "looper_reply" {
		t.Fatalf("ReplyRoleMessage() = %#v, %v", replied, err)
	}
	handedOff, err := client.Handoff(context.Background(), transitioned.Dispatch, HandoffInput{
		ApprovalActorMemberID: "aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb",
		ApprovalCommentID:     "bbbbbbbb-2222-4333-8444-cccccccccccc",
		SpecContentHash:       strings.Repeat("a", 64), SpecRevision: 2,
	})
	if err != nil || handedOff.Dispatch.ActiveRole != "worker" {
		t.Fatalf("Handoff() = %#v, %v", handedOff, err)
	}
	if requestCount != 9 {
		t.Fatalf("requests = %d, want 9", requestCount)
	}
}

func TestClientRejectsRedirectAndPlainHTTPOutsideLocalhost(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	credentials := Credentials{
		BindingID:   mustUUID(t, "55555555-6666-4777-8888-999999999999"),
		KeyRevision: 1, PrivateKey: privateKey, NodeID: "node-cyan",
	}
	if _, err := NewClient("http://plane.example.test", "workspace", "project", credentials); err == nil {
		t.Fatal("NewClient() accepted non-local HTTP")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "https://attacker.example/", http.StatusFound)
	}))
	defer redirect.Close()
	client, err := NewClient(redirect.URL, "workspace", "project", credentials)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Inbox(context.Background(), "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusFound {
		t.Fatalf("Inbox redirect error = %v", err)
	}
}

func verifyRequestSignature(t *testing.T, request *http.Request, rawBody []byte, publicKey ed25519.PublicKey, bindingID planeprotocol.UUID, dispatch *Dispatch, now time.Time) {
	t.Helper()
	header, err := planeprotocol.ParseSignatureHeader(request.Header.Get("Looper-Signature"))
	if err != nil {
		t.Fatal(err)
	}
	bodyHash := sha256.Sum256(rawBody)
	value := planeprotocol.NodeRequest{
		Method: request.Method, Path: request.URL.EscapedPath(), Query: request.URL.RawQuery,
		BodySHA256: bodyHash, BindingID: bindingID, KeyRevision: 2,
		TimestampMS: now.UnixMilli(), Nonce: header.Nonce,
	}
	if dispatch != nil {
		id := mustUUID(t, dispatch.ID)
		value.DispatchID = &id
		value.DispatchRevision = &dispatch.Revision
		value.StateVersion = &dispatch.StateVersion
		if dispatch.ExecutionAttemptID != nil {
			attempt := mustUUID(t, *dispatch.ExecutionAttemptID)
			value.ExecutionAttemptID = &attempt
			value.FencingToken = &dispatch.FencingToken
		}
	}
	payload, err := planeprotocol.EncodeNodeRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := planeprotocol.DomainDigest(planeprotocol.NodeRequestProfile, payload)
	if !ed25519.Verify(publicKey, digest[:], header.Signature[:]) {
		t.Fatal("request signature did not verify")
	}
}

func writeJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func mustUUID(t *testing.T, value string) planeprotocol.UUID {
	t.Helper()
	parsed, err := parseUUID(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

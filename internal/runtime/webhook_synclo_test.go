package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/webhookforward"
)

func TestSyncloInboxPollSignsForwardsAndAcks(t *testing.T) {
	const secret = "test-secret"
	t.Setenv("SYNC_HMAC_SECRET", secret)

	var acked syncloAckRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webhook/pending":
			if r.Method != http.MethodGet {
				t.Fatalf("pending method = %s, want GET", r.Method)
			}
			if r.URL.RequestURI() != "/webhook/pending?consumer=looper-node&limit=100" {
				t.Fatalf("pending RequestURI = %q, want exact consumer/limit query", r.URL.RequestURI())
			}
			wantSignature := syncloSignature(secret, "GET "+r.URL.RequestURI())
			if got := r.Header.Get("X-Signature"); got != wantSignature {
				t.Fatalf("pending X-Signature = %q, want %q", got, wantSignature)
			}
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"consumer":"looper-node","count":1,"events":[{"delivery_id":"delivery-1","event_type":"pull_request","action":"synchronize","repo":"acme/looper","entity_number":42,"payload_json":"{\"action\":\"synchronize\",\"repository\":{\"full_name\":\"acme/looper\"},\"pull_request\":{\"number\":42}}","received_at":"2026-06-18T00:00:00Z"}]}`))
		case "/webhook/ack":
			if r.Method != http.MethodPost {
				t.Fatalf("ack method = %s, want POST", r.Method)
			}
			var raw json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
				t.Fatalf("decode ack body: %v", err)
			}
			wantSignature := syncloSignature(secret, string(raw))
			if got := r.Header.Get("X-Signature"); got != wantSignature {
				t.Fatalf("ack X-Signature = %q, want %q", got, wantSignature)
			}
			if err := json.Unmarshal(raw, &acked); err != nil {
				t.Fatalf("unmarshal ack body: %v", err)
			}
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := config.SyncloWebhookConfig{
		BaseURL:             server.URL,
		Consumer:            "looper-node",
		SecretEnv:           "SYNC_HMAC_SECRET",
		Limit:               100,
		PollIntervalSeconds: 5,
	}
	forwarder := &recordingSyncloForwarder{}
	rt := &webhookRuntime{
		cfg:    config.Config{Webhook: config.WebhookConfig{Synclo: cfg}},
		now:    func() time.Time { return time.Date(2026, time.June, 18, 12, 0, 0, 0, time.UTC) },
		stopCh: make(chan struct{}),
		status: WebhookStatus{SyncloInbox: WebhookSyncloInboxState{}},
		forwarder: func() WebhookForwarder {
			return forwarder
		},
	}

	rt.pollSyncloInbox(context.Background(), server.Client(), cfg)

	requests := forwarder.recordedRequests()
	if len(requests) != 1 {
		t.Fatalf("forwarded requests = %d, want 1", len(requests))
	}
	if requests[0].DeliveryID != "delivery-1" || requests[0].EventType != "pull_request" {
		t.Fatalf("forwarded request = %#v, want delivery-1 pull_request", requests[0])
	}
	if len(acked.DeliveryIDs) != 1 || acked.DeliveryIDs[0] != "delivery-1" || acked.ConsumerName != "looper-node" {
		t.Fatalf("acked = %#v, want delivery-1 for looper-node", acked)
	}
	status := rt.Status()
	if status.SyncloInbox.PendingFetched != 1 || status.SyncloInbox.Forwarded != 1 || status.SyncloInbox.Acked != 1 {
		t.Fatalf("SyncloInbox status = %#v, want one fetched/forwarded/acked", status.SyncloInbox)
	}
	if status.Degraded {
		t.Fatalf("Status().Degraded = true, want false; reasons=%v", status.DegradedReasons)
	}
}

type recordingSyncloForwarder struct {
	mu       sync.Mutex
	requests []webhookforward.DeliveryRequest
}

func (f *recordingSyncloForwarder) Forward(_ context.Context, request webhookforward.DeliveryRequest) (webhookforward.ForwardResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	return webhookforward.ForwardResult{Status: "accepted", WorkItems: 1}, nil
}

func (f *recordingSyncloForwarder) Stats() webhookforward.Stats { return webhookforward.Stats{} }

func (f *recordingSyncloForwarder) Close() {}

func (f *recordingSyncloForwarder) recordedRequests() []webhookforward.DeliveryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]webhookforward.DeliveryRequest{}, f.requests...)
}

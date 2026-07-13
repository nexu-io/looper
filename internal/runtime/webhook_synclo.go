package runtime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/webhookforward"
)

const syncloInboxHTTPTimeout = 10 * time.Second

type syncloPendingResponse struct {
	OK       bool               `json:"ok"`
	Consumer string             `json:"consumer"`
	Count    int                `json:"count"`
	Events   []syncloInboxEvent `json:"events"`
}

type syncloInboxEvent struct {
	DeliveryID   string `json:"delivery_id"`
	EventType    string `json:"event_type"`
	Action       string `json:"action"`
	Repo         string `json:"repo"`
	EntityNumber int64  `json:"entity_number"`
	PayloadJSON  string `json:"payload_json"`
	Signature    string `json:"signature"`
	ReceivedAt   string `json:"received_at"`
}

type syncloAckRequest struct {
	DeliveryIDs  []string `json:"delivery_ids"`
	ConsumerName string   `json:"consumer_name"`
}

func (w *webhookRuntime) reconcileSyncloInbox(repoSet map[string]struct{}) {
	w.mu.Lock()
	w.status.SyncloInbox.Enabled = len(repoSet) > 0
	w.status.SyncloInbox.BaseURL = strings.TrimRight(strings.TrimSpace(w.cfg.Webhook.Synclo.BaseURL), "/")
	w.status.SyncloInbox.Consumer = strings.TrimSpace(w.cfg.Webhook.Synclo.Consumer)
	w.status.SyncloInbox.Limit = w.cfg.Webhook.Synclo.Limit
	if len(repoSet) == 0 {
		if w.syncloStarted && w.syncloStopCh != nil {
			close(w.syncloStopCh)
			w.syncloStopCh = nil
		}
		w.syncloStarted = false
		w.status.SyncloInbox.Running = false
		w.mu.Unlock()
		return
	}
	if w.syncloStarted {
		w.mu.Unlock()
		return
	}
	w.syncloStarted = true
	stopCh := make(chan struct{})
	w.syncloStopCh = stopCh
	w.status.SyncloInbox.Running = true
	w.mu.Unlock()

	w.wg.Add(1)
	go w.runSyncloInboxLoop(stopCh)
}

func (w *webhookRuntime) runSyncloInboxLoop(stopCh <-chan struct{}) {
	defer w.wg.Done()
	cfg := w.cfg.Webhook.Synclo
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	client := &http.Client{Timeout: syncloInboxHTTPTimeout}

	for {
		w.pollSyncloInbox(context.Background(), client, cfg)
		select {
		case <-w.stopCh:
			w.setSyncloRunning(false)
			return
		case <-stopCh:
			w.setSyncloRunning(false)
			return
		case <-time.After(interval):
		}
	}
}

func (w *webhookRuntime) pollSyncloInbox(ctx context.Context, client *http.Client, cfg config.SyncloWebhookConfig) {
	secret := os.Getenv(strings.TrimSpace(cfg.SecretEnv))
	if secret == "" {
		w.setSyncloError(fmt.Sprintf("synclo secret environment variable %s is empty", strings.TrimSpace(cfg.SecretEnv)))
		return
	}
	response, err := fetchSyncloPending(ctx, client, cfg, secret)
	if err != nil {
		w.setSyncloError(err.Error())
		return
	}
	w.recordSyncloPoll(len(response.Events))
	if len(response.Events) == 0 {
		w.clearSyncloError()
		return
	}
	forwarder := w.forwarder
	if forwarder == nil {
		w.setSyncloError("webhook forwarder is unavailable")
		return
	}
	target := forwarder()
	if target == nil {
		w.setSyncloError("webhook forwarder is unavailable")
		return
	}
	delivered := make([]string, 0, len(response.Events))
	for _, event := range response.Events {
		deliveryID := strings.TrimSpace(event.DeliveryID)
		eventType := strings.TrimSpace(event.EventType)
		if deliveryID == "" || eventType == "" {
			continue
		}
		result, err := target.Forward(ctx, webhookforward.DeliveryRequest{
			DeliveryID: deliveryID,
			EventType:  eventType,
			Payload:    []byte(event.PayloadJSON),
		})
		if err != nil {
			w.setSyncloError(fmt.Sprintf("forward synclo delivery %s: %v", deliveryID, err))
			continue
		}
		if w.logger != nil {
			w.logger.Info("webhook.synclo.forwarded", map[string]any{"deliveryId": deliveryID, "eventType": eventType, "status": result.Status, "workItems": result.WorkItems})
		}
		delivered = append(delivered, deliveryID)
		w.recordSyncloForwarded(1)
	}
	if len(delivered) == 0 {
		return
	}
	if err := ackSyncloDeliveries(ctx, client, cfg, secret, delivered); err != nil {
		w.setSyncloError(err.Error())
		return
	}
	w.recordSyncloAck(len(delivered))
	w.clearSyncloError()
}

func fetchSyncloPending(ctx context.Context, client *http.Client, cfg config.SyncloWebhookConfig, secret string) (syncloPendingResponse, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return syncloPendingResponse{}, fmt.Errorf("synclo base URL is empty")
	}
	pendingURL, err := url.Parse(baseURL + "/webhook/pending")
	if err != nil {
		return syncloPendingResponse{}, fmt.Errorf("parse synclo pending URL: %w", err)
	}
	query := pendingURL.Query()
	query.Set("consumer", strings.TrimSpace(cfg.Consumer))
	query.Set("limit", fmt.Sprintf("%d", cfg.Limit))
	pendingURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pendingURL.String(), nil)
	if err != nil {
		return syncloPendingResponse{}, err
	}
	request.Header.Set("X-Signature", syncloSignature(secret, "GET "+pendingURL.RequestURI()))
	response, err := client.Do(request)
	if err != nil {
		return syncloPendingResponse{}, fmt.Errorf("fetch synclo pending: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxWebhookTunnelPayloadBytes))
	if err != nil {
		return syncloPendingResponse{}, fmt.Errorf("read synclo pending response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return syncloPendingResponse{}, fmt.Errorf("fetch synclo pending: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded syncloPendingResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return syncloPendingResponse{}, fmt.Errorf("decode synclo pending response: %w", err)
	}
	if !decoded.OK {
		return syncloPendingResponse{}, fmt.Errorf("synclo pending response ok=false")
	}
	return decoded, nil
}

func ackSyncloDeliveries(ctx context.Context, client *http.Client, cfg config.SyncloWebhookConfig, secret string, deliveryIDs []string) error {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	ackURL := baseURL + "/webhook/ack"
	body, err := json.Marshal(syncloAckRequest{DeliveryIDs: deliveryIDs, ConsumerName: strings.TrimSpace(cfg.Consumer)})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ackURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Signature", syncloSignature(secret, string(body)))
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("ack synclo deliveries: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxWebhookTunnelPayloadBytes))
	if err != nil {
		return fmt.Errorf("read synclo ack response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("ack synclo deliveries: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func syncloSignature(secret string, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func (w *webhookRuntime) setSyncloRunning(running bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.SyncloInbox.Running = running
}

func (w *webhookRuntime) setSyncloError(message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.SyncloInbox.LastError = strings.TrimSpace(message)
	w.status.Degraded = true
	w.status.DegradedReasons = upsertReason(w.status.DegradedReasons, "synclo inbox degraded: "+w.status.SyncloInbox.LastError)
}

func (w *webhookRuntime) clearSyncloError() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.SyncloInbox.LastError = ""
	w.status.DegradedReasons = filterReasons(w.status.DegradedReasons, func(reason string) bool {
		return !strings.HasPrefix(reason, "synclo inbox degraded:")
	})
	w.status.Degraded = len(w.status.DegradedReasons) > 0
}

func (w *webhookRuntime) recordSyncloPoll(count int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.SyncloInbox.LastPollAt = formatJavaScriptISOString(w.currentTime().UTC())
	w.status.SyncloInbox.PendingFetched += count
}

func (w *webhookRuntime) recordSyncloForwarded(count int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.SyncloInbox.Forwarded += count
}

func (w *webhookRuntime) recordSyncloAck(count int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.SyncloInbox.LastAckAt = formatJavaScriptISOString(w.currentTime().UTC())
	w.status.SyncloInbox.Acked += count
}

func upsertReason(reasons []string, reason string) []string {
	filtered := filterReasons(reasons, func(existing string) bool {
		return !strings.HasPrefix(existing, "synclo inbox degraded:")
	})
	return append(filtered, reason)
}

func filterReasons(reasons []string, keep func(string) bool) []string {
	filtered := reasons[:0]
	for _, reason := range reasons {
		if keep(reason) {
			filtered = append(filtered, reason)
		}
	}
	return filtered
}

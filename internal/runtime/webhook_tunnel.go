package runtime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/webhookforward"
)

const webhookTunnelDisableLatchThreshold = 3
const webhookTunnelDisableLatchWindow = 24 * time.Hour
const maxWebhookTunnelPayloadBytes = 1 << 20

type webhookTunnelGitHubHook struct {
	ID     int64    `json:"id"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
		InsecureSSL string `json:"insecure_ssl"`
	} `json:"config"`
	LastResponse *struct {
		Code int `json:"code"`
	} `json:"last_response"`
}

type webhookTunnelGitHubClient interface {
	GetHook(ctx context.Context, repo string, id int64) (webhookTunnelGitHubHook, bool, error)
	CreateHook(ctx context.Context, repo string, url string, secret string, events []string) (webhookTunnelGitHubHook, error)
	UpdateHook(ctx context.Context, repo string, id int64, url string, secret string, events []string, active bool) (webhookTunnelGitHubHook, error)
	DeleteHook(ctx context.Context, repo string, id int64) error
}

type ghWebhookTunnelClient struct{ ghPath string }

func (c ghWebhookTunnelClient) GetHook(ctx context.Context, repo string, id int64) (webhookTunnelGitHubHook, bool, error) {
	result, err := c.run(ctx, repo, []string{"api", fmt.Sprintf("repos/%s/hooks/%d", splitRepoPath(repo), id)})
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "Not Found") {
			return webhookTunnelGitHubHook{}, false, nil
		}
		return webhookTunnelGitHubHook{}, false, err
	}
	var hook webhookTunnelGitHubHook
	if err := json.Unmarshal(result, &hook); err != nil {
		return webhookTunnelGitHubHook{}, false, fmt.Errorf("decode hook %s#%d: %w", repo, id, err)
	}
	return hook, true, nil
}

func (c ghWebhookTunnelClient) CreateHook(ctx context.Context, repo string, url string, secret string, events []string) (webhookTunnelGitHubHook, error) {
	body := webhookHookMutationBody(url, secret, events, true)
	return c.mutateWithInput(ctx, repo, 0, []string{"api", "-X", "POST", fmt.Sprintf("repos/%s/hooks", splitRepoPath(repo))}, body)
}

func (c ghWebhookTunnelClient) UpdateHook(ctx context.Context, repo string, id int64, url string, secret string, events []string, active bool) (webhookTunnelGitHubHook, error) {
	body := webhookHookMutationBody(url, secret, events, active)
	return c.mutateWithInput(ctx, repo, id, []string{"api", "-X", "PATCH", fmt.Sprintf("repos/%s/hooks/%d", splitRepoPath(repo), id)}, body)
}

func (c ghWebhookTunnelClient) DeleteHook(ctx context.Context, repo string, id int64) error {
	_, err := c.run(ctx, repo, []string{"api", "-X", "DELETE", fmt.Sprintf("repos/%s/hooks/%d", splitRepoPath(repo), id)})
	return err
}

func (c ghWebhookTunnelClient) mutate(ctx context.Context, repo string, id int64, args []string) (webhookTunnelGitHubHook, error) {
	result, err := c.run(ctx, repo, args)
	if err != nil {
		return webhookTunnelGitHubHook{}, err
	}
	var hook webhookTunnelGitHubHook
	if err := json.Unmarshal(result, &hook); err != nil {
		return webhookTunnelGitHubHook{}, fmt.Errorf("decode hook mutation for %s#%d: %w", repo, id, err)
	}
	return hook, nil
}

func (c ghWebhookTunnelClient) mutateWithInput(ctx context.Context, repo string, id int64, args []string, body []byte) (webhookTunnelGitHubHook, error) {
	tmp, err := os.CreateTemp("", "looper-webhook-*.json")
	if err != nil {
		return webhookTunnelGitHubHook{}, err
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return webhookTunnelGitHubHook{}, err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return webhookTunnelGitHubHook{}, err
	}
	if err := tmp.Close(); err != nil {
		return webhookTunnelGitHubHook{}, err
	}
	args = append(args, "--input", path)
	return c.mutate(ctx, repo, id, args)
}

func webhookHookMutationBody(url string, secret string, events []string, active bool) []byte {
	hookConfig := map[string]string{"url": url, "content_type": "json", "insecure_ssl": "0"}
	if secret != "" {
		hookConfig["secret"] = secret
	}
	body := map[string]any{"name": "web", "active": active, "events": canonicalWebhookEvents(events), "config": hookConfig}
	encoded, _ := json.Marshal(body)
	return encoded
}

func (c ghWebhookTunnelClient) run(ctx context.Context, repo string, args []string) ([]byte, error) {
	if strings.TrimSpace(c.ghPath) == "" {
		return nil, errors.New("gh is not configured or could not be resolved")
	}
	if host, _ := splitTunnelRepoHostname(repo); host != "" {
		args = append(args, "--hostname", host)
	}
	cmd := exec.CommandContext(ctx, c.ghPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(strings.Join([]string{stderr.String(), stdout.String()}, "\n"))
		if out == "" {
			out = err.Error()
		}
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), out)
	}
	return stdout.Bytes(), nil
}

func (w *webhookRuntime) reconcileTunnelHooks(ctx context.Context, repos *storage.Repositories, repoSet map[string]struct{}) error {
	w.setAllowedTunnelRepos(repoSet)
	if repos == nil || repos.WebhookTunnelHooks == nil {
		w.addDegradedReason("webhook tunnel hook store is unavailable")
		return nil
	}
	w.mu.Lock()
	w.tunnelStore = repos.WebhookTunnelHooks
	w.mu.Unlock()
	if len(repoSet) > 0 {
		if err := w.ensureTunnelServer(); err != nil {
			listenerErr := fmt.Errorf("webhook tunnel listener failed: %w", err)
			w.addDegradedReason(listenerErr.Error())
			return listenerErr
		}
	} else {
		w.stopTunnelServer()
	}
	w.clearTunnelDegradedReasons()
	if len(repoSet) > 0 && w.ghPath == "" {
		w.addDegradedReason("webhook tunnel hooks require gh to create or reconcile repository webhooks")
		return nil
	}
	store := repos.WebhookTunnelHooks
	existing, err := store.List(ctx)
	if err != nil {
		w.addDegradedReason(fmt.Sprintf("list webhook tunnel hooks: %v", err))
		return nil
	}
	now := w.currentTime().UnixNano()
	states := make([]WebhookTunnelState, 0, len(repoSet)+len(existing))
	for _, record := range existing {
		_, desired := repoSet[record.Repo]
		if !desired && !record.Orphaned {
			if err := store.MarkOrphaned(ctx, record.Repo, true, now); err != nil {
				states = append(states, WebhookTunnelState{Repo: record.Repo, HookID: &record.HookID, ManagedURL: record.ManagedURL, LastError: fmt.Sprintf("mark hook orphaned: %v", err)})
				continue
			}
			record.Orphaned = true
		}
		if record.Orphaned && !desired {
			states = append(states, tunnelStateFromRecord(record, ""))
		}
	}
	for repo := range repoSet {
		if strings.Count(repo, "/") != 1 {
			states = append(states, WebhookTunnelState{Repo: repo, LastError: "tunnel mode does not support host-qualified repo names"})
			continue
		}
		record, ok, err := store.Get(ctx, repo)
		if err != nil {
			states = append(states, WebhookTunnelState{Repo: repo, LastError: fmt.Sprintf("load tunnel hook record: %v", err)})
			continue
		}
		repoCtx, cancel := context.WithTimeout(ctx, w.shutdownTimeout())
		state := w.reconcileTunnelHook(repoCtx, store, repo, record, ok, now)
		cancel()
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Repo < states[j].Repo })
	w.setTunnelStates(states)
	w.updateTunnelDegradedReasons(states)
	return nil
}

func (w *webhookRuntime) reconcileTunnelHook(ctx context.Context, store *storage.WebhookTunnelHooksRepository, repo string, record storage.WebhookTunnelHookRecord, ok bool, now int64) WebhookTunnelState {
	client := w.tunnelGitHubClient()
	url := webhookTunnelManagedURL(w.cfg, repo)
	secretRef := webhookTunnelSecretRef(repo)
	if !ok || record.HookID == 0 {
		secret, err := ensureWebhookTunnelSecret(w.cfg.Storage.DBPath, secretRef)
		if err != nil {
			return WebhookTunnelState{Repo: repo, ManagedURL: url, LastError: err.Error()}
		}
		hook, err := client.CreateHook(ctx, repo, url, secret, webhookForwardEvents)
		if err != nil {
			return WebhookTunnelState{Repo: repo, ManagedURL: url, LastError: err.Error()}
		}
		record = storage.WebhookTunnelHookRecord{Repo: repo, HookID: hook.ID, ManagedURL: url, SecretRef: secretRef, ConsecutiveDisables: 0, CreatedAt: now, UpdatedAt: now}
		if err := store.Upsert(ctx, record); err != nil {
			if deleteErr := client.DeleteHook(ctx, repo, hook.ID); deleteErr != nil {
				return WebhookTunnelState{Repo: repo, HookID: &record.HookID, ManagedURL: url, LastError: fmt.Sprintf("persist tunnel hook: %v; rollback delete of remote hook %d failed: %v", err, hook.ID, deleteErr)}
			}
			return WebhookTunnelState{Repo: repo, HookID: &record.HookID, ManagedURL: url, LastError: fmt.Sprintf("persist tunnel hook: %v", err)}
		}
		return tunnelStateFromRecord(record, "")
	}
	secret, err := readWebhookTunnelSecret(w.cfg.Storage.DBPath, record.SecretRef)
	if err != nil {
		return tunnelStateFromRecord(record, fmt.Sprintf("read webhook secret: %v", err))
	}
	hook, found, err := client.GetHook(ctx, repo, record.HookID)
	if err != nil {
		return tunnelStateFromRecord(record, fmt.Sprintf("get hook by id: %v", err))
	}
	if !found {
		hook, err := client.CreateHook(ctx, repo, url, secret, webhookForwardEvents)
		if err != nil {
			return tunnelStateFromRecord(record, fmt.Sprintf("recreate missing hook: %v", err))
		}
		record.HookID = hook.ID
		record.ManagedURL = url
		record.SecretRef = secretRef
		record.ConsecutiveDisables = 0
		record.LastDisableAt = nil
		record.Orphaned = false
		record.UpdatedAt = now
		if err := store.Upsert(ctx, record); err != nil {
			if deleteErr := client.DeleteHook(ctx, repo, hook.ID); deleteErr != nil {
				return tunnelStateFromRecord(record, fmt.Sprintf("persist recreated hook: %v; rollback delete of remote hook %d failed: %v", err, hook.ID, deleteErr))
			}
			return tunnelStateFromRecord(record, fmt.Sprintf("persist recreated hook: %v", err))
		}
		return tunnelStateFromRecord(record, "")
	}
	if strings.TrimSpace(hook.Config.URL) != "" && strings.TrimSpace(hook.Config.URL) != strings.TrimSpace(record.ManagedURL) && strings.TrimSpace(hook.Config.URL) != url {
		_ = store.MarkOrphaned(ctx, repo, true, now)
		record.Orphaned = true
		return tunnelStateFromRecord(record, "remote hook URL drifted; record marked orphaned and not mutated")
	}
	reactivateOrphan := record.Orphaned
	if reactivateOrphan {
		record.Orphaned = false
	}
	refreshManagedRecord := strings.TrimSpace(record.ManagedURL) != url || record.SecretRef != secretRef
	if record.LastDisableAt != nil && w.currentTime().Sub(time.Unix(0, *record.LastDisableAt)) > webhookTunnelDisableLatchWindow {
		record.ConsecutiveDisables = 0
		record.LastDisableAt = nil
	}
	if record.ConsecutiveDisables >= webhookTunnelDisableLatchThreshold && !hook.Active {
		return tunnelStateLatched(record, "remote hook disabled repeatedly; not re-enabling")
	}
	needPatch := !hook.Active || strings.TrimSpace(hook.Config.URL) != url || !sameWebhookEvents(hook.Events, webhookForwardEvents) || !strings.EqualFold(strings.TrimSpace(hook.Config.ContentType), "json") || strings.TrimSpace(hook.Config.InsecureSSL) != "0"
	if needPatch {
		if !hook.Active {
			record.ConsecutiveDisables++
			last := now
			record.LastDisableAt = &last
			if record.ConsecutiveDisables >= webhookTunnelDisableLatchThreshold {
				record.UpdatedAt = now
				if err := store.Upsert(ctx, record); err != nil {
					return tunnelStateFromRecord(record, fmt.Sprintf("persist latch state: %v", err))
				}
				return tunnelStateLatched(record, "remote hook disabled repeatedly; not re-enabling")
			}
		}
		_, err := client.UpdateHook(ctx, repo, record.HookID, url, secret, webhookForwardEvents, true)
		if err != nil {
			return tunnelStateFromRecord(record, fmt.Sprintf("update hook by id: %v", err))
		}
		record.ManagedURL = url
		record.SecretRef = secretRef
		record.Orphaned = false
		record.UpdatedAt = now
		if err := store.Upsert(ctx, record); err != nil {
			return tunnelStateFromRecord(record, fmt.Sprintf("persist hook update: %v", err))
		}
	} else if reactivateOrphan || refreshManagedRecord {
		record.ManagedURL = url
		record.SecretRef = secretRef
		record.UpdatedAt = now
		if err := store.Upsert(ctx, record); err != nil {
			return tunnelStateFromRecord(record, fmt.Sprintf("persist hook state: %v", err))
		}
	}
	return tunnelStateFromRecord(record, "")
}

func (w *webhookRuntime) ensureTunnelServer() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tunnelServer != nil {
		return nil
	}
	server := &webhookTunnelServer{runtime: w}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(w.cfg.Webhook.ListenPort))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server.server = &http.Server{Handler: server}
	w.tunnelServer = server
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		if err := server.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && w.logger != nil {
			w.logger.Warn("webhook.tunnel.listener_exited", map[string]any{"error": err.Error()})
		}
	}()
	return nil
}

func (w *webhookRuntime) stopTunnelServer() {
	w.mu.Lock()
	server := w.tunnelServer
	w.tunnelServer = nil
	w.mu.Unlock()
	if server != nil && server.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), w.shutdownTimeout())
		defer cancel()
		_ = server.server.Shutdown(ctx)
	}
}

type webhookTunnelServer struct {
	runtime *webhookRuntime
	server  *http.Server
}

func (s *webhookTunnelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	path, ok := webhookTunnelRequestPath(s.runtime.cfg, r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	repo, ok := repoFromWebhookTunnelPath(path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !s.runtime.isAllowedTunnelRepo(repo) {
		http.NotFound(w, r)
		return
	}
	store := s.runtime.currentTunnelStore()
	if store == nil {
		http.Error(w, "webhook tunnel store unavailable", http.StatusServiceUnavailable)
		return
	}
	record, found, err := store.Get(r.Context(), repo)
	if err != nil || !found || record.Orphaned {
		http.NotFound(w, r)
		return
	}
	secret, err := readWebhookTunnelSecret(s.runtime.cfg.Storage.DBPath, record.SecretRef)
	if err != nil {
		http.Error(w, "webhook secret unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookTunnelPayloadBytes))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !validGitHubSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		if s.runtime.logger != nil {
			s.runtime.logger.Warn("webhook.tunnel.signature_failed", map[string]any{"repo": repo, "delivery_id": r.Header.Get("X-GitHub-Delivery")})
		}
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if strings.EqualFold(r.Header.Get("X-GitHub-Event"), "ping") {
		_ = store.UpdatePing(r.Context(), repo, s.runtime.currentTime().UnixNano())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	payloadRepo := githubPayloadRepo(body)
	if payloadRepo != "" && !strings.EqualFold(payloadRepo, repo) {
		http.Error(w, "repository mismatch", http.StatusBadRequest)
		return
	}
	forwarder := s.runtime.currentForwarder()
	if forwarder == nil {
		http.Error(w, "webhook forwarder unavailable", http.StatusServiceUnavailable)
		return
	}
	result, err := forwarder.Forward(r.Context(), webhookforward.DeliveryRequest{DeliveryID: r.Header.Get("X-GitHub-Delivery"), EventType: r.Header.Get("X-GitHub-Event"), Payload: body})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.EqualFold(result.Status, "accepted") || result.WorkItems > 0 {
		s.runtime.RecordDelivery(r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery"))
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(result)
}

func (w *webhookRuntime) currentForwarder() WebhookForwarder {
	if w == nil || w.forwarder == nil {
		return nil
	}
	return w.forwarder()
}

func (w *webhookRuntime) currentTunnelStore() *storage.WebhookTunnelHooksRepository {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.tunnelStore
}

func (w *webhookRuntime) setAllowedTunnelRepos(repos map[string]struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(repos) == 0 {
		w.allowedTunnelRepos = map[string]struct{}{}
		return
	}
	allowed := make(map[string]struct{}, len(repos))
	for repo := range repos {
		allowed[repo] = struct{}{}
	}
	w.allowedTunnelRepos = allowed
}

func (w *webhookRuntime) isAllowedTunnelRepo(repo string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, allowed := w.allowedTunnelRepos[repo]
	return allowed
}

func (w *webhookRuntime) tunnelGitHubClient() webhookTunnelGitHubClient {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tunnelClient == nil {
		w.tunnelClient = ghWebhookTunnelClient{ghPath: w.ghPath}
	}
	return w.tunnelClient
}

func (w *webhookRuntime) setTunnelStates(states []WebhookTunnelState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.TunnelHooks = append([]WebhookTunnelState{}, states...)
}

func (w *webhookRuntime) clearTunnelDegradedReasons() {
	w.clearDegradedReasons(func(reason string) bool {
		return strings.HasPrefix(reason, "tunnel hook for ") || strings.HasPrefix(reason, "webhook tunnel ")
	})
}

func (w *webhookRuntime) updateTunnelDegradedReasons(states []WebhookTunnelState) {
	for _, state := range states {
		if state.Orphaned || (state.LastError == "" && !state.Latched) {
			continue
		}
		reason := state.LastError
		if reason == "" && state.Latched {
			reason = "latched"
		}
		w.addDegradedReason(fmt.Sprintf("tunnel hook for %s degraded: %s; polling fallback continues every %d seconds", state.Repo, reason, w.status.FallbackPollIntervalSeconds))
	}
}

func (w *webhookRuntime) configuredWebhookReposForMode(projects []storage.ProjectRecord, mode config.WebhookMode) []string {
	seen := map[string]struct{}{}
	repos := make([]string, 0, len(projects))
	for _, project := range projects {
		if project.Archived || w.webhookModeForProject(project.ID) != mode {
			continue
		}
		repo := repoFromProjectMetadata(project.MetadataJSON)
		if repo == "" {
			continue
		}
		if _, ok := seen[repo]; ok {
			continue
		}
		seen[repo] = struct{}{}
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

func (w *webhookRuntime) webhookModeForProject(projectID string) config.WebhookMode {
	mode := w.cfg.Webhook.Mode
	for _, project := range w.cfg.Projects {
		if project.ID == projectID && project.Webhook.Mode != "" {
			mode = project.Webhook.Mode
			break
		}
	}
	if mode == "" {
		return config.WebhookModeGHForward
	}
	return mode
}

func wModeNeedsGHForward(cfg config.Config) bool {
	if cfg.Webhook.Mode == config.WebhookModeGHForward || cfg.Webhook.Mode == "" {
		return true
	}
	for _, project := range cfg.Projects {
		if project.Webhook.Mode == config.WebhookModeGHForward {
			return true
		}
	}
	return false
}

func wModeNeedsTunnel(cfg config.Config) bool {
	if cfg.Webhook.Mode == config.WebhookModeTunnel {
		return true
	}
	for _, project := range cfg.Projects {
		if project.Webhook.Mode == config.WebhookModeTunnel {
			return true
		}
	}
	return false
}

func configuredTunnelProjectIDs(cfg config.Config) []string {
	ids := make([]string, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		mode := cfg.Webhook.Mode
		if project.Webhook.Mode != "" {
			mode = project.Webhook.Mode
		}
		if mode == "" {
			mode = config.WebhookModeGHForward
		}
		if mode != config.WebhookModeTunnel {
			continue
		}
		ids = append(ids, project.ID)
	}
	sort.Strings(ids)
	return ids
}

func webhookTunnelListenerURL(cfg config.Config) string {
	if cfg.Webhook.ListenPort <= 0 {
		return ""
	}
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Webhook.ListenPort))
}

func webhookTunnelManagedURL(cfg config.Config, repo string) string {
	return strings.TrimRight(strings.TrimSpace(cfg.Webhook.PublicBaseURL), "/") + "/webhook/" + strings.Trim(strings.TrimSpace(repo), "/")
}

func webhookTunnelRequestPath(cfg config.Config, requestPath string) (string, bool) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.Webhook.PublicBaseURL), "/")
	if baseURL == "" {
		return requestPath, true
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	basePath := strings.TrimRight(strings.TrimSpace(parsed.Path), "/")
	if basePath == "" {
		return requestPath, true
	}
	trimmed, ok := strings.CutPrefix(requestPath, basePath)
	if !ok || trimmed == "" {
		return "", false
	}
	return trimmed, true
}

func repoFromWebhookTunnelPath(path string) (string, bool) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[0] != "webhook" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return parts[1] + "/" + parts[2], true
}

func githubPayloadRepo(payload []byte) string {
	var parsed struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Repository.FullName)
}

func validGitHubSignature(secret string, body []byte, signature string) bool {
	signature = strings.TrimSpace(signature)
	if !strings.HasPrefix(signature, "sha256=") || secret == "" {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

func webhookTunnelSecretRef(repo string) string {
	replacer := strings.NewReplacer("/", "_", ":", "_", "\\", "_")
	return "webhook_" + replacer.Replace(strings.TrimSpace(repo)) + ".key"
}

func ensureWebhookTunnelSecret(dbPath, ref string) (string, error) {
	secret, err := readWebhookTunnelSecret(dbPath, ref)
	if err == nil && secret != "" {
		return secret, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	secret = hex.EncodeToString(buf[:])
	path := webhookTunnelSecretPath(dbPath, ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		return "", err
	}
	return secret, nil
}

func readWebhookTunnelSecret(dbPath, ref string) (string, error) {
	path := webhookTunnelSecretPath(dbPath, ref)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("webhook secret %s mode must not be wider than 0600", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("webhook secret %s is empty", path)
	}
	return secret, nil
}

func webhookTunnelSecretPath(dbPath, ref string) string {
	dir := filepath.Dir(strings.TrimSpace(dbPath))
	if dir == "." || dir == "" || strings.HasPrefix(dbPath, "file:") || dbPath == ":memory:" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".looper")
		}
	}
	return filepath.Join(dir, "secrets", filepath.Base(ref))
}

func tunnelStateFromRecord(record storage.WebhookTunnelHookRecord, lastErr string) WebhookTunnelState {
	hookID := record.HookID
	var lastPing *string
	if record.LastPingAt != nil {
		formatted := formatJavaScriptISOString(time.Unix(0, *record.LastPingAt).UTC())
		lastPing = &formatted
	}
	return WebhookTunnelState{Repo: record.Repo, HookID: &hookID, ManagedURL: record.ManagedURL, LastPingAt: lastPing, ConsecutiveDisables: record.ConsecutiveDisables, Orphaned: record.Orphaned, LastError: lastErr}
}

func tunnelStateLatched(record storage.WebhookTunnelHookRecord, lastErr string) WebhookTunnelState {
	state := tunnelStateFromRecord(record, lastErr)
	state.Latched = true
	return state
}

func sameWebhookEvents(a, b []string) bool {
	aa := canonicalWebhookEvents(a)
	bb := canonicalWebhookEvents(b)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func splitRepoPath(repo string) string {
	_, path := splitTunnelRepoHostname(repo)
	return path
}

func splitTunnelRepoHostname(repo string) (string, string) {
	repo = strings.TrimSpace(repo)
	if strings.Count(repo, "/") >= 2 {
		parts := strings.SplitN(repo, "/", 2)
		if strings.Contains(parts[0], ".") {
			return parts[0], parts[1]
		}
	}
	return "", repo
}

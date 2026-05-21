package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/network/protocol"
)

type Server struct {
	httpServer *http.Server
	service    *Service
	config     Config
}

func NewServer(cfg Config, service *Service) *Server {
	mux := http.NewServeMux()
	s := &Server{service: service, config: cfg}
	mux.HandleFunc("/healthz", s.adminOnly(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, map[string]any{"ok": true}) }))
	mux.HandleFunc("/status", s.adminOnly(s.handleStatus))
	mux.HandleFunc("/v1/join-keys", s.adminOnly(s.handleJoinKey))
	mux.HandleFunc("/v1/join", s.handleJoin)
	mux.HandleFunc("/v1/status", s.nodeOnly(s.handleNodeStatus))
	mux.HandleFunc("/v1/heartbeat", s.nodeOnly(s.handleHeartbeat))
	mux.HandleFunc("/v1/leave", s.nodeOnly(s.handleLeave))
	mux.HandleFunc("/v1/coordinator-lease/acquire", s.nodeOnly(s.handleAcquireLease))
	mux.HandleFunc("/v1/coordinator-lease/renew", s.nodeOnly(s.handleRenewLease))
	mux.HandleFunc("/v1/coordinator-lease/handoff", s.nodeOnly(s.handleHandoffLease))
	mux.HandleFunc("/v1/coordinator-lease/expire", s.nodeOnly(s.handleExpireLease))
	mux.HandleFunc("/v1/coordinator-lease/revalidate", s.nodeOnly(s.handleRevalidateLease))
	mux.HandleFunc("/v1/events", s.nodeOnly(s.handleEvents))
	mux.HandleFunc("/v1/github/webhook-secret", s.adminOnly(s.handleGitHubWebhookSecret))
	mux.HandleFunc("/v1/github/webhook", s.handleGitHubWebhook)
	s.httpServer = &http.Server{Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	return s
}

func (s *Server) Start() error                       { return s.httpServer.ListenAndServe() }
func (s *Server) Shutdown(ctx context.Context) error { return s.httpServer.Shutdown(ctx) }
func (s *Server) Handler() http.Handler              { return s.httpServer.Handler }

func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bearerToken(r) != s.config.AdminToken {
			writeError(w, http.StatusUnauthorized, "admin authorization token is required")
			return
		}
		next(w, r)
	}
}

func (s *Server) nodeOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bearerToken(r) == "" {
			writeError(w, http.StatusUnauthorized, "node authorization token is required")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.service.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleJoinKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	key, err := s.service.CreateJoinKey(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"joinKey": key})
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req protocol.JoinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.service.Join(r.Context(), req)
	if err != nil {
		writeCompatibilityError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req protocol.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.service.Heartbeat(r.Context(), bearerToken(r), req)
	if err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleNodeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	status, err := s.service.NodeStatus(r.Context(), bearerToken(r))
	if err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := s.service.Leave(r.Context(), bearerToken(r)); err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req protocol.CoordinatorLeaseAcquireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lease, err := s.service.AcquireLease(r.Context(), bearerToken(r), req.TTLSeconds)
	if err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) handleRenewLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req protocol.CoordinatorLeaseRenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lease, err := s.service.RenewLease(r.Context(), bearerToken(r), req.FencingToken, req.TTLSeconds)
	if err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) handleHandoffLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req protocol.CoordinatorLeaseHandoffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lease, err := s.service.HandoffLease(r.Context(), bearerToken(r), req.FencingToken, req.TargetNodeName, req.TTLSeconds)
	if err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) handleExpireLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		FencingToken int64 `json:"fencingToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lease, err := s.service.ExpireLease(r.Context(), bearerToken(r), req.FencingToken)
	if err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) handleRevalidateLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req protocol.CoordinatorLeaseRevalidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.service.RevalidateLease(r.Context(), bearerToken(r), req); err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	nodeToken := bearerToken(r)
	if _, err := s.service.NodeStatus(r.Context(), nodeToken); err != nil {
		writeLeaseOrAuthError(w, err)
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch, cancel := s.service.Subscribe()
		defer cancel()
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-ch:
				if _, err := s.service.NodeStatus(r.Context(), nodeToken); err != nil {
					return
				}
				payload, _ := json.Marshal(event)
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Event, payload)
				flusher.Flush()
			}
		}
	}
	writeError(w, http.StatusInternalServerError, "streaming is not supported")
}

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	secret, err := s.service.WebhookSecret(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validGitHubSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	repo := ""
	if repository, ok := payload["repository"].(map[string]any); ok {
		repo = strings.TrimSpace(asString(repository["full_name"]))
	}
	s.service.RecordWebhookDelivery(r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery"), repo)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleGitHubWebhookSecret(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	secret, err := s.service.WebhookSecret(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret})
}

func asString(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
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
	expected := mac.Sum(nil)
	return hmac.Equal(expected, provided)
}

func writeCompatibilityError(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "unsupported protocol version") || strings.Contains(err.Error(), "unsupported daemon version") || strings.Contains(err.Error(), protocol.MinimumDaemonField) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeLeaseOrAuthError(w, err)
}

func writeLeaseOrAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnauthorized) {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if strings.Contains(err.Error(), "stale coordinator lease token") {
		writeError(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

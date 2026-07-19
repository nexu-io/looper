package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/reviewer"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/version"
	"github.com/nexu-io/looper/internal/webhookforward"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

const (
	requestIDHeaderName        = "x-request-id"
	apiBasePath                = "/api/v1"
	webhookForwardPath         = "/webhook/forward"
	maxConfigPatchBodyBytes    = 1 << 20
	javaScriptISOString        = "2006-01-02T15:04:05.000Z"
	loopLogsFollowPollInterval = 200 * time.Millisecond
	activeRunHeartbeatTTL      = 30 * time.Minute
	webhookListenerPath        = "/webhook/forward"
)

var nonProjectIDPattern = regexp.MustCompile(`[^a-z0-9]+`)
var issueTargetPattern = regexp.MustCompile(`^issue:[^:]+\/[^:]+:(\d+)$`)

type RuntimeState interface {
	Services() looperdruntime.Services
	StartedAt() (time.Time, bool)
}

type activeRunExecutionVerifier interface {
	ExecutionMatchesProcess(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error)
}

// ConfigFieldMetadata describes where an effective field came from and whether
// the dashboard may change it without restarting looperd.
type ConfigFieldMetadata struct {
	Source    string `json:"source"`
	Editable  bool   `json:"editable"`
	ApplyMode string `json:"applyMode"`
}

// ConfigMetadata describes the effective configuration source and the most
// recent file reload attempt. Fields is keyed by canonical dotted config path.
type ConfigMetadata struct {
	ConfigPath    string                         `json:"configPath"`
	Format        string                         `json:"format"`
	FilePresent   bool                           `json:"filePresent"`
	Revision      string                         `json:"revision"`
	LastAttemptAt *time.Time                     `json:"lastAttemptAt,omitempty"`
	LastAppliedAt *time.Time                     `json:"lastAppliedAt,omitempty"`
	LastError     *string                        `json:"lastError,omitempty"`
	RejectedPaths []string                       `json:"rejectedPaths"`
	Fields        map[string]ConfigFieldMetadata `json:"fields"`
}

// ConfigPatchRequest is the dashboard's field-level mutation contract. Set
// values stay as raw JSON so the configuration authority performs all typing
// and validation; Unset removes values from the file layer.
type ConfigPatchRequest struct {
	Revision string                     `json:"revision"`
	Set      map[string]json.RawMessage `json:"set"`
	Unset    []string                   `json:"unset"`
}

type ConfigRequestErrorKind string

const (
	ConfigRequestErrorKindValidation  ConfigRequestErrorKind = "validation"
	ConfigRequestErrorKindConflict    ConfigRequestErrorKind = "conflict"
	ConfigRequestErrorKindUnsupported ConfigRequestErrorKind = "unsupported"
	ConfigRequestErrorKindTooLarge    ConfigRequestErrorKind = "too_large"
)

// ConfigPatchIssue identifies one rejected field-level mutation.
type ConfigPatchIssue struct {
	Path    string `json:"path,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ConfigRequestError lets the configuration authority report stable field
// issues while the HTTP layer owns status codes and envelope formatting.
type ConfigRequestError struct {
	Kind    ConfigRequestErrorKind
	Message string
	Issues  []ConfigPatchIssue
}

func (e ConfigRequestError) Error() string {
	return e.Message
}

type Context struct {
	Config             config.Config
	ConfigMetadata     func() ConfigMetadata
	ConfigSnapshot     func() (config.Config, ConfigMetadata)
	PatchConfig        func(context.Context, ConfigPatchRequest) error
	Runtime            RuntimeState
	WebhookForwarder   webhookforward.Forwarder
	ProjectsService    projectService
	Now                func() time.Time
	RecoverySummary    func() any
	ReconcileStaleRuns func(context.Context) (looperdruntime.StaleRunReconcileSummary, error)
	StopLoop           func(context.Context, string, string) (any, error)
	CloseLoop          func(context.Context, string, string) (any, error)
	StopAll            func(context.Context, string) (any, error)
	// TakeoverLoop parks a loop for interactive human takeover: stops the daemon's
	// in-flight run (session id preserved on disk) and transitions the loop to
	// human_takeover, returning what a human needs to resume the exact session.
	TakeoverLoop         func(context.Context, string, string) (TakeoverResult, error)
	RepairReviewer       func(context.Context, reviewer.RepairInput) (reviewer.RepairResult, error)
	TriggerSchedulerTick func()
}

// TakeoverResult is what a takeover yields: the native session id + worktree +
// vendor of the loop's last run, so the caller can hand a human the exact resume
// command.
type TakeoverResult struct {
	LoopID       string
	Vendor       string
	SessionID    string
	WorktreePath string
}

type Handler struct {
	context          Context
	now              func() time.Time
	recoverySummary  func() any
	webhookForwarder webhookforward.Forwarder
	bootstrap        *bootstrapCodes
	// discardBeforeGitHook is test-only: invoked after discard preflight recheck
	// and immediately before git reset/clean so tests can inject a requeue race
	// that bypasses LockLoopRequeue (defense-in-depth for the pre-git recheck).
	discardBeforeGitHook func(loopID string)
	// retryAfterClearStopGateHook is test-only: invoked after ClearLoopStop and
	// before the requeue transaction so tests can inject a TX-time conflict
	// after the sticky stop gate was already cleared.
	retryAfterClearStopGateHook func(loopID string)
}

// effectiveConfig returns the live config when ConfigSnapshot is wired, else
// the request/handler context config. Agent gates (create/start/retry) must use
// this so role bindings reloaded after daemon start are visible.
func (h *Handler) effectiveConfig() config.Config {
	if h.context.ConfigSnapshot != nil {
		cfg, _ := h.context.ConfigSnapshot()
		return cfg
	}
	return h.context.Config
}

func NewHandler(context Context) *Handler {
	now := context.Now
	if now == nil {
		now = time.Now
	}

	recoverySummary := context.RecoverySummary
	if recoverySummary == nil {
		if runtimeWithRecovery, ok := any(context.Runtime).(interface {
			RecoverySummary() looperdruntime.RecoverySummary
		}); ok {
			recoverySummary = func() any {
				return normalizeRecoverySummary(runtimeWithRecovery.RecoverySummary())
			}
		} else {
			recoverySummary = func() any {
				return map[string]any{}
			}
		}
	}
	forwarder := context.WebhookForwarder
	if forwarder == nil {
		if runtimeWithForwarder, ok := any(context.Runtime).(interface {
			WebhookForwarder() looperdruntime.WebhookForwarder
		}); ok {
			forwarder = runtimeWithForwarder.WebhookForwarder()
		}
	}

	bootstrap := newBootstrapCodes()
	bootstrap.now = now

	return &Handler{
		context:          context,
		now:              now,
		recoverySummary:  recoverySummary,
		webhookForwarder: forwarder,
		bootstrap:        bootstrap,
	}
}

// lockLoopRetry acquires the process-wide per-loop requeue mutex shared by
// retryLoop, start/requeue (mutateLoopStatus → Running), issue-worker reuse
// (POST /workers), and runtime HITL free-text / answer requeues
// (looperdruntime.LockLoopRequeue). Without this shared exclusion, runtime
// inbox delivery can requeue after discard preflight and before git reset,
// wiping the worktree for the message-driven continuation when the retry TX
// then conflicts.
func (h *Handler) lockLoopRetry(loopID string) func() {
	return looperdruntime.LockLoopRequeue(loopID)
}

// lockLoopTarget acquires the process-wide same-target mutex so discard retry,
// same-target requeue (regular retry / start), active loop creation, and runtime
// HITL requeues cannot race. Concurrent project-scoped workers are exempt
// (assertUniqueActiveLoopCompat allows them). Pull-request targets omit loop
// type so fixer/reviewer/worker share one key for the shared PR worktree.
// Call order with lockLoopRetry: take the per-loop lock first, then the target
// lock, so retry/start/reuse paths share a consistent order with discard.
func (h *Handler) lockLoopTarget(projectID string, loopType domain.LoopType, target domain.LoopTarget) func() {
	key := looperdruntime.LoopTargetGuardKey(projectID, string(loopType), string(target.TargetType), loopTargetKeyCompat(target))
	return looperdruntime.LockLoopTarget(key)
}

// lockLoopTargetForStatus is a no-op when the candidate status is not a
// uniqueness-conflicting active status (create of failed/completed/etc.).
func (h *Handler) lockLoopTargetForStatus(projectID string, loopType domain.LoopType, target domain.LoopTarget, status domain.LoopStatus) func() {
	if !domain.IsConflictingActiveLoopStatus(status) {
		return func() {}
	}
	return h.lockLoopTarget(projectID, loopType, target)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestHandler := *h
	requestHandler.context = h.context
	// The config endpoint needs one coherent config-and-metadata snapshot.
	// Other routes use the current in-memory runtime config for authorization
	// and policy without constructing dashboard metadata.
	if normalizePath(r.URL.Path) == apiBasePath+"/config" && h.context.ConfigSnapshot != nil {
		cfg, metadata := h.context.ConfigSnapshot()
		requestHandler.context.Config = cfg
		requestHandler.context.ConfigMetadata = func() ConfigMetadata { return metadata }
	} else if runtimeConfig, ok := any(h.context.Runtime).(interface{ Config() config.Config }); ok {
		requestHandler.context.Config = runtimeConfig.Config()
	}
	requestHandler.serveHTTP(w, r)
}

func (h *Handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := normalizePath(r.URL.Path)
	requestID := strings.TrimSpace(r.Header.Get(requestIDHeaderName))
	if requestID == "" {
		requestID = generateRequestID()
	}
	if path == apiBasePath+"/config" {
		w.Header().Set("Cache-Control", "no-store")
	}

	if err := authorizeRequest(r, path, h.context.Config); err != nil {
		var typed apiError
		if !asAPIError(err, &typed) {
			typed = internalServerError(err)
		}
		// Bootstrap paths must never be cached, including auth/Host/Origin failures.
		if isDashboardBootstrapPath(path) {
			w.Header().Set("Cache-Control", "no-store")
		}
		h.writeError(w, requestID, typed)
		return
	}

	// Mutation readiness is a projection of the single admission Authority
	// (ADR-0015 / #575). Reads remain available while starting/stopping/degraded.
	// Feishu url_verification is a non-mutating challenge echo and is gated
	// inside handleFeishuCardActionRoute after the handshake branch.
	if isMutatingHTTPMethod(r.Method) && !isAdmissionExemptMutationPath(path) && path != apiBasePath+"/hitl/feishu" {
		if typed, denied := h.admissionMutationDenial(); denied {
			h.writeError(w, requestID, typed)
			return
		}
	}

	switch path {
	case dashboardBootstrapCodePath:
		h.handleBootstrapMint(w, r, requestID)
		return
	case dashboardBootstrapExchangePath:
		h.handleBootstrapExchange(w, r, requestID)
		return
	case webhookForwardPath:
		payload, err := h.buildWebhookForwardResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}
		h.writeJSON(w, http.StatusAccepted, pkgapi.Success(requestID, payload))
		return
	case apiBasePath + "/healthz":
		if !assertMethod(r.Method, http.MethodGet, path, w, requestID, h.writeError) {
			return
		}

		payload, err := h.buildHealthResponse(r.Context())
		if err != nil {
			h.writeError(w, requestID, internalServerError(err))
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/status":
		if !assertMethod(r.Method, http.MethodGet, path, w, requestID, h.writeError) {
			return
		}

		payload, err := h.buildStatusResponse(r.Context())
		if err != nil {
			h.writeError(w, requestID, internalServerError(err))
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/version":
		if !assertMethod(r.Method, http.MethodGet, path, w, requestID, h.writeError) {
			return
		}

		h.writeSuccess(w, requestID, h.buildVersionResponse())
		return
	case apiBasePath + "/config":
		h.handleConfigRoute(w, r, requestID)
		return
	case apiBasePath + "/webhook/status":
		if !assertMethod(r.Method, http.MethodGet, path, w, requestID, h.writeError) {
			return
		}

		h.writeSuccess(w, requestID, h.buildWebhookStatusResponse())
		return
	case apiBasePath + "/events":
		payload, err := h.buildEventsRouteResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/pull-requests":
		payload, err := h.buildPullRequestsRouteResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/projects":
		payload, err := h.buildProjectsRouteResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/loops":
		payload, err := h.buildLoopsRouteResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/workers":
		payload, err := h.buildWorkersCreateResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/planners":
		payload, err := h.buildPlannersCreateResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/runs":
		payload, err := h.buildRunsRouteResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/runs/reconcile-stale":
		payload, err := h.buildReconcileStaleRunsResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/runs/active":
		payload, err := h.buildActiveRunsResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	case apiBasePath + "/reviewer/repair":
		payload, err := h.buildReviewerRepairRouteResponse(r)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}
		h.writeSuccess(w, requestID, payload)
		return
	}

	if path == apiBasePath+"/hitl/feishu" {
		h.handleFeishuCardActionRoute(w, r, requestID)
		return
	}

	if strings.HasPrefix(path, apiBasePath+"/loops/") {
		if isFollowLoopLogsRequest(r, path) {
			if err := h.streamLoopLogsRoute(w, r, path, requestID); err != nil {
				var typed apiError
				if !asAPIError(err, &typed) {
					typed = internalServerError(err)
				}
				h.writeError(w, requestID, typed)
			}
			return
		}
		payload, err := h.buildLoopRouteResponse(r, path)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	}

	if strings.HasPrefix(path, apiBasePath+"/projects/") {
		payload, err := h.buildProjectRouteResponse(r, path)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	}

	if strings.HasPrefix(path, apiBasePath+"/events/") {
		payload, err := h.buildEntityEventsRouteResponse(r, path)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	}

	if strings.HasPrefix(path, apiBasePath+"/pull-requests/") {
		payload, err := h.buildPullRequestRouteResponse(r, path)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	}

	if strings.HasPrefix(path, apiBasePath+"/runs/active/") {
		payload, err := h.buildActiveRunRouteResponse(r, path)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	}

	if strings.HasPrefix(path, apiBasePath+"/runs/") {
		payload, err := h.buildRunRouteResponse(r, path)
		if err != nil {
			var typed apiError
			if !asAPIError(err, &typed) {
				typed = internalServerError(err)
			}
			h.writeError(w, requestID, typed)
			return
		}

		h.writeSuccess(w, requestID, payload)
		return
	}

	h.writeError(w, requestID, apiError{
		code:    pkgapi.ErrorCodeRouteNotFound,
		status:  http.StatusNotFound,
		message: fmt.Sprintf("Unknown route: %s", path),
	})
}

func (h *Handler) buildWebhookForwardResponse(r *http.Request) (webhookforward.ForwardResult, error) {
	if r.Method != http.MethodPost {
		return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: "Unsupported method for /webhook/forward"}
	}
	if !isLoopbackRequest(r) {
		return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeUnauthorized, status: http.StatusForbidden, message: "Webhook forwarding is limited to loopback callers"}
	}
	forwarder := h.webhookForwarder
	if runtimeWithForwarder, ok := any(h.context.Runtime).(interface {
		WebhookForwarder() looperdruntime.WebhookForwarder
	}); ok {
		if current := runtimeWithForwarder.WebhookForwarder(); current != nil {
			forwarder = current
		}
	}
	if forwarder == nil {
		return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Webhook forwarding is not configured"}
	}
	if runtimeWithWebhook, ok := any(h.context.Runtime).(interface {
		WebhookStatus() looperdruntime.WebhookStatus
	}); ok {
		status := runtimeWithWebhook.WebhookStatus()
		if !status.Enabled {
			return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusServiceUnavailable, message: "webhook runtime is disabled; deliveries are not being processed"}
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return webhookforward.ForwardResult{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	eventType := r.Header.Get("X-GitHub-Event")
	result, err := forwarder.Forward(r.Context(), webhookforward.DeliveryRequest{DeliveryID: deliveryID, EventType: eventType, Payload: body})
	if err != nil {
		status := http.StatusBadRequest
		code := pkgapi.ErrorCodeValidationFailed
		message := err.Error()
		// Post-gate admission refusal (race after outer AllowMutations) is temporary
		// unavailability, not a bad delivery payload.
		if errors.Is(err, webhookforward.ErrAdmissionRefused) {
			status = http.StatusServiceUnavailable
			code = pkgapi.ErrorCodeServiceUnavailable
		} else {
			lower := strings.ToLower(message)
			if strings.Contains(lower, "not configured") {
				status = http.StatusInternalServerError
				code = pkgapi.ErrorCodeInternalError
			} else if strings.Contains(lower, "queue is full") {
				status = http.StatusServiceUnavailable
			}
		}
		return webhookforward.ForwardResult{}, apiError{code: code, status: status, message: message}
	}
	if (strings.EqualFold(result.Status, "accepted") || result.WorkItems > 0) && any(h.context.Runtime) != nil {
		runtimeWithWebhook, ok := any(h.context.Runtime).(interface{ RecordWebhookDelivery(string, string) })
		if ok {
			runtimeWithWebhook.RecordWebhookDelivery(eventType, deliveryID)
		}
	}
	return result, nil
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hasForwardingProxyHeaders(headers http.Header) bool {
	for name, values := range headers {
		normalized := strings.ToLower(strings.TrimSpace(name))
		if normalized != "forwarded" && normalized != "x-real-ip" && !strings.HasPrefix(normalized, "x-forwarded-") {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

type apiError struct {
	code    pkgapi.ErrorCode
	status  int
	message string
	details any
}

type loopLogsFollowChunkEvent struct {
	RunID       *string `json:"runId,omitempty"`
	CurrentStep *string `json:"currentStep,omitempty"`
	ExecutionID *string `json:"executionId,omitempty"`
	Vendor      *string `json:"vendor,omitempty"`
	PID         *int64  `json:"pid,omitempty"`
	Status      *string `json:"status,omitempty"`
	Content     string  `json:"content"`
}

func (e apiError) Error() string {
	return e.message
}

func asAPIError(err error, target *apiError) bool {
	if err == nil || target == nil {
		return false
	}

	typed, ok := err.(apiError)
	if !ok {
		return false
	}

	*target = typed
	return true
}

func internalServerError(err error) apiError {
	message := "Unknown error"
	if err != nil {
		message = err.Error()
	}

	return apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: message}
}

func assertMethod(method, allowed, path string, w http.ResponseWriter, requestID string, writeError func(http.ResponseWriter, string, apiError)) bool {
	if method == allowed {
		return true
	}

	writeError(w, requestID, apiError{
		code:    pkgapi.ErrorCodeMethodNotAllowed,
		status:  http.StatusMethodNotAllowed,
		message: fmt.Sprintf("Unsupported method for %s", path),
	})

	return false
}

func isMutatingHTTPMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// isAdmissionExemptMutationPath lists mutation routes that must stay available
// before admission is ready (dashboard bootstrap) or are not work-producing.
func isAdmissionExemptMutationPath(path string) bool {
	switch path {
	case dashboardBootstrapCodePath, dashboardBootstrapExchangePath:
		return true
	default:
		return false
	}
}

func (h *Handler) admissionMutationDenial() (apiError, bool) {
	runtimeValue := h.context.Runtime
	if runtimeValue == nil {
		// Tests and embedders without a runtime keep prior open-admission behavior.
		return apiError{}, false
	}
	gate, ok := any(runtimeValue).(interface{ AllowMutations() error })
	if !ok {
		return apiError{}, false
	}
	if err := gate.AllowMutations(); err != nil {
		return apiError{
			code:    pkgapi.ErrorCodeServiceUnavailable,
			status:  http.StatusServiceUnavailable,
			message: err.Error(),
		}, true
	}
	return apiError{}, false
}

// admissionStateString projects the single admission Authority for status
// surfaces. Missing/partial runtimes report "ready" so embedders and older
// test doubles keep open-admission behavior (same as AllowMutations).
func (h *Handler) admissionStateString() string {
	runtimeValue := h.context.Runtime
	if runtimeValue == nil {
		return string(looperdruntime.AdmissionReady)
	}
	if typed, ok := any(runtimeValue).(interface {
		AdmissionState() looperdruntime.AdmissionState
	}); ok {
		return string(typed.AdmissionState())
	}
	return string(looperdruntime.AdmissionReady)
}

func authorizeRequest(r *http.Request, path string, cfg config.Config) error {
	if path == webhookForwardPath && cfg.Webhook.Enabled && isLoopbackRemoteAddr(r.RemoteAddr) {
		if !hasForwardingProxyHeaders(r.Header) {
			return nil
		}
	}

	// Browser foundation: Host allowlist + Origin match against config-derived
	// authorities when Origin is present (reads and mutations). CLI without Origin OK.
	// Non-browser callbacks (e.g. Feishu) with no Origin skip Host allowlist so a
	// public Host on 0.0.0.0 without server.baseUrl still reaches token verification.
	if err := validateBrowserRequestForPath(r, cfg, path); err != nil {
		return err
	}
	if path == apiBasePath+"/config" && r.Method == http.MethodPatch && cfg.Server.AuthMode != config.AuthModeLocalToken && !isDirectLoopbackConfigMutation(r, cfg) {
		return apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusForbidden,
			message: "Configuration updates without token authentication are limited to direct loopback clients",
		}
	}

	if cfg.Server.AuthMode != config.AuthModeLocalToken {
		return nil
	}

	if cfg.Server.LocalToken == nil || strings.TrimSpace(*cfg.Server.LocalToken) == "" {
		return apiError{
			code:    pkgapi.ErrorCodeAuthMisconfigured,
			status:  http.StatusInternalServerError,
			message: "Local token auth is enabled but no token is configured",
		}
	}

	// Public one-shot exception: SPA exchanges bootstrap code without Bearer.
	if isDashboardBootstrapExchange(path, r.Method) {
		return nil
	}

	if r.Header.Get("Authorization") != fmt.Sprintf("Bearer %s", *cfg.Server.LocalToken) {
		return apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusUnauthorized,
			message: "Authorization token is required",
		}
	}

	return nil
}

func isDirectLoopbackConfigMutation(r *http.Request, cfg config.Config) bool {
	if r == nil || !isLoopbackRemoteAddr(r.RemoteAddr) || hasForwardingProxyHeaders(r.Header) {
		return false
	}
	// RemoteAddr alone identifies a local reverse proxy, not the original
	// caller. Requiring a literal loopback request authority closes the common
	// stripped-forwarding-header proxy path while retaining direct CLI/browser
	// access through localhost, 127.0.0.1, or ::1.
	return isLoopbackAuthority(effectiveRequestHost(r, cfg))
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return false
	}
	host := remoteAddr
	if parsedHost, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func normalizePath(path string) string {
	if path == "" {
		return "/"
	}

	if len(path) == 1 {
		return path
	}

	return strings.TrimRight(path, "/")
}

func (h *Handler) writeSuccess(w http.ResponseWriter, requestID string, data any) {
	h.writeJSON(w, http.StatusOK, pkgapi.Success(requestID, data))
}

func (h *Handler) writeError(w http.ResponseWriter, requestID string, err apiError) {
	h.writeJSON(w, err.status, pkgapi.Failure(requestID, err.code, err.message, err.details))
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type healthResponse struct {
	Healthy   bool          `json:"healthy"`
	StartedAt *string       `json:"startedAt,omitempty"`
	Storage   storageHealth `json:"storage"`
}

type storageHealth struct {
	OK          bool            `json:"ok"`
	Mode        string          `json:"mode"`
	DBPath      string          `json:"dbPath"`
	LastUpdated string          `json:"lastUpdatedAt"`
	Details     *string         `json:"details,omitempty"`
	Migration   migrationHealth `json:"migration"`
}

type migrationHealth struct {
	LatestAvailableID string `json:"latestAvailableId,omitempty"`
	LatestAppliedID   string `json:"latestAppliedId,omitempty"`
	PendingCount      int    `json:"pendingCount"`
}

func (h *Handler) buildHealthResponse(ctx context.Context) (healthResponse, error) {
	state, err := h.loadStorageState(ctx)
	if err != nil {
		details := err.Error()
		state = storageState{
			Details: &details,
		}
	}

	startedAt := h.startedAtISO()

	return healthResponse{
		Healthy:   state.OK,
		StartedAt: startedAt,
		Storage: storageHealth{
			OK:          state.OK,
			Mode:        h.context.Config.Storage.Mode,
			DBPath:      h.context.Config.Storage.DBPath,
			LastUpdated: h.now().UTC().Format(javaScriptISOString),
			Details:     state.Details,
			Migration: migrationHealth{
				LatestAvailableID: state.LatestAvailableID,
				LatestAppliedID:   state.LatestAppliedID,
				PendingCount:      len(state.PendingMigrationIDs),
			},
		},
	}, nil
}

type statusResponse struct {
	Service         statusService                 `json:"service"`
	Storage         statusStorage                 `json:"storage"`
	Scheduler       statusScheduler               `json:"scheduler"`
	Agent           statusAgent                   `json:"agent"`
	WorktreeCleanup any                           `json:"worktreeCleanup"`
	Webhook         statusWebhook                 `json:"webhook"`
	Loops           statusLoops                   `json:"loops"`
	Network         any                           `json:"network,omitempty"`
	Safety          statusSafety                  `json:"safety"`
	Notifications   statusNotifications           `json:"notifications"`
	Tools           statusTools                   `json:"tools"`
	Providers       []forge.ForgejoProviderHealth `json:"providers"`
}

type statusService struct {
	Healthy    bool                  `json:"healthy"`
	Version    string                `json:"version"`
	Build      version.BuildMetadata `json:"build"`
	DaemonMode config.DaemonMode     `json:"daemonMode"`
	// AdmissionState is the single live admission Authority (ADR-0015 R1).
	// HTTP mutations and scheduler claims open only when this is "ready".
	AdmissionState string       `json:"admissionState"`
	StartedAt      *string      `json:"startedAt,omitempty"`
	Recovery       any          `json:"recovery"`
	Binary         statusBinary `json:"binary"`
}

type statusBinary struct {
	Name             string   `json:"name"`
	Path             string   `json:"path,omitempty"`
	InstallDir       string   `json:"installDir"`
	CurrentTarget    string   `json:"currentTarget"`
	ArtifactName     *string  `json:"artifactName"`
	SupportedTargets []string `json:"supportedTargets"`
}

type versionResponse struct {
	Version string                `json:"version"`
	Build   version.BuildMetadata `json:"build"`
	Binary  versionBinaryResponse `json:"binary"`
}

type versionBinaryResponse struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

func daemonExecutablePath() string {
	executablePath, err := os.Executable()
	if err != nil {
		return ""
	}
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" {
		return ""
	}

	resolvedPath, err := filepath.EvalSymlinks(executablePath)
	if err != nil {
		return executablePath
	}

	resolvedPath = strings.TrimSpace(resolvedPath)
	if resolvedPath == "" {
		return executablePath
	}

	return resolvedPath
}

type statusStorage struct {
	Mode              string   `json:"mode"`
	DBPath            string   `json:"dbPath"`
	SchemaVersion     string   `json:"schemaVersion,omitempty"`
	PendingMigrations []string `json:"pendingMigrations"`
	Healthy           bool     `json:"healthy"`
}

type statusScheduler struct {
	Healthy        bool `json:"healthy"`
	QueuedItems    int  `json:"queuedItems"`
	RunningItems   int  `json:"runningItems"`
	CompletedItems int  `json:"completedItems"`
	FailedItems    int  `json:"failedItems"`
	TotalRuns      int  `json:"totalRuns"`
	ActiveRuns     int  `json:"activeRuns"`
}

type statusAgent struct {
	Vendor              *config.AgentVendor `json:"vendor,omitempty"`
	Model               *string             `json:"model,omitempty"`
	NativeResumeEnabled bool                `json:"nativeResumeEnabled"`
	Timeouts            statusAgentTimeouts `json:"timeouts"`
}

type statusAgentTimeouts struct {
	Planner  statusAgentRoleTimeouts `json:"planner"`
	Worker   statusAgentRoleTimeouts `json:"worker"`
	Reviewer statusAgentRoleTimeouts `json:"reviewer"`
	Fixer    statusAgentRoleTimeouts `json:"fixer"`
}

type statusAgentRoleTimeouts struct {
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds"`
	MaxRuntimeSeconds  int `json:"maxRuntimeSeconds"`
}

type statusWebhook struct {
	Enabled                     bool     `json:"enabled"`
	EndpointURL                 string   `json:"endpointUrl"`
	FallbackPollIntervalSeconds int      `json:"fallbackPollIntervalSeconds"`
	Degraded                    bool     `json:"degraded"`
	DegradedReasons             []string `json:"degradedReasons"`
	ConfiguredForwarders        int      `json:"configuredForwarders"`
	RunningForwarders           int      `json:"runningForwarders"`
}

type statusLoopType struct {
	Queued     int `json:"queued"`
	Running    int `json:"running"`
	Waiting    int `json:"waiting"`
	Paused     int `json:"paused"`
	Failed     int `json:"failed"`
	Terminated int `json:"terminated"`
	Stopped    int `json:"stopped"`
}

type statusLoops struct {
	Planner  statusLoopType `json:"planner"`
	Reviewer statusLoopType `json:"reviewer"`
	Worker   statusLoopType `json:"worker"`
	Fixer    statusLoopType `json:"fixer"`
}

type statusSafety struct {
	AllowAutoCommit    bool                  `json:"allowAutoCommit"`
	AllowAutoPush      bool                  `json:"allowAutoPush"`
	AllowAutoApprove   bool                  `json:"allowAutoApprove"`
	AllowRiskyFixes    bool                  `json:"allowRiskyFixes"`
	FixAllPullRequests bool                  `json:"fixAllPullRequests"`
	OpenPRStrategy     config.OpenPRStrategy `json:"openPrStrategy"`
}

type statusNotifications struct {
	InAppEnabled     bool `json:"inAppEnabled"`
	OsascriptEnabled bool `json:"osascriptEnabled"`
}

type statusTools struct {
	Git       bool `json:"git"`
	GH        bool `json:"gh"`
	Osascript bool `json:"osascript"`
}

func (h *Handler) handleConfigRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	switch r.Method {
	case http.MethodGet:
		h.writeSuccess(w, requestID, h.buildConfigResponse())
	case http.MethodPatch:
		patch, err := decodeConfigPatchRequest(w, r)
		if err != nil {
			h.writeError(w, requestID, configRequestAPIError(err))
			return
		}
		if h.context.PatchConfig == nil {
			h.writeError(w, requestID, configRequestAPIError(ConfigRequestError{
				Kind:    ConfigRequestErrorKindUnsupported,
				Message: "Dynamic configuration updates are unavailable",
				Issues: []ConfigPatchIssue{{
					Code:    "config_patch_unsupported",
					Message: "This daemon does not support field-level configuration updates",
				}},
			}))
			return
		}
		if err := h.context.PatchConfig(r.Context(), patch); err != nil {
			h.writeError(w, requestID, configRequestAPIError(err))
			return
		}

		// A mutation establishes a new snapshot boundary. Refresh once after the
		// callback so the PATCH response projects the configuration just applied.
		if h.context.ConfigSnapshot != nil {
			cfg, metadata := h.context.ConfigSnapshot()
			h.context.Config = cfg
			h.context.ConfigMetadata = func() ConfigMetadata { return metadata }
		} else if runtimeConfig, ok := any(h.context.Runtime).(interface{ Config() config.Config }); ok {
			h.context.Config = runtimeConfig.Config()
		}
		h.writeSuccess(w, requestID, h.buildConfigResponse())
	default:
		h.writeError(w, requestID, apiError{
			code:    pkgapi.ErrorCodeMethodNotAllowed,
			status:  http.StatusMethodNotAllowed,
			message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/config"),
		})
	}
}

func decodeConfigPatchRequest(w http.ResponseWriter, r *http.Request) (ConfigPatchRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigPatchBodyBytes)
	raw, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(readErr, &maxBytesError) {
			return ConfigPatchRequest{}, ConfigRequestError{
				Kind:    ConfigRequestErrorKindTooLarge,
				Message: "Configuration patch is too large",
				Issues:  []ConfigPatchIssue{{Code: "request_too_large", Message: fmt.Sprintf("Request body exceeds %d bytes", maxConfigPatchBodyBytes)}},
			}
		}
		return ConfigPatchRequest{}, ConfigRequestError{Kind: ConfigRequestErrorKindValidation, Message: "Invalid configuration patch", Issues: []ConfigPatchIssue{{Code: "invalid_json", Message: readErr.Error()}}}
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := config.ValidateUniqueJSONNames(raw); err != nil {
			return ConfigPatchRequest{}, ConfigRequestError{
				Kind:    ConfigRequestErrorKindValidation,
				Message: "Invalid configuration patch",
				Issues:  []ConfigPatchIssue{{Code: "duplicate_json_name", Message: err.Error()}},
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var decoded *ConfigPatchRequest
	if err := decoder.Decode(&decoded); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return ConfigPatchRequest{}, ConfigRequestError{
				Kind:    ConfigRequestErrorKindTooLarge,
				Message: "Configuration patch is too large",
				Issues: []ConfigPatchIssue{{
					Code:    "request_too_large",
					Message: fmt.Sprintf("Request body exceeds %d bytes", maxConfigPatchBodyBytes),
				}},
			}
		}
		message := "Request body must be a JSON object"
		code := "invalid_json"
		if errors.Is(err, io.EOF) {
			message = "Request body is required"
			code = "request_body_required"
		}
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Code: code, Message: message}},
		}
	}
	if decoded == nil {
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Code: "invalid_json", Message: "Request body must be a JSON object"}},
		}
	}
	patch := *decoded
	if strings.TrimSpace(patch.Revision) == "" {
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Path: "revision", Code: "missing_revision", Message: "revision is required; refresh configuration and try again"}},
		}
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				return ConfigPatchRequest{}, ConfigRequestError{
					Kind:    ConfigRequestErrorKindTooLarge,
					Message: "Configuration patch is too large",
					Issues:  []ConfigPatchIssue{{Code: "request_too_large", Message: fmt.Sprintf("Request body exceeds %d bytes", maxConfigPatchBodyBytes)}},
				}
			}
		}
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Code: "trailing_json", Message: "Request body must contain exactly one JSON object"}},
		}
	}

	issues := validateConfigPatchRequest(patch)
	if len(issues) > 0 {
		return ConfigPatchRequest{}, ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  issues,
		}
	}
	if patch.Set == nil {
		patch.Set = map[string]json.RawMessage{}
	}
	if patch.Unset == nil {
		patch.Unset = []string{}
	}
	return patch, nil
}

func validateConfigPatchRequest(patch ConfigPatchRequest) []ConfigPatchIssue {
	issues := make([]ConfigPatchIssue, 0)
	if len(patch.Set) == 0 && len(patch.Unset) == 0 {
		issues = append(issues, ConfigPatchIssue{Code: "empty_patch", Message: "At least one set or unset operation is required"})
	}

	setPaths := make(map[string]struct{}, len(patch.Set))
	for path, raw := range patch.Set {
		setPaths[path] = struct{}{}
		if path == "" || path != strings.TrimSpace(path) {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "invalid_path", Message: "Set paths must be non-empty and contain no surrounding whitespace"})
		}
		if len(raw) == 0 {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "missing_value", Message: "Set operations require a JSON value"})
		}
	}

	unsetPaths := make(map[string]struct{}, len(patch.Unset))
	for _, path := range patch.Unset {
		if path == "" || path != strings.TrimSpace(path) {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "invalid_path", Message: "Unset paths must be non-empty and contain no surrounding whitespace"})
		}
		if _, exists := unsetPaths[path]; exists {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "duplicate_unset", Message: "Unset paths must be unique"})
		}
		unsetPaths[path] = struct{}{}
		if _, exists := setPaths[path]; exists {
			issues = append(issues, ConfigPatchIssue{Path: path, Code: "conflicting_operation", Message: "A path cannot be both set and unset"})
		}
	}

	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Path == issues[j].Path {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Path < issues[j].Path
	})
	return issues
}

func configRequestAPIError(err error) apiError {
	requestError, ok := asConfigRequestError(err)
	if !ok {
		return internalServerError(err)
	}

	status := http.StatusBadRequest
	code := pkgapi.ErrorCodeValidationFailed
	switch requestError.Kind {
	case ConfigRequestErrorKindValidation, ConfigRequestErrorKindUnsupported:
	case ConfigRequestErrorKindConflict:
		status = http.StatusConflict
		code = pkgapi.ErrorCodeConfigConflict
	case ConfigRequestErrorKindTooLarge:
		status = http.StatusRequestEntityTooLarge
		code = pkgapi.ErrorCodeRequestTooLarge
	default:
		return internalServerError(err)
	}

	message := strings.TrimSpace(requestError.Message)
	if message == "" {
		message = "Configuration update failed"
	}
	issues := append([]ConfigPatchIssue{}, requestError.Issues...)
	return apiError{
		code:    code,
		status:  status,
		message: message,
		details: struct {
			Issues []ConfigPatchIssue `json:"issues"`
		}{Issues: issues},
	}
}

func asConfigRequestError(err error) (ConfigRequestError, bool) {
	if err == nil {
		return ConfigRequestError{}, false
	}
	var pointer *ConfigRequestError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	var value ConfigRequestError
	if errors.As(err, &value) {
		return value, true
	}
	return ConfigRequestError{}, false
}

type configResponse struct {
	Server        configServerResponse      `json:"server"`
	Storage       config.StorageConfig      `json:"storage"`
	Scheduler     config.SchedulerConfig    `json:"scheduler"`
	Webhook       config.WebhookConfig      `json:"webhook"`
	Network       config.NetworkConfig      `json:"network"`
	Agent         configAgentResponse       `json:"agent"`
	Logging       config.LoggingConfig      `json:"logging"`
	Notifications config.NotificationConfig `json:"notifications"`
	Disclosure    config.DisclosureConfig   `json:"disclosure"`
	Tools         config.ToolPathsConfig    `json:"tools"`
	Daemon        configDaemonResponse      `json:"daemon"`
	Package       config.PackageConfig      `json:"package"`
	Defaults      config.DefaultsConfig     `json:"defaults"`
	Instructions  config.InstructionsConfig `json:"instructions"`
	HITL          config.HITLConfig         `json:"hitl"`
	Roles         config.RoleConfigs        `json:"roles"`
	Providers     []config.ProviderConfig   `json:"providers"`
	Projects      []config.ProjectRefConfig `json:"projects"`
	Metadata      ConfigMetadata            `json:"metadata"`
}

type configServerResponse struct {
	Host     string          `json:"host"`
	Port     int             `json:"port"`
	BaseURL  *string         `json:"baseUrl,omitempty"`
	AuthMode config.AuthMode `json:"authMode"`
}

type configAgentResponse struct {
	Vendor       *config.AgentVendor                  `json:"vendor,omitempty"`
	Model        *string                              `json:"model,omitempty"`
	Profiles     map[string]config.AgentBindingConfig `json:"profiles,omitempty"`
	Params       map[string]any                       `json:"params"`
	Env          map[string]string                    `json:"env"`
	EnvKeys      []string                             `json:"envKeys"`
	Timeouts     config.AgentTimeoutConfig            `json:"timeouts"`
	NativeResume config.AgentNativeResumeConfig       `json:"nativeResume"`
}

type configDaemonResponse struct {
	Mode                   config.DaemonMode            `json:"mode"`
	RestartPolicy          config.DaemonRestartPolicy   `json:"restartPolicy"`
	RestartThrottleSeconds int                          `json:"restartThrottleSeconds"`
	PlistPath              *string                      `json:"plistPath,omitempty"`
	LogDir                 string                       `json:"logDir"`
	WorkingDirectory       string                       `json:"workingDirectory"`
	Environment            map[string]string            `json:"environment"`
	WorktreeCleanup        config.WorktreeCleanupConfig `json:"worktreeCleanup"`
}

func (h *Handler) buildConfigResponse() configResponse {
	cfg := h.context.Config

	return configResponse{
		Server: configServerResponse{
			Host:     cfg.Server.Host,
			Port:     cfg.Server.Port,
			BaseURL:  cfg.Server.BaseURL,
			AuthMode: cfg.Server.AuthMode,
		},
		Storage:   cfg.Storage,
		Scheduler: cfg.Scheduler,
		Webhook:   cfg.Webhook,
		Network:   cfg.Network,
		Agent: configAgentResponse{
			Vendor:       cfg.Agent.Vendor,
			Model:        cfg.Agent.Model,
			Profiles:     cloneAgentProfiles(cfg.Agent.Profiles),
			Params:       map[string]any{},
			Env:          map[string]string{},
			EnvKeys:      sortedMapKeys(cfg.Agent.Env),
			Timeouts:     cfg.Agent.Timeouts,
			NativeResume: cfg.Agent.NativeResume,
		},
		Logging:       cfg.Logging,
		Notifications: cfg.Notifications,
		Disclosure:    cfg.Disclosure,
		Tools:         cfg.Tools,
		Daemon: configDaemonResponse{
			Mode:                   cfg.Daemon.Mode,
			RestartPolicy:          cfg.Daemon.RestartPolicy,
			RestartThrottleSeconds: cfg.Daemon.RestartThrottleSeconds,
			PlistPath:              cfg.Daemon.PlistPath,
			LogDir:                 cfg.Daemon.LogDir,
			WorkingDirectory:       cfg.Daemon.WorkingDirectory,
			Environment:            map[string]string{},
			WorktreeCleanup:        cfg.Daemon.WorktreeCleanup,
		},
		Package:      cfg.Package,
		Defaults:     cfg.Defaults,
		Instructions: cfg.Instructions,
		HITL:         cfg.HITL,
		Roles:        cfg.Roles,
		Providers:    append([]config.ProviderConfig{}, cfg.Providers...),
		Projects:     append([]config.ProjectRefConfig{}, cfg.Projects...),
		Metadata:     h.buildConfigMetadata(),
	}
}

func (h *Handler) buildConfigMetadata() ConfigMetadata {
	metadata := ConfigMetadata{}
	if h.context.ConfigMetadata != nil {
		metadata = h.context.ConfigMetadata()
	}
	metadata.RejectedPaths = append([]string{}, metadata.RejectedPaths...)
	fields := make(map[string]ConfigFieldMetadata, len(metadata.Fields))
	for path, field := range metadata.Fields {
		fields[path] = field
	}
	metadata.Fields = fields
	return metadata
}

func sortedMapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// cloneAgentProfiles copies profile bindings for the secret-safe config projection.
// Empty maps become nil so json omitempty matches zero-diff style.
func cloneAgentProfiles(profiles map[string]config.AgentBindingConfig) map[string]config.AgentBindingConfig {
	if len(profiles) == 0 {
		return nil
	}
	cloned := make(map[string]config.AgentBindingConfig, len(profiles))
	for id, binding := range profiles {
		entry := config.AgentBindingConfig{}
		if binding.Vendor != nil {
			vendor := *binding.Vendor
			entry.Vendor = &vendor
		}
		if binding.Model != nil {
			model := *binding.Model
			entry.Model = &model
		}
		cloned[id] = entry
	}
	return cloned
}

func (h *Handler) buildWebhookStatusResponse() looperdruntime.WebhookStatus {
	if runtimeWithWebhook, ok := any(h.context.Runtime).(interface {
		WebhookStatus() looperdruntime.WebhookStatus
	}); ok {
		return runtimeWithWebhook.WebhookStatus()
	}
	return looperdruntime.WebhookStatus{
		Enabled:                     h.context.Config.Webhook.Enabled,
		Mode:                        h.context.Config.Webhook.Mode,
		FallbackPollIntervalSeconds: h.context.Config.Webhook.FallbackPollIntervalSeconds,
		ListenerPath:                "/webhook/forward",
		EndpointURL:                 strings.TrimRight(serverBaseURL(h.context.Config.Server), "/") + "/webhook/forward",
		TunnelPublicBaseURL:         strings.TrimRight(strings.TrimSpace(h.context.Config.Webhook.PublicBaseURL), "/"),
		DegradedReasons:             []string{},
		RecentOutcomes:              []looperdruntime.WebhookRecentOutcome{},
		Forwarders:                  []looperdruntime.WebhookForwarderState{},
	}
}

func summarizeWebhookStatus(status looperdruntime.WebhookStatus) statusWebhook {
	running := 0
	for _, forwarder := range status.Forwarders {
		if forwarder.Running {
			running++
		}
	}
	return statusWebhook{
		Enabled:                     status.Enabled,
		EndpointURL:                 status.EndpointURL,
		FallbackPollIntervalSeconds: status.FallbackPollIntervalSeconds,
		Degraded:                    status.Degraded,
		DegradedReasons:             append([]string{}, status.DegradedReasons...),
		ConfiguredForwarders:        len(status.Forwarders),
		RunningForwarders:           running,
	}
}

func serverBaseURL(cfg config.ServerConfig) string {
	if cfg.BaseURL != nil && strings.TrimSpace(*cfg.BaseURL) != "" {
		return strings.TrimSpace(*cfg.BaseURL)
	}
	return fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
}

func (h *Handler) buildVersionResponse() versionResponse {
	return versionResponse{
		Version: version.Current().Version,
		Build:   version.Current().Metadata,
		Binary: versionBinaryResponse{
			Name: "looperd",
			Path: daemonExecutablePath(),
		},
	}
}

func (h *Handler) buildStatusResponse(ctx context.Context) (statusResponse, error) {
	storageState, err := h.loadStorageState(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	services := h.context.Runtime.Services()
	loopCountsByType, err := services.Repositories.Loops.CountByTypeAndStatus(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	runCounts, err := services.Repositories.Runs.CountByStatus(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	queueCounts, err := services.Repositories.Queue.CountByAllStatuses(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	loopCounts := countLoops(loopCountsByType)

	currentTarget := currentLooperdTarget()
	installDir := filepath.Join(homeDirOrEmpty(), ".looper", "bin")
	artifactName := looperdArtifactName(currentTarget)

	return statusResponse{
		Service: statusService{
			Healthy:        storageState.OK,
			Version:        version.Current().Version,
			Build:          version.Current().Metadata,
			DaemonMode:     h.context.Config.Daemon.Mode,
			AdmissionState: h.admissionStateString(),
			StartedAt:      h.startedAtISO(),
			Recovery:       h.recoverySummary(),
			Binary: statusBinary{
				Name:             "looperd",
				Path:             daemonExecutablePath(),
				InstallDir:       installDir,
				CurrentTarget:    currentTarget,
				ArtifactName:     artifactName,
				SupportedTargets: []string{"darwin-arm64", "linux-amd64"},
			},
		},
		Storage: statusStorage{
			Mode:              h.context.Config.Storage.Mode,
			DBPath:            h.context.Config.Storage.DBPath,
			SchemaVersion:     storageState.schemaVersion(),
			PendingMigrations: append([]string{}, storageState.PendingMigrationIDs...),
			Healthy:           storageState.OK,
		},
		Scheduler: statusScheduler{
			Healthy:        true,
			QueuedItems:    int(queueCounts["queued"]),
			RunningItems:   int(queueCounts["running"]),
			CompletedItems: int(queueCounts["completed"]),
			FailedItems:    int(queueCounts["failed"] + queueCounts["manual_intervention"]),
			TotalRuns:      sumStatusCounts(runCounts),
			ActiveRuns:     int(runCounts["running"]),
		},
		Agent: statusAgent{
			Vendor:              h.context.Config.Agent.Vendor,
			Model:               h.context.Config.Agent.Model,
			NativeResumeEnabled: h.context.Config.Agent.NativeResume.Enabled,
			Timeouts: statusAgentTimeouts{
				Planner:  statusAgentRoleTimeouts{IdleTimeoutSeconds: h.context.Config.Agent.Timeouts.PlannerIdleTimeoutSeconds, MaxRuntimeSeconds: h.context.Config.Agent.Timeouts.PlannerMaxRuntimeSeconds},
				Worker:   statusAgentRoleTimeouts{IdleTimeoutSeconds: h.context.Config.Agent.Timeouts.WorkerIdleTimeoutSeconds, MaxRuntimeSeconds: h.context.Config.Agent.Timeouts.WorkerMaxRuntimeSeconds},
				Reviewer: statusAgentRoleTimeouts{IdleTimeoutSeconds: h.context.Config.Agent.Timeouts.ReviewerIdleTimeoutSeconds, MaxRuntimeSeconds: h.context.Config.Agent.Timeouts.ReviewerMaxRuntimeSeconds},
				Fixer:    statusAgentRoleTimeouts{IdleTimeoutSeconds: h.context.Config.Agent.Timeouts.FixerIdleTimeoutSeconds, MaxRuntimeSeconds: h.context.Config.Agent.Timeouts.FixerMaxRuntimeSeconds},
			},
		},
		WorktreeCleanup: h.buildWorktreeCleanupStatusResponse(),
		Webhook:         summarizeWebhookStatus(h.buildWebhookStatusResponse()),
		Loops:           loopCounts,
		Network:         h.buildNetworkStatusResponse(),
		Safety: statusSafety{
			AllowAutoCommit:    h.context.Config.Defaults.AllowAutoCommit,
			AllowAutoPush:      h.context.Config.Defaults.AllowAutoPush,
			AllowAutoApprove:   h.context.Config.Defaults.AllowAutoApprove,
			AllowRiskyFixes:    h.context.Config.Defaults.AllowRiskyFixes,
			FixAllPullRequests: h.context.Config.Defaults.FixAllPullRequests,
			OpenPRStrategy:     h.context.Config.Defaults.OpenPRStrategy,
		},
		Notifications: statusNotifications{
			InAppEnabled:     h.context.Config.Notifications.InApp,
			OsascriptEnabled: h.context.Config.Notifications.Osascript.Enabled,
		},
		Tools: statusTools{
			Git:       hasValue(h.context.Config.Tools.GitPath),
			GH:        hasValue(h.context.Config.Tools.GHPath),
			Osascript: hasValue(h.context.Config.Tools.OsascriptPath),
		},
		Providers: h.buildProviderHealth(ctx),
	}, nil
}

func (h *Handler) buildProviderHealth(ctx context.Context) []forge.ForgejoProviderHealth {
	providers := make([]forge.ForgejoProviderHealth, 0)
	for _, provider := range h.context.Config.Providers {
		if provider.Kind != config.ProviderKindForgejo {
			continue
		}
		projects := make([]forge.ForgejoProbeProject, 0)
		for _, project := range h.context.Config.Projects {
			if project.Provider == provider.ID {
				projects = append(projects, forge.ForgejoProbeProject{ID: project.ID, Repo: project.Repo})
			}
		}
		providers = append(providers, forge.ProbeForgejoProvider(ctx, provider, projects))
	}
	return providers
}

func (h *Handler) buildWorktreeCleanupStatusResponse() any {
	if runtimeWithCleanup, ok := any(h.context.Runtime).(interface {
		WorktreeCleanupStatus() looperdruntime.WorktreeCleanupStatus
	}); ok {
		return runtimeWithCleanup.WorktreeCleanupStatus()
	}
	return looperdruntime.WorktreeCleanupStatus{
		Enabled:    h.context.Config.Daemon.WorktreeCleanup.Enabled,
		DryRun:     h.context.Config.Daemon.WorktreeCleanup.DryRun,
		LastStatus: "idle",
	}
}

func (h *Handler) buildNetworkStatusResponse() any {
	if runtimeWithNetwork, ok := any(h.context.Runtime).(interface{ NetworkStatus() networkclient.Status }); ok {
		return runtimeWithNetwork.NetworkStatus()
	}
	return nil
}

type storageState struct {
	OK                  bool
	LatestAvailableID   string
	LatestAppliedID     string
	PendingMigrationIDs []string
	Details             *string
}

func (h *Handler) loadStorageState(ctx context.Context) (storageState, error) {
	services := h.context.Runtime.Services()
	status, err := services.Coordinator.MigrationRunner().Status(ctx)
	if err != nil {
		return storageState{}, err
	}

	state := storageState{OK: true}
	if len(status.Available) > 0 {
		state.LatestAvailableID = status.Available[len(status.Available)-1].ID
	}
	if len(status.Applied) > 0 {
		state.LatestAppliedID = status.Applied[len(status.Applied)-1].ID
	}
	state.PendingMigrationIDs = make([]string, 0, len(status.Pending))
	for _, migration := range status.Pending {
		state.PendingMigrationIDs = append(state.PendingMigrationIDs, migration.ID)
	}

	return state, nil
}

func (h *Handler) startedAtISO() *string {
	startedAt, ok := h.context.Runtime.StartedAt()
	if !ok {
		return nil
	}

	value := startedAt.UTC().Format(javaScriptISOString)
	return &value
}

func (s storageState) schemaVersion() string {
	if s.LatestAppliedID == "" {
		return "uninitialized"
	}

	return s.LatestAppliedID
}

func countLoops(countsByType map[string]map[string]int64) statusLoops {
	counts := statusLoops{}
	for loopType, statuses := range countsByType {
		var target *statusLoopType
		switch loopType {
		case "planner":
			target = &counts.Planner
		case "reviewer":
			target = &counts.Reviewer
		case "worker":
			target = &counts.Worker
		case "fixer":
			target = &counts.Fixer
		default:
			continue
		}

		for status, count := range statuses {
			switch status {
			case "queued":
				target.Queued = int(count)
			case "running":
				target.Running = int(count)
			case "waiting":
				target.Waiting = int(count)
			case "paused":
				target.Paused = int(count)
			case "failed":
				target.Failed = int(count)
			case "terminated":
				target.Terminated = int(count)
			case "stopped":
				target.Stopped = int(count)
			}
		}
	}

	return counts
}

func sumStatusCounts(counts map[string]int64) int {
	total := 0
	for _, count := range counts {
		total += int(count)
	}
	return total
}

func generateRequestID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}

	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80

	hexValue := hex.EncodeToString(buffer)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[0:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:32])
}

func hasValue(value *string) bool {
	return value != nil && strings.TrimSpace(*value) != ""
}

func homeDirOrEmpty() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return home
}

func currentLooperdTarget() string {
	return fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
}

func normalizeRecoverySummary(summary looperdruntime.RecoverySummary) map[string]any {
	normalized := map[string]any{}
	if summary.StartedAt != "" {
		normalized["startedAt"] = summary.StartedAt
	}
	if summary.CompletedAt != "" {
		normalized["completedAt"] = summary.CompletedAt
	}
	if summary.OrphanAgentCleanup.Attempted || summary.OrphanAgentCleanup.CleanedCount != 0 || summary.OrphanAgentCleanup.QuarantinedCount != 0 || summary.OrphanAgentCleanup.Warning != "" {
		orphan := map[string]any{
			"attempted":        summary.OrphanAgentCleanup.Attempted,
			"cleanedCount":     summary.OrphanAgentCleanup.CleanedCount,
			"quarantinedCount": summary.OrphanAgentCleanup.QuarantinedCount,
		}
		if summary.OrphanAgentCleanup.Warning != "" {
			orphan["warning"] = summary.OrphanAgentCleanup.Warning
		}
		normalized["orphanAgentCleanup"] = orphan
	}
	if summary.ExpiredLocksReleased != 0 {
		normalized["expiredLocksReleased"] = summary.ExpiredLocksReleased
	}
	if summary.InterruptedRunsMarked != 0 {
		normalized["interruptedRunsMarked"] = summary.InterruptedRunsMarked
	}
	if summary.LoopsRequeued != 0 {
		normalized["loopsRequeued"] = summary.LoopsRequeued
	}
	if summary.EventsWritten != 0 {
		normalized["eventsWritten"] = summary.EventsWritten
	}

	return normalized
}

type projectsListResponse struct {
	Items []projectResponse `json:"items"`
}

type loopsListResponse struct {
	Items  []loopResponse `json:"items"`
	Total  int64          `json:"total"`
	Limit  *int64         `json:"limit,omitempty"`
	Offset int64          `json:"offset"`
}

const maxLoopsListLimit int64 = 200

type eventsListResponse struct {
	Items []eventResponse `json:"items"`
}

type entityEventsResponse struct {
	EntityType string          `json:"entityType"`
	EntityID   string          `json:"entityId"`
	Items      []eventResponse `json:"items"`
}

type eventResponse struct {
	ID               string  `json:"id"`
	EventType        string  `json:"eventType"`
	ProjectID        *string `json:"projectId"`
	LoopID           *string `json:"loopId"`
	RunID            *string `json:"runId"`
	EntityType       *string `json:"entityType"`
	EntityID         *string `json:"entityId"`
	CorrelationID    *string `json:"correlationId"`
	CausationID      *string `json:"causationId"`
	ActorType        *string `json:"actorType"`
	ActorID          *string `json:"actorId"`
	ActorDisplayName *string `json:"actorDisplayName"`
	PayloadJSON      string  `json:"payloadJson"`
	CreatedAt        string  `json:"createdAt"`
	Payload          any     `json:"payload"`
}

type pullRequestsListResponse struct {
	Items []pullRequestResponse `json:"items"`
}

type pullRequestResponse struct {
	Repo                  string  `json:"repo"`
	PRNumber              int64   `json:"prNumber"`
	ProjectID             *string `json:"projectId"`
	HeadSHA               *string `json:"headSha"`
	BaseSHA               *string `json:"baseSha"`
	Title                 *string `json:"title"`
	Body                  *string `json:"body"`
	Author                *string `json:"author"`
	DiffRef               *string `json:"diffRef"`
	ChecksSummary         *string `json:"checksSummary"`
	UnresolvedThreadCount int64   `json:"unresolvedThreadCount"`
	ReviewState           *string `json:"reviewState"`
	Mergeability          *string `json:"mergeability"`
	BlockingReason        *string `json:"blockingReason"`
	IsDraft               *bool   `json:"isDraft"`
	HasConflicts          *bool   `json:"hasConflicts"`
	CapturedAt            *string `json:"capturedAt"`
	Reviewer              *string `json:"reviewer"`
	Fixer                 *string `json:"fixer"`
}

type pullRequestStatusResponse struct {
	Repo                  string                `json:"repo"`
	PRNumber              int64                 `json:"prNumber"`
	ReviewState           *string               `json:"reviewState"`
	ChecksSummary         *string               `json:"checksSummary"`
	UnresolvedThreadCount int64                 `json:"unresolvedThreadCount"`
	CapturedAt            string                `json:"capturedAt"`
	Reviewer              *string               `json:"reviewer"`
	Fixer                 *string               `json:"fixer"`
	LoopStatus            pullRequestLoopStatus `json:"loopStatus"`
}

type pullRequestLoopStatus struct {
	Loops           []string `json:"loops"`
	LatestRunStatus *string  `json:"latestRunStatus"`
	RunningRunCount int      `json:"runningRunCount"`
}

type loopResponse struct {
	ID           string  `json:"id"`
	Seq          int64   `json:"seq"`
	ProjectID    string  `json:"projectId"`
	Type         string  `json:"type"`
	TargetType   string  `json:"targetType"`
	TargetID     *string `json:"targetId"`
	Repo         *string `json:"repo"`
	PRNumber     *int64  `json:"prNumber"`
	Status       string  `json:"status"`
	ConfigJSON   *string `json:"configJson"`
	MetadataJSON *string `json:"metadataJson"`
	LastRunAt    *string `json:"lastRunAt"`
	NextRunAt    *string `json:"nextRunAt"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
	// Queue-derived diagnostics (latest queue item / run), matching looper describe / ps.
	Attempts          *int64  `json:"attempts,omitempty"`
	MaxAttempts       *int64  `json:"maxAttempts,omitempty"`
	LastFailureKind   *string `json:"lastFailureKind,omitempty"`
	LastFailureReason *string `json:"lastFailureReason,omitempty"`
}

type loopLogsResponse struct {
	Seq        int64                 `json:"seq"`
	LoopID     string                `json:"loopId"`
	LoopType   string                `json:"loopType"`
	LoopStatus string                `json:"loopStatus"`
	Run        *loopLogsRunResponse  `json:"run"`
	Agent      *loopLogsAgentPayload `json:"agent"`
}

type loopLogsRunResponse struct {
	RunID        string  `json:"runId"`
	Status       string  `json:"status"`
	CurrentStep  *string `json:"currentStep"`
	StartedAt    string  `json:"startedAt"`
	EndedAt      *string `json:"endedAt"`
	Summary      *string `json:"summary"`
	ErrorMessage *string `json:"errorMessage"`
}

type loopLogsAgentPayload struct {
	ExecutionID     string  `json:"executionId"`
	Vendor          string  `json:"vendor"`
	Status          string  `json:"status"`
	PID             *int64  `json:"pid"`
	StartedAt       string  `json:"startedAt"`
	EndedAt         *string `json:"endedAt"`
	HeartbeatCount  int64   `json:"heartbeatCount"`
	LastHeartbeatAt *string `json:"lastHeartbeatAt"`
	Summary         *string `json:"summary"`
	ParseStatus     *string `json:"parseStatus"`
	ErrorMessage    *string `json:"errorMessage"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
}

type projectResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	RepoPath   string `json:"repoPath"`
	BaseBranch string `json:"baseBranch"`
	Archived   bool   `json:"archived"`
	// Provider is the resolved provider kind for display (github, forgejo, plane).
	Provider     string  `json:"provider"`
	Repo         *string `json:"repo"`
	WorktreeRoot *string `json:"worktreeRoot"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
}

type createProjectResponse struct {
	projectResponse
	DiscoveredPullRequests int      `json:"discoveredPullRequests"`
	DiscoveredWorktrees    int      `json:"discoveredWorktrees"`
	PendingSnapshots       int      `json:"pendingSnapshots"`
	CapturedSnapshots      int      `json:"capturedSnapshots"`
	Warnings               []string `json:"warnings"`
}

type runsListResponse struct {
	Items []runResponse `json:"items"`
}

type runResponse struct {
	ID                string  `json:"id"`
	LoopID            string  `json:"loopId"`
	Status            string  `json:"status"`
	CurrentStep       *string `json:"currentStep"`
	LastCompletedStep *string `json:"lastCompletedStep"`
	CheckpointJSON    *string `json:"checkpointJson"`
	Summary           *string `json:"summary"`
	ErrorMessage      *string `json:"errorMessage"`
	StartedAt         string  `json:"startedAt"`
	LastHeartbeatAt   *string `json:"lastHeartbeatAt"`
	EndedAt           *string `json:"endedAt"`
	CreatedAt         string  `json:"createdAt"`
	UpdatedAt         string  `json:"updatedAt"`
}

type activeRunsListResponse struct {
	Items []activeRunView `json:"items"`
}

type activeRunsQuery struct {
	All       bool
	Status    string
	Type      string
	ProjectID string
	Repo      string
	PRNumber  *int64
}

type activeRunView struct {
	Seq               int64              `json:"seq"`
	RunID             *string            `json:"runId"`
	LoopID            string             `json:"loopId"`
	ProjectID         string             `json:"projectId"`
	Type              string             `json:"type"`
	Status            string             `json:"status"`
	LoopStatus        string             `json:"loopStatus"`
	DisplayStatus     string             `json:"displayStatus"`
	Attempts          *int64             `json:"attempts,omitempty"`
	MaxAttempts       *int64             `json:"maxAttempts,omitempty"`
	LastFailureKind   *string            `json:"lastFailureKind,omitempty"`
	LastFailureReason *string            `json:"lastFailureReason,omitempty"`
	ResumePolicy      *string            `json:"resumePolicy,omitempty"`
	CurrentStep       *string            `json:"currentStep"`
	StartedAt         *string            `json:"startedAt"`
	EndedAt           *string            `json:"endedAt,omitempty"`
	Target            activeRunTarget    `json:"target"`
	Agent             *activeRunAgent    `json:"agent"`
	Worktree          *activeRunWorktree `json:"worktree"`
}

type retryLoopRequest struct {
	Mode                   string `json:"mode"`
	ResetAttempts          *bool  `json:"resetAttempts"`
	DiscardWorktreeChanges *bool  `json:"discardWorktreeChanges"`
}

type retryLoopResponse struct {
	Loop                   loopResponse           `json:"loop"`
	QueueItemID            *string                `json:"queueItemId,omitempty"`
	Mode                   string                 `json:"mode"`
	ResetAttempts          bool                   `json:"resetAttempts"`
	DiscardWorktreeChanges bool                   `json:"discardWorktreeChanges"`
	WorktreeDiscard        *worktreeDiscardResult `json:"worktreeDiscard,omitempty"`
}

type activeRunTarget struct {
	Type        string  `json:"type"`
	ProjectID   *string `json:"projectId,omitempty"`
	Repo        *string `json:"repo,omitempty"`
	PRNumber    *int64  `json:"prNumber,omitempty"`
	IssueNumber *int64  `json:"issueNumber,omitempty"`
	Label       string  `json:"label"`
}

type activeRunAgent struct {
	Active          bool    `json:"active"`
	ActiveCount     int     `json:"activeCount"`
	ExecutionID     string  `json:"executionId"`
	Vendor          string  `json:"vendor"`
	PID             *int64  `json:"pid"`
	StartedAt       string  `json:"startedAt"`
	LastHeartbeatAt *string `json:"lastHeartbeatAt"`
	HeartbeatCount  int64   `json:"heartbeatCount"`
	Status          string  `json:"status"`
}

type activeRunWorktree struct {
	ID     *string `json:"id"`
	Path   string  `json:"path"`
	Branch *string `json:"branch"`
}

type stopLoopInput struct {
	LoopID string
	Reason string
}

type stopLoopResponse struct {
	Stopped bool   `json:"stopped"`
	LoopID  string `json:"loopId"`
}

type projectService interface {
	List(context.Context) ([]storage.ProjectRecord, error)
	AddProject(context.Context, projects.AddInput) (projects.AddResult, error)
	RemoveProject(context.Context, string) (storage.ProjectRecord, error)
}

func (h *Handler) buildProjectsRouteResponse(r *http.Request) (any, error) {
	service := h.context.ProjectsService
	if service == nil {
		runtimeProjects := h.context.Runtime.Services().Projects
		if runtimeProjects != nil {
			service = runtimeProjects
		}
	}
	if service == nil {
		return nil, apiError{
			code:    pkgapi.ErrorCodeProjectsUnavailable,
			status:  http.StatusInternalServerError,
			message: "Project management is not available in this runtime",
		}
	}

	switch r.Method {
	case http.MethodGet:
		items, err := service.List(r.Context())
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}

		responseItems := make([]projectResponse, 0, len(items))
		for _, item := range items {
			responseItems = append(responseItems, serializeProject(item, h.context.Config, h.context.Config.Defaults.BaseBranch))
		}
		return projectsListResponse{Items: responseItems}, nil
	case http.MethodPost:
		return h.buildCreateProjectResponse(r, service)
	default:
		return nil, apiError{
			code:    pkgapi.ErrorCodeMethodNotAllowed,
			status:  http.StatusMethodNotAllowed,
			message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/projects"),
		}
	}
}

func (h *Handler) buildProjectRouteResponse(r *http.Request, path string) (any, error) {
	service := h.context.ProjectsService
	if service == nil {
		runtimeProjects := h.context.Runtime.Services().Projects
		if runtimeProjects != nil {
			service = runtimeProjects
		}
	}
	if service == nil {
		return nil, apiError{
			code:    pkgapi.ErrorCodeProjectsUnavailable,
			status:  http.StatusInternalServerError,
			message: "Project management is not available in this runtime",
		}
	}

	identifier, err := decodeProjectIdentifier(normalizePath(r.URL.EscapedPath()))
	if err != nil {
		return nil, err
	}
	if r.Method != http.MethodDelete {
		return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
	}

	removed, err := service.RemoveProject(r.Context(), identifier)
	if err != nil {
		var notFound projects.ProjectNotFoundError
		var ambiguous projects.AmbiguousProjectIdentifierError
		var validation projects.ProjectValidationError
		switch {
		case errors.As(err, &notFound):
			return nil, apiError{code: pkgapi.ErrorCodeProjectNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Project not found: %s", notFound.Identifier)}
		case errors.As(err, &ambiguous):
			return nil, apiError{code: pkgapi.ErrorCodeProjectAmbiguous, status: http.StatusConflict, message: err.Error()}
		case errors.As(err, &validation):
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
		default:
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	}
	return serializeProject(removed, h.context.Config, h.context.Config.Defaults.BaseBranch), nil
}

func (h *Handler) buildLoopsRouteResponse(r *http.Request) (any, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.Loops == nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Loops repository is not configured"}
	}

	switch r.Method {
	case http.MethodGet:
		opts, err := parseLoopsListOptions(r)
		if err != nil {
			return nil, err
		}

		total, err := services.Repositories.Loops.CountFiltered(r.Context(), opts)
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}

		items, err := services.Repositories.Loops.ListFiltered(r.Context(), opts)
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}

		loopIDs := make([]string, 0, len(items))
		for _, item := range items {
			loopIDs = append(loopIDs, item.ID)
		}
		latestQueueByLoopID := map[string]*storage.QueueItemRecord{}
		latestRunByLoopID := map[string]*storage.RunRecord{}
		if len(loopIDs) > 0 && services.Repositories.Queue != nil {
			queues, qErr := services.Repositories.Queue.ListLatestByLoopIDs(r.Context(), loopIDs)
			if qErr != nil {
				return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: qErr.Error()}
			}
			for i := range queues {
				item := &queues[i]
				if item.LoopID == nil || strings.TrimSpace(*item.LoopID) == "" {
					continue
				}
				latestQueueByLoopID[*item.LoopID] = item
			}
		}
		if len(loopIDs) > 0 && services.Repositories.Runs != nil {
			runs, rErr := services.Repositories.Runs.ListLatestByLoopIDs(r.Context(), loopIDs)
			if rErr != nil {
				return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: rErr.Error()}
			}
			for i := range runs {
				run := &runs[i]
				latestRunByLoopID[run.LoopID] = run
			}
		}

		responseItems := make([]loopResponse, 0, len(items))
		for _, item := range items {
			view := serializeLoop(item)
			decorateLoopDiagnostics(&view, latestQueueByLoopID[item.ID], latestRunByLoopID[item.ID])
			responseItems = append(responseItems, view)
		}

		resp := loopsListResponse{
			Items:  responseItems,
			Total:  total,
			Offset: opts.Offset,
		}
		if opts.Limit > 0 {
			limit := opts.Limit
			resp.Limit = &limit
		}
		return resp, nil
	case http.MethodPost:
		return h.buildCreateLoopResponse(r)
	default:
		return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/loops")}
	}
}

func parseLoopsListOptions(r *http.Request) (storage.ListLoopsOptions, error) {
	opts := storage.ListLoopsOptions{
		Status:    strings.TrimSpace(r.URL.Query().Get("status")),
		ProjectID: strings.TrimSpace(r.URL.Query().Get("projectId")),
	}

	if limitValue := strings.TrimSpace(r.URL.Query().Get("limit")); limitValue != "" {
		parsed, err := strconv.ParseInt(limitValue, 10, 64)
		if err != nil || parsed <= 0 {
			return storage.ListLoopsOptions{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "limit must be a positive integer"}
		}
		if parsed > maxLoopsListLimit {
			return storage.ListLoopsOptions{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("limit must be at most %d", maxLoopsListLimit)}
		}
		opts.Limit = parsed
	}

	if offsetValue := strings.TrimSpace(r.URL.Query().Get("offset")); offsetValue != "" {
		parsed, err := strconv.ParseInt(offsetValue, 10, 64)
		if err != nil || parsed < 0 {
			return storage.ListLoopsOptions{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "offset must be a non-negative integer"}
		}
		opts.Offset = parsed
	}

	return opts, nil
}

func (h *Handler) buildRunsRouteResponse(r *http.Request) (runsListResponse, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.Runs == nil {
		return runsListResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Runs repository is not configured"}
	}
	if r.Method != http.MethodGet {
		return runsListResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/runs")}
	}

	loopID := strings.TrimSpace(r.URL.Query().Get("loopId"))
	var (
		runItems []storage.RunRecord
		err      error
	)
	if loopID != "" {
		runItems, err = services.Repositories.Runs.ListByLoop(r.Context(), loopID)
	} else {
		runItems, err = services.Repositories.Runs.List(r.Context())
	}
	if err != nil {
		return runsListResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	items := make([]runResponse, 0, len(runItems))
	for _, item := range runItems {
		items = append(items, serializeRun(item))
	}

	return runsListResponse{Items: items}, nil
}

func (h *Handler) buildReconcileStaleRunsResponse(r *http.Request) (looperdruntime.StaleRunReconcileSummary, error) {
	if r.Method != http.MethodPost {
		return looperdruntime.StaleRunReconcileSummary{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/runs/reconcile-stale")}
	}
	if h.context.ReconcileStaleRuns == nil {
		return looperdruntime.StaleRunReconcileSummary{}, apiError{code: pkgapi.ErrorCodeRuntimeControlUnavailable, status: http.StatusNotImplemented, message: "Runtime control is not available in this process"}
	}
	summary, err := h.context.ReconcileStaleRuns(r.Context())
	if err != nil {
		return looperdruntime.StaleRunReconcileSummary{}, err
	}
	if h.context.TriggerSchedulerTick != nil && summary.LoopsRequeued > 0 {
		h.context.TriggerSchedulerTick()
	}
	return summary, nil
}

func (h *Handler) buildEventsRouteResponse(r *http.Request) (eventsListResponse, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.Events == nil {
		return eventsListResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Events repository is not configured"}
	}
	if r.Method != http.MethodGet {
		return eventsListResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/events")}
	}

	limit := int64(100)
	if limitValue := strings.TrimSpace(r.URL.Query().Get("limit")); limitValue != "" {
		parsed, err := strconv.ParseInt(limitValue, 10, 64)
		if err != nil || parsed <= 0 {
			return eventsListResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "limit must be a positive integer"}
		}
		limit = parsed
	}

	items, err := services.Repositories.Events.List(r.Context(), limit)
	if err != nil {
		return eventsListResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	responseItems := make([]eventResponse, 0, len(items))
	for _, item := range items {
		responseItems = append(responseItems, serializeEvent(item))
	}

	return eventsListResponse{Items: responseItems}, nil
}

func (h *Handler) buildEntityEventsRouteResponse(r *http.Request, path string) (entityEventsResponse, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.Events == nil {
		return entityEventsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Events repository is not configured"}
	}
	if r.Method != http.MethodGet {
		return entityEventsResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
	}

	parts := strings.Split(strings.TrimPrefix(path, apiBasePath+"/events/"), "/")
	entityType, err := decodePathSegment(parts, 0)
	if err != nil {
		return entityEventsResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "entityType and entityId are required"}
	}
	entityID, err := decodePathSegment(parts, 1)
	if err != nil {
		return entityEventsResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "entityType and entityId are required"}
	}
	if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
		return entityEventsResponse{}, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}

	items, err := services.Repositories.Events.ListByEntity(r.Context(), entityType, entityID)
	if err != nil {
		return entityEventsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	responseItems := make([]eventResponse, 0, len(items))
	for _, item := range items {
		responseItems = append(responseItems, serializeEvent(item))
	}

	return entityEventsResponse{EntityType: entityType, EntityID: entityID, Items: responseItems}, nil
}

func (h *Handler) buildPullRequestsRouteResponse(r *http.Request) (pullRequestsListResponse, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.PullRequestSnapshots == nil || services.Repositories.Loops == nil {
		return pullRequestsListResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	if r.Method != http.MethodGet {
		return pullRequestsListResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/pull-requests")}
	}

	snapshots, err := services.Repositories.PullRequestSnapshots.List(r.Context())
	if err != nil {
		return pullRequestsListResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	latestSnapshots := dedupeLatestSnapshots(snapshots)
	loops, err := services.Repositories.Loops.List(r.Context())
	if err != nil {
		return pullRequestsListResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	loopMatchesByPullRequest := groupPullRequestLoops(loops)
	identities := collectPullRequestIdentities(latestSnapshots, loops)
	snapshotByKey := map[string]storage.PullRequestSnapshotRecord{}
	for _, snapshot := range latestSnapshots {
		snapshotByKey[pullRequestKey(snapshot.ProjectID, snapshot.Repo, snapshot.PRNumber)] = snapshot
	}

	items := make([]pullRequestResponse, 0, len(identities))
	for _, identity := range identities {
		key := pullRequestKey(identity.ProjectID, identity.Repo, identity.PRNumber)
		loopMatches := loopMatchesByPullRequest[key]
		snapshot, ok := snapshotByKey[key]
		if ok {
			items = append(items, h.serializePullRequestListItem(identity.Repo, identity.PRNumber, &snapshot, loopMatches))
			continue
		}
		items = append(items, h.serializePullRequestListItem(identity.Repo, identity.PRNumber, nil, loopMatches))
	}

	return pullRequestsListResponse{Items: items}, nil
}

func (h *Handler) buildPullRequestRouteResponse(r *http.Request, path string) (any, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.PullRequestSnapshots == nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	if r.Method != http.MethodGet {
		return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
	}

	rawPath := normalizePath(r.URL.EscapedPath())
	parts := strings.Split(strings.TrimPrefix(rawPath, apiBasePath+"/pull-requests/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo and prNumber are required"}
	}
	repo, err := url.PathUnescape(strings.TrimSpace(parts[0]))
	if err != nil || strings.TrimSpace(repo) == "" {
		return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo and prNumber are required"}
	}
	prNumber, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || prNumber <= 0 {
		return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "prNumber must be a positive integer"}
	}

	projectID := strings.TrimSpace(r.URL.Query().Get("projectId"))
	var snapshot *storage.PullRequestSnapshotRecord
	if projectID != "" {
		snapshot, err = services.Repositories.PullRequestSnapshots.GetLatestByProject(r.Context(), projectID, repo, prNumber)
	} else {
		snapshots, listErr := services.Repositories.PullRequestSnapshots.ListLatestByRepoAndPR(r.Context(), repo, prNumber)
		if listErr != nil {
			err = listErr
		} else {
			matchedProjects := map[string]struct{}{}
			for index := range snapshots {
				candidate := snapshots[index]
				matchedProjects[candidate.ProjectID] = struct{}{}
				if snapshot == nil {
					candidateCopy := candidate
					snapshot = &candidateCopy
				}
			}
			loops, loopErr := services.Repositories.Loops.ListByRepoAndPR(r.Context(), repo, prNumber)
			if loopErr != nil {
				err = loopErr
			} else {
				for _, loop := range loops {
					matchedProjects[loop.ProjectID] = struct{}{}
				}
			}
			if err == nil && len(matchedProjects) > 1 {
				return nil, apiError{code: pkgapi.ErrorCodeProjectAmbiguous, status: http.StatusConflict, message: fmt.Sprintf("Multiple projects match pull request %s#%d; pass projectId explicitly", repo, prNumber)}
			}
		}
	}
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if snapshot == nil {
		return nil, apiError{code: pkgapi.ErrorCodePRNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Pull request not found: %s#%d", repo, prNumber)}
	}

	if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
		if strings.TrimSpace(parts[2]) == "status" {
			if len(parts) > 3 && strings.TrimSpace(parts[3]) != "" {
				return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
			}
			response, err := h.buildPullRequestStatusResponse(r.Context(), *snapshot)
			if err != nil {
				return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
			}
			return response, nil
		}
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}

	loopMatches, err := h.findPullRequestLoops(r.Context(), snapshot.ProjectID, snapshot.Repo, prNumber)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	return h.serializePullRequestListItem(repo, prNumber, snapshot, loopMatches), nil
}

func (h *Handler) buildPullRequestStatusResponse(ctx context.Context, snapshot storage.PullRequestSnapshotRecord) (pullRequestStatusResponse, error) {
	loopMatches, err := h.findPullRequestLoops(ctx, snapshot.ProjectID, snapshot.Repo, snapshot.PRNumber)
	if err != nil {
		return pullRequestStatusResponse{}, err
	}
	runs := make([]storage.RunRecord, 0)
	for _, loop := range loopMatches {
		loopRuns, err := h.context.Runtime.Services().Repositories.Runs.ListByLoop(ctx, loop.ID)
		if err != nil {
			return pullRequestStatusResponse{}, err
		}
		runs = append(runs, loopRuns...)
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].StartedAt != runs[j].StartedAt {
			return runs[i].StartedAt > runs[j].StartedAt
		}
		if runs[i].UpdatedAt != runs[j].UpdatedAt {
			return runs[i].UpdatedAt > runs[j].UpdatedAt
		}
		return runs[i].ID > runs[j].ID
	})

	var latestRunStatus *string
	if len(runs) > 0 {
		latestRunStatus = &runs[0].Status
	}
	runningRunCount := 0
	for _, run := range runs {
		if run.Status == string(domain.RunStatusRunning) {
			runningRunCount++
		}
	}

	unresolvedThreadCount := int64(0)
	if snapshot.UnresolvedThreadCount != nil {
		unresolvedThreadCount = *snapshot.UnresolvedThreadCount
	}

	return pullRequestStatusResponse{
		Repo:                  snapshot.Repo,
		PRNumber:              snapshot.PRNumber,
		ReviewState:           snapshot.ReviewState,
		ChecksSummary:         snapshot.ChecksSummary,
		UnresolvedThreadCount: unresolvedThreadCount,
		CapturedAt:            snapshot.CapturedAt,
		Reviewer:              findLatestLoopStatus(loopMatches, string(domain.LoopTypeReviewer)),
		Fixer:                 findLatestLoopStatus(loopMatches, string(domain.LoopTypeFixer)),
		LoopStatus: pullRequestLoopStatus{
			Loops:           pullRequestLoopStates(loopMatches),
			LatestRunStatus: latestRunStatus,
			RunningRunCount: runningRunCount,
		},
	}, nil
}

func (h *Handler) findPullRequestLoops(ctx context.Context, projectID, repo string, prNumber int64) ([]storage.LoopRecord, error) {
	loops, err := h.context.Runtime.Services().Repositories.Loops.ListByRepoAndPR(ctx, repo, prNumber)
	if err != nil {
		return nil, err
	}
	matches := make([]storage.LoopRecord, 0)
	for _, loop := range loops {
		if loop.ProjectID == projectID {
			matches = append(matches, loop)
		}
	}
	return matches, nil
}

func (h *Handler) serializePullRequestListItem(repo string, prNumber int64, snapshot *storage.PullRequestSnapshotRecord, loopMatches []storage.LoopRecord) pullRequestResponse {
	var projectID *string
	if snapshot != nil {
		projectID = &snapshot.ProjectID
	} else if len(loopMatches) > 0 {
		projectID = &loopMatches[0].ProjectID
	}

	unresolvedThreadCount := int64(0)
	if snapshot != nil && snapshot.UnresolvedThreadCount != nil {
		unresolvedThreadCount = *snapshot.UnresolvedThreadCount
	}
	actionability := derivePullRequestActionability(snapshot)

	return pullRequestResponse{
		Repo:                  repo,
		PRNumber:              prNumber,
		ProjectID:             projectID,
		HeadSHA:               snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return &s.HeadSHA }),
		BaseSHA:               snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return s.BaseSHA }),
		Title:                 snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return s.Title }),
		Body:                  snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return s.Body }),
		Author:                snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return s.Author }),
		DiffRef:               snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return s.DiffRef }),
		ChecksSummary:         snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return s.ChecksSummary }),
		UnresolvedThreadCount: unresolvedThreadCount,
		ReviewState:           snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return s.ReviewState }),
		Mergeability:          stringPtrOrNil(actionability.mergeability),
		BlockingReason:        stringPtrOrNil(actionability.blockingReason),
		IsDraft:               actionability.isDraft,
		HasConflicts:          actionability.hasConflicts,
		CapturedAt:            snapshotString(snapshot, func(s storage.PullRequestSnapshotRecord) *string { return &s.CapturedAt }),
		Reviewer:              findLatestLoopStatus(loopMatches, string(domain.LoopTypeReviewer)),
		Fixer:                 findLatestLoopStatus(loopMatches, string(domain.LoopTypeFixer)),
	}
}

type pullRequestActionability struct {
	mergeability   string
	blockingReason string
	isDraft        *bool
	hasConflicts   *bool
}

func derivePullRequestActionability(snapshot *storage.PullRequestSnapshotRecord) pullRequestActionability {
	if snapshot == nil {
		return pullRequestActionability{mergeability: "unknown", blockingReason: "no snapshot"}
	}

	detail := pullRequestSnapshotDetail(snapshot.PayloadJSON)
	isDraft := boolPtrIfPresent(detail, "isDraft", "IsDraft")
	hasConflicts := boolPtrIfPresent(detail, "hasConflicts", "HasConflicts")
	if hasConflicts == nil && strings.EqualFold(stringFromMap(detail, "mergeStateStatus"), "DIRTY") {
		hasConflicts = boolPtr(true)
	}

	if isDraft != nil && *isDraft {
		return pullRequestActionability{mergeability: "draft", blockingReason: "draft", isDraft: isDraft, hasConflicts: hasConflicts}
	}
	if hasConflicts != nil && *hasConflicts {
		return pullRequestActionability{mergeability: "blocked", blockingReason: "conflicts", isDraft: isDraft, hasConflicts: hasConflicts}
	}
	if checksBlockMerge(snapshot.ChecksSummary) {
		return pullRequestActionability{mergeability: "blocked", blockingReason: "checks", isDraft: isDraft, hasConflicts: hasConflicts}
	}
	if checksPending(snapshot.ChecksSummary) {
		return pullRequestActionability{mergeability: "waiting", blockingReason: "checks pending", isDraft: isDraft, hasConflicts: hasConflicts}
	}
	if reviewBlocksMerge(snapshot.ReviewState) {
		return pullRequestActionability{mergeability: "blocked", blockingReason: "review", isDraft: isDraft, hasConflicts: hasConflicts}
	}
	if reviewPending(snapshot.ReviewState) {
		return pullRequestActionability{mergeability: "waiting", blockingReason: "review pending", isDraft: isDraft, hasConflicts: hasConflicts}
	}
	return pullRequestActionability{mergeability: "ready", blockingReason: "", isDraft: isDraft, hasConflicts: hasConflicts}
}

func pullRequestSnapshotDetail(payload *string) map[string]any {
	if payload == nil || strings.TrimSpace(*payload) == "" {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(*payload), &parsed); err != nil {
		return nil
	}
	detail, _ := parsed["detail"].(map[string]any)
	return detail
}

func checksBlockMerge(summary *string) bool {
	if summary == nil {
		return false
	}
	lower := strings.ToLower(*summary)
	return strings.Contains(lower, "failure") || strings.Contains(lower, "failed") || strings.Contains(lower, "error") || strings.Contains(lower, "cancel")
}

func checksPending(summary *string) bool {
	if summary == nil {
		return false
	}
	lower := strings.ToLower(*summary)
	return strings.Contains(lower, "pending") || strings.Contains(lower, "queued") || strings.Contains(lower, "in_progress") || strings.Contains(lower, "unknown")
}

func reviewBlocksMerge(state *string) bool {
	return state != nil && strings.EqualFold(strings.TrimSpace(*state), "CHANGES_REQUESTED")
}

func reviewPending(state *string) bool {
	if state == nil {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(*state)) {
	case "", "APPROVED":
		return false
	default:
		return true
	}
}

func boolPtrIfPresent(values map[string]any, keys ...string) *bool {
	for _, key := range keys {
		value, ok := values[key].(bool)
		if ok {
			return boolPtr(value)
		}
	}
	return nil
}

func stringFromMap(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func boolPtr(value bool) *bool {
	return &value
}

func snapshotString(snapshot *storage.PullRequestSnapshotRecord, getter func(storage.PullRequestSnapshotRecord) *string) *string {
	if snapshot == nil {
		return nil
	}
	return getter(*snapshot)
}

func pullRequestKey(projectID, repo string, prNumber int64) string {
	return fmt.Sprintf("%s:%s#%d", projectID, repo, prNumber)
}

func groupPullRequestLoops(loops []storage.LoopRecord) map[string][]storage.LoopRecord {
	grouped := make(map[string][]storage.LoopRecord)
	for _, loop := range loops {
		if loop.Repo == nil || loop.PRNumber == nil {
			continue
		}
		key := pullRequestKey(loop.ProjectID, *loop.Repo, *loop.PRNumber)
		grouped[key] = append(grouped[key], loop)
	}
	return grouped
}

func dedupeLatestSnapshots(snapshots []storage.PullRequestSnapshotRecord) []storage.PullRequestSnapshotRecord {
	seen := map[string]struct{}{}
	deduped := make([]storage.PullRequestSnapshotRecord, 0, len(snapshots))
	for _, snapshot := range snapshots {
		key := pullRequestKey(snapshot.ProjectID, snapshot.Repo, snapshot.PRNumber)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, snapshot)
	}
	return deduped
}

type pullRequestIdentity struct {
	Repo      string
	PRNumber  int64
	ProjectID string
}

func collectPullRequestIdentities(snapshots []storage.PullRequestSnapshotRecord, loops []storage.LoopRecord) []pullRequestIdentity {
	seen := map[string]struct{}{}
	identities := make([]pullRequestIdentity, 0)
	appendIdentity := func(repo *string, prNumber *int64, projectID string) {
		if repo == nil || prNumber == nil {
			return
		}
		key := pullRequestKey(projectID, *repo, *prNumber)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		identities = append(identities, pullRequestIdentity{Repo: *repo, PRNumber: *prNumber, ProjectID: projectID})
	}

	for _, snapshot := range snapshots {
		repo := snapshot.Repo
		prNumber := snapshot.PRNumber
		appendIdentity(&repo, &prNumber, snapshot.ProjectID)
	}
	for _, loop := range loops {
		appendIdentity(loop.Repo, loop.PRNumber, loop.ProjectID)
	}
	return identities
}

func findLatestLoopStatus(loops []storage.LoopRecord, loopType string) *string {
	for _, loop := range loops {
		if loop.Type == loopType {
			status := loop.Status
			return &status
		}
	}
	return nil
}

func pullRequestLoopStates(loops []storage.LoopRecord) []string {
	items := make([]string, 0, len(loops))
	for _, loop := range loops {
		items = append(items, loop.Status)
	}
	return items
}

func serializeEvent(event storage.EventLogRecord) eventResponse {
	return eventResponse{
		ID:               event.ID,
		EventType:        event.EventType,
		ProjectID:        event.ProjectID,
		LoopID:           event.LoopID,
		RunID:            event.RunID,
		EntityType:       event.EntityType,
		EntityID:         event.EntityID,
		CorrelationID:    event.CorrelationID,
		CausationID:      event.CausationID,
		ActorType:        event.ActorType,
		ActorID:          event.ActorID,
		ActorDisplayName: event.ActorDisplayName,
		PayloadJSON:      event.PayloadJSON,
		CreatedAt:        event.CreatedAt,
		Payload:          parsePayloadJSON(event.PayloadJSON),
	}
}

func parsePayloadJSON(payloadJSON string) any {
	var parsed any
	if err := json.Unmarshal([]byte(payloadJSON), &parsed); err != nil {
		return payloadJSON
	}
	return parsed
}

func decodePathSegment(parts []string, index int) (string, error) {
	if index >= len(parts) {
		return "", fmt.Errorf("missing path segment")
	}
	segment := strings.TrimSpace(parts[index])
	if segment == "" {
		return "", fmt.Errorf("missing path segment")
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return "", err
	}
	decoded = strings.TrimSpace(decoded)
	if decoded == "" {
		return "", fmt.Errorf("missing path segment")
	}
	return decoded, nil
}

func decodeProjectIdentifier(path string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(path, apiBasePath+"/projects/"), "/")
	if len(parts) != 1 {
		return "", apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}
	identifier, err := decodePathSegment(parts, 0)
	if err != nil {
		return "", apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "project identifier is required"}
	}
	return identifier, nil
}

func (h *Handler) buildActiveRunsResponse(r *http.Request) (activeRunsListResponse, error) {
	if r.Method != http.MethodGet {
		return activeRunsListResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/runs/active")}
	}

	query, err := readActiveRunsQuery(r.URL.Query())
	if err != nil {
		return activeRunsListResponse{}, err
	}

	items, err := h.buildActiveRunViews(r.Context(), true, query.All || query.Status != "")
	if err != nil {
		return activeRunsListResponse{}, err
	}

	filtered := make([]activeRunView, 0, len(items))
	for _, item := range items {
		if matchesActiveRunQuery(item, query) {
			filtered = append(filtered, item)
		}
	}

	return activeRunsListResponse{Items: filtered}, nil
}

func (h *Handler) buildActiveRunRouteResponse(r *http.Request, path string) (any, error) {
	parts := strings.Split(strings.TrimPrefix(path, apiBasePath+"/runs/active/"), "/")
	selector, err := urlPathSegment(parts, 0)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "run selector is required"}
	}
	if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}

	if selector == "stop-all" {
		if len(parts) != 1 {
			return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
		}
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		if h.context.StopAll == nil {
			return nil, apiError{code: pkgapi.ErrorCodeRuntimeControlUnavailable, status: http.StatusNotImplemented, message: "Runtime control is not available in this process"}
		}
		return h.context.StopAll(r.Context(), "Stopped by user via selector all")
	}

	if len(parts) == 1 || strings.TrimSpace(parts[1]) == "" {
		if r.Method != http.MethodGet {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		loop, err := h.resolveLoop(r.Context(), selector)
		if err != nil {
			return nil, err
		}
		return h.buildActiveRunDetailResponse(r.Context(), loop.ID)
	}

	switch parts[1] {
	case "stop":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		if h.context.StopLoop == nil {
			return nil, apiError{code: pkgapi.ErrorCodeRuntimeControlUnavailable, status: http.StatusNotImplemented, message: "Runtime control is not available in this process"}
		}
		loop, err := h.resolveLoop(r.Context(), selector)
		if err != nil {
			return nil, err
		}
		return h.context.StopLoop(r.Context(), loop.ID, fmt.Sprintf("Stopped by user via selector %s", selector))
	case "close":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		if h.context.CloseLoop == nil {
			return nil, apiError{code: pkgapi.ErrorCodeRuntimeControlUnavailable, status: http.StatusNotImplemented, message: "Runtime control is not available in this process"}
		}
		loop, err := h.resolveLoop(r.Context(), selector)
		if err != nil {
			return nil, err
		}
		return h.context.CloseLoop(r.Context(), loop.ID, fmt.Sprintf("Closed by user via selector %s", selector))
	default:
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}
}

func (h *Handler) buildRunRouteResponse(r *http.Request, path string) (any, error) {
	parts := strings.Split(strings.TrimPrefix(path, apiBasePath+"/runs/"), "/")
	runID, err := urlPathSegment(parts, 0)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "run id is required"}
	}
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != "logs" {
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}
	if r.Method != http.MethodGet {
		return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
	}

	return h.buildRunLogsResponse(r.Context(), runID)
}

func (h *Handler) buildActiveRunDetailResponse(ctx context.Context, loopID string) (activeRunView, error) {
	items, err := h.buildActiveRunViews(ctx, true, false)
	if err != nil {
		return activeRunView{}, err
	}
	for _, item := range items {
		if item.LoopID == loopID {
			return item, nil
		}
	}
	return activeRunView{}, apiError{code: pkgapi.ErrorCodeActiveRunNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Active run not found for loop: %s", loopID)}
}

func (h *Handler) buildActiveRunViews(ctx context.Context, includeRunningLoopsWithoutRuns bool, includeInactiveLoops bool) ([]activeRunView, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.Runs == nil || services.Repositories.Loops == nil || services.Repositories.Queue == nil || services.Repositories.AgentExecutions == nil || services.Repositories.Projects == nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}

	activeRuns, err := services.Repositories.Runs.ListByStatus(ctx, string(domain.RunStatusRunning))
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	activeExecutions, err := services.Repositories.AgentExecutions.ListActive(ctx)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	queueItems := make([]storage.QueueItemRecord, 0)
	loopsList := make([]storage.LoopRecord, 0)
	latestRunsByLoopID := map[string]*storage.RunRecord{}
	if includeInactiveLoops {
		queueItems, err = services.Repositories.Queue.List(ctx)
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		loopsList, err = services.Repositories.Loops.List(ctx)
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		latestRunsByLoopID, err = h.latestRunsByLoopID(ctx, services.Repositories.Runs, loopIDsFromLoops(loopsList))
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	} else {
		queueItems, err = services.Repositories.Queue.ListLatestByLoopStatuses(ctx, []string{"queued", "running", "manual_intervention"})
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		manualResumeRuns, err := services.Repositories.Runs.ListLatestByLoopStatusesAndResumePolicy(ctx, manualResumeCandidateLoopStatuses(), string(loops.ResumePolicyManualIntervention))
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		loopsList, err = h.listDefaultActiveRunLoops(ctx, services.Repositories, activeRuns, queueItems, manualResumeRuns, includeRunningLoopsWithoutRuns)
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		latestRunsByLoopID, err = h.latestRunsForDefaultActiveRuns(ctx, services.Repositories, activeRuns, queueItems, manualResumeRuns, loopsList)
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	}

	loopsByID := make(map[string]storage.LoopRecord, len(loopsList))
	for _, loop := range loopsList {
		loopsByID[loop.ID] = loop
	}
	projectNamesByID, err := loadProjectNamesByIDForActiveRunTargets(ctx, services.Repositories.Projects, loopsList)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	queuedLoopIDs := make(map[string]struct{})
	latestQueueByLoopID := latestQueueItemByLoopID(queueItems)
	for _, item := range queueItems {
		if item.LoopID != nil && (item.Status == "queued" || item.Status == "running") {
			queuedLoopIDs[*item.LoopID] = struct{}{}
		}
	}

	queuedLoops := make([]storage.LoopRecord, 0)
	for _, loop := range loopsList {
		if loop.Status == string(domain.LoopStatusQueued) {
			if _, ok := queuedLoopIDs[loop.ID]; ok {
				queuedLoops = append(queuedLoops, loop)
			}
		}
	}

	verifiedActiveAgentByRunID := buildVerifiedActiveAgentByRunID(ctx, h.context.Runtime, activeExecutions)
	activeAgentByRunID := buildActiveAgentByRunID(activeExecutions)
	plausiblyLiveRunningLoopIDs := make(map[string]struct{}, len(activeRuns))
	runningViews := make([]activeRunView, 0, len(activeRuns))
	for _, run := range activeRuns {
		loop, ok := loopsByID[run.LoopID]
		if !ok {
			continue
		}
		latestRun := latestRunsByLoopID[run.LoopID]
		hasActiveAgent := verifiedActiveAgentByRunID[run.ID] != nil
		if !isPlausiblyLiveActiveRun(run, loop, latestRun, hasActiveAgent, h.now().UTC()) {
			continue
		}
		plausiblyLiveRunningLoopIDs[run.LoopID] = struct{}{}
		target, ok, err := h.tryBuildActiveRunTarget(loop, projectNamesByID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		runID := run.ID
		view := activeRunView{
			Seq:         loop.Seq,
			RunID:       &runID,
			LoopID:      run.LoopID,
			ProjectID:   loop.ProjectID,
			Type:        loop.Type,
			Status:      run.Status,
			LoopStatus:  loop.Status,
			CurrentStep: run.CurrentStep,
			StartedAt:   stringPtrOrNil(run.StartedAt),
			Target:      target,
			Agent:       preferredActiveRunAgent(verifiedActiveAgentByRunID[run.ID], activeAgentByRunID[run.ID]),
			Worktree:    buildWorktreeSummary(loop, run),
		}
		decorateActiveRunView(&view, loop, latestQueueByLoopID[loop.ID], latestRun, h.now().UTC())
		runningViews = append(runningViews, view)
	}

	runningLoopsWithoutRuns := make([]storage.LoopRecord, 0)
	if includeRunningLoopsWithoutRuns {
		for _, loop := range loopsList {
			if loop.Status != string(domain.LoopStatusRunning) {
				continue
			}
			if _, ok := plausiblyLiveRunningLoopIDs[loop.ID]; ok {
				continue
			}
			if !includeInactiveLoops && !runningLoopWithoutRunIsFresh(loop, h.now().UTC(), activeRunHeartbeatTTL) {
				continue
			}
			runningLoopsWithoutRuns = append(runningLoopsWithoutRuns, loop)
		}
	}

	queuedViews := make([]activeRunView, 0, len(queuedLoops))
	for _, loop := range queuedLoops {
		target, ok, err := h.tryBuildActiveRunTarget(loop, projectNamesByID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		startedAt := firstNonEmptyString(loop.NextRunAt, stringPtrOrNil(loop.UpdatedAt), stringPtrOrNil(loop.CreatedAt))
		view := activeRunView{
			Seq:         loop.Seq,
			RunID:       nil,
			LoopID:      loop.ID,
			ProjectID:   loop.ProjectID,
			Type:        loop.Type,
			Status:      loop.Status,
			LoopStatus:  loop.Status,
			CurrentStep: nil,
			StartedAt:   startedAt,
			Target:      target,
			Agent:       nil,
			Worktree:    nil,
		}
		decorateActiveRunView(&view, loop, latestQueueByLoopID[loop.ID], latestRunsByLoopID[loop.ID], h.now().UTC())
		queuedViews = append(queuedViews, view)
	}

	runningLoopViews := make([]activeRunView, 0, len(runningLoopsWithoutRuns))
	for _, loop := range runningLoopsWithoutRuns {
		target, ok, err := h.tryBuildActiveRunTarget(loop, projectNamesByID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		startedAt := firstNonEmptyString(loop.LastRunAt, loop.NextRunAt, stringPtrOrNil(loop.UpdatedAt), stringPtrOrNil(loop.CreatedAt))
		view := activeRunView{
			Seq:         loop.Seq,
			RunID:       nil,
			LoopID:      loop.ID,
			ProjectID:   loop.ProjectID,
			Type:        loop.Type,
			Status:      loop.Status,
			LoopStatus:  loop.Status,
			CurrentStep: nil,
			StartedAt:   startedAt,
			Target:      target,
			Agent:       nil,
			Worktree:    nil,
		}
		decorateActiveRunView(&view, loop, latestQueueByLoopID[loop.ID], latestRunsByLoopID[loop.ID], h.now().UTC())
		runningLoopViews = append(runningLoopViews, view)
	}

	includedLoopIDs := make(map[string]struct{}, len(runningViews)+len(runningLoopViews)+len(queuedViews))
	for _, item := range runningViews {
		includedLoopIDs[item.LoopID] = struct{}{}
	}
	for _, item := range runningLoopViews {
		includedLoopIDs[item.LoopID] = struct{}{}
	}
	for _, item := range queuedViews {
		includedLoopIDs[item.LoopID] = struct{}{}
	}

	inactiveLoopViews := make([]activeRunView, 0)
	for _, loop := range loopsList {
		if _, ok := includedLoopIDs[loop.ID]; ok {
			continue
		}
		latestQueue := latestQueueByLoopID[loop.ID]
		latestRun := latestRunsByLoopID[loop.ID]
		if !includeInactiveLoops {
			// Closed loops are not actionable even when their latest queue item
			// remains manual_intervention after looper close (issue #561).
			if isClosedLoopStatus(loop.Status) {
				continue
			}
			if !isManualInterventionQueue(latestQueue) && !hasManualInterventionResumePolicy(latestRun) {
				continue
			}
		}
		target, ok, err := h.tryBuildActiveRunTarget(loop, projectNamesByID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var (
			runID    *string
			worktree *activeRunWorktree
		)
		startedAt := firstNonEmptyString(loop.LastRunAt, loop.NextRunAt, stringPtrOrNil(loop.UpdatedAt), stringPtrOrNil(loop.CreatedAt))
		var endedAt *string
		if latestRun != nil {
			runID = &latestRun.ID
			startedAt = stringPtrOrNil(latestRun.StartedAt)
			endedAt = latestRun.EndedAt
			worktree = buildWorktreeSummary(loop, *latestRun)
		}
		view := activeRunView{
			Seq:         loop.Seq,
			RunID:       runID,
			LoopID:      loop.ID,
			ProjectID:   loop.ProjectID,
			Type:        loop.Type,
			Status:      loop.Status,
			LoopStatus:  loop.Status,
			CurrentStep: nil,
			StartedAt:   startedAt,
			EndedAt:     endedAt,
			Target:      target,
			Agent:       nil,
			Worktree:    worktree,
		}
		decorateActiveRunView(&view, loop, latestQueue, latestRun, h.now().UTC())
		inactiveLoopViews = append(inactiveLoopViews, view)
	}

	items := append(runningViews, runningLoopViews...)
	queuedViews = excludeActiveRunViewsByLoopID(queuedViews, items)
	items = append(items, queuedViews...)
	items = append(items, inactiveLoopViews...)
	sort.Slice(items, func(i, j int) bool {
		return compareActiveRunViews(items[i], items[j]) < 0
	})
	return items, nil
}

func excludeActiveRunViewsByLoopID(items []activeRunView, excluded []activeRunView) []activeRunView {
	if len(items) == 0 || len(excluded) == 0 {
		return items
	}
	excludedLoopIDs := make(map[string]struct{}, len(excluded))
	for _, item := range excluded {
		excludedLoopIDs[item.LoopID] = struct{}{}
	}
	filtered := items[:0]
	for _, item := range items {
		if _, ok := excludedLoopIDs[item.LoopID]; ok {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func (h *Handler) listDefaultActiveRunLoops(ctx context.Context, repos *storage.Repositories, activeRuns []storage.RunRecord, queueItems []storage.QueueItemRecord, manualResumeRuns []storage.RunRecord, includeRunningLoopsWithoutRuns bool) ([]storage.LoopRecord, error) {
	loopIDs := make(map[string]struct{}, len(activeRuns)+len(queueItems)+len(manualResumeRuns))
	for _, run := range activeRuns {
		loopIDs[run.LoopID] = struct{}{}
	}
	for _, item := range queueItems {
		if item.LoopID != nil && strings.TrimSpace(*item.LoopID) != "" {
			loopIDs[*item.LoopID] = struct{}{}
		}
	}
	for _, run := range manualResumeRuns {
		loopIDs[run.LoopID] = struct{}{}
	}
	if includeRunningLoopsWithoutRuns {
		runningAndQueued, err := repos.Loops.ListByStatuses(ctx, []string{string(domain.LoopStatusRunning), string(domain.LoopStatusQueued)})
		if err != nil {
			return nil, err
		}
		for _, loop := range runningAndQueued {
			loopIDs[loop.ID] = struct{}{}
		}
	}
	return repos.Loops.ListByIDs(ctx, mapKeys(loopIDs))
}

func (h *Handler) latestRunsForDefaultActiveRuns(ctx context.Context, repos *storage.Repositories, activeRuns []storage.RunRecord, queueItems []storage.QueueItemRecord, manualResumeRuns []storage.RunRecord, loopsList []storage.LoopRecord) (map[string]*storage.RunRecord, error) {
	loopIDs := make(map[string]struct{}, len(activeRuns)+len(queueItems)+len(manualResumeRuns)+len(loopsList))
	for _, run := range activeRuns {
		loopIDs[run.LoopID] = struct{}{}
	}
	for _, item := range queueItems {
		if item.LoopID != nil && strings.TrimSpace(*item.LoopID) != "" {
			loopIDs[*item.LoopID] = struct{}{}
		}
	}
	for _, loop := range loopsList {
		loopIDs[loop.ID] = struct{}{}
	}
	latestRunsByLoopID, err := h.latestRunsByLoopID(ctx, repos.Runs, mapKeys(loopIDs))
	if err != nil {
		return nil, err
	}
	for i := range manualResumeRuns {
		latestRunsByLoopID[manualResumeRuns[i].LoopID] = &manualResumeRuns[i]
	}
	return latestRunsByLoopID, nil
}

func manualResumeCandidateLoopStatuses() []string {
	return []string{
		string(domain.LoopStatusPaused),
		string(domain.LoopStatusWaiting),
		string(domain.LoopStatusFailed),
		string(domain.LoopStatusInterrupted),
	}
}

func (h *Handler) latestRunsByLoopID(ctx context.Context, runs *storage.RunsRepository, loopIDs []string) (map[string]*storage.RunRecord, error) {
	latestRuns, err := runs.ListLatestByLoopIDs(ctx, loopIDs)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*storage.RunRecord, len(latestRuns))
	for i := range latestRuns {
		result[latestRuns[i].LoopID] = &latestRuns[i]
	}
	return result, nil
}

func loopIDsFromLoops(loopsList []storage.LoopRecord) []string {
	ids := make([]string, 0, len(loopsList))
	for _, loop := range loopsList {
		ids = append(ids, loop.ID)
	}
	return ids
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func latestQueueItemByLoopID(items []storage.QueueItemRecord) map[string]*storage.QueueItemRecord {
	latest := make(map[string]*storage.QueueItemRecord)
	for i := range items {
		item := &items[i]
		if item.LoopID == nil || strings.TrimSpace(*item.LoopID) == "" {
			continue
		}
		loopID := *item.LoopID
		current := latest[loopID]
		if current == nil || item.UpdatedAt > current.UpdatedAt || (item.UpdatedAt == current.UpdatedAt && item.ID > current.ID) {
			latest[loopID] = item
		}
	}
	return latest
}

func isManualInterventionQueue(item *storage.QueueItemRecord) bool {
	return item != nil && (item.Status == "manual_intervention" || (item.LastErrorKind != nil && *item.LastErrorKind == "manual_intervention"))
}

func hasManualInterventionResumePolicy(run *storage.RunRecord) bool {
	policy := resumePolicyFromRun(run)
	return policy != nil && *policy == loops.ResumePolicyManualIntervention
}

// isClosedLoopStatus reports loop statuses that are fully finished and must not
// appear in the default active-run listing (looper ps). Failed/interrupted loops
// remain eligible when parked for manual intervention so operators can retry.
func isClosedLoopStatus(status string) bool {
	switch domain.LoopStatus(status) {
	case domain.LoopStatusTerminated, domain.LoopStatusCompleted, domain.LoopStatusStopped:
		return true
	default:
		return false
	}
}

func decorateActiveRunView(view *activeRunView, loop storage.LoopRecord, latestQueue *storage.QueueItemRecord, latestRun *storage.RunRecord, now time.Time) {
	if view.LoopStatus == "" {
		view.LoopStatus = loop.Status
	}
	view.DisplayStatus = view.Status
	if latestQueue != nil {
		attempts := latestQueue.Attempts
		maxAttempts := latestQueue.MaxAttempts
		view.Attempts = &attempts
		view.MaxAttempts = &maxAttempts
		view.LastFailureKind = latestQueue.LastErrorKind
		view.LastFailureReason = latestQueue.LastError
	}
	// Fall back to the latest run error/summary when no queue error is present
	// (e.g. checkpoint-only manual holds or failed runs without a parked queue item).
	// Summary is only used for failure-like runs: successful completeRun summaries
	// must not appear as lastFailureReason on queued/running or --all listings.
	if view.LastFailureReason == nil || strings.TrimSpace(*view.LastFailureReason) == "" {
		if latestRun != nil {
			if latestRun.ErrorMessage != nil && strings.TrimSpace(*latestRun.ErrorMessage) != "" {
				view.LastFailureReason = latestRun.ErrorMessage
			} else if shouldUseRunSummaryAsFailureReason(latestRun) && latestRun.Summary != nil && strings.TrimSpace(*latestRun.Summary) != "" {
				view.LastFailureReason = latestRun.Summary
			}
		}
	}
	view.ResumePolicy = resumePolicyFromRun(latestRun)
	// Do not override a closed loop's status with manual_intervention: the loop
	// is no longer actionable even if the latest queue item still has that status.
	if !isClosedLoopStatus(loop.Status) && (isManualInterventionQueue(latestQueue) || (view.ResumePolicy != nil && *view.ResumePolicy == loops.ResumePolicyManualIntervention)) {
		view.DisplayStatus = "manual_intervention"
	} else if isBackingOffQueue(latestQueue, now) {
		view.DisplayStatus = "backing_off"
	}
	if view.DisplayStatus == "" {
		view.DisplayStatus = view.Status
	}
}

// decorateLoopDiagnostics attaches latest-queue attempt counts and failure reason
// (with the same run fallback as active-run views) for dashboard list/detail.
func decorateLoopDiagnostics(view *loopResponse, latestQueue *storage.QueueItemRecord, latestRun *storage.RunRecord) {
	if view == nil {
		return
	}
	if latestQueue != nil {
		attempts := latestQueue.Attempts
		maxAttempts := latestQueue.MaxAttempts
		view.Attempts = &attempts
		view.MaxAttempts = &maxAttempts
		view.LastFailureKind = latestQueue.LastErrorKind
		view.LastFailureReason = latestQueue.LastError
	}
	if view.LastFailureReason == nil || strings.TrimSpace(*view.LastFailureReason) == "" {
		if latestRun != nil {
			if latestRun.ErrorMessage != nil && strings.TrimSpace(*latestRun.ErrorMessage) != "" {
				view.LastFailureReason = latestRun.ErrorMessage
			} else if shouldUseRunSummaryAsFailureReason(latestRun) && latestRun.Summary != nil && strings.TrimSpace(*latestRun.Summary) != "" {
				view.LastFailureReason = latestRun.Summary
			}
		}
	}
}

// shouldUseRunSummaryAsFailureReason reports whether a run's Summary is safe to
// surface as lastFailureReason. Successful completeRun summaries must not be
// treated as failures for queued/running loops or ps --all completed rows.
func shouldUseRunSummaryAsFailureReason(run *storage.RunRecord) bool {
	if run == nil {
		return false
	}
	switch domain.RunStatus(strings.TrimSpace(run.Status)) {
	case domain.RunStatusFailed, domain.RunStatusInterrupted, domain.RunStatusParseFailed:
		return true
	default:
		return false
	}
}

func isBackingOffQueue(item *storage.QueueItemRecord, now time.Time) bool {
	if item == nil || item.Status != "queued" || item.LastErrorKind == nil {
		return false
	}
	switch strings.TrimSpace(*item.LastErrorKind) {
	case "retryable_transient", "retryable_after_resume":
	default:
		return false
	}
	availableAt := strings.TrimSpace(item.AvailableAt)
	if availableAt == "" {
		return false
	}
	availableTime, err := time.Parse(time.RFC3339Nano, availableAt)
	if err != nil {
		return availableAt > eventlog.FormatJavaScriptISOString(now)
	}
	return availableTime.After(now)
}

func resumePolicyFromRun(run *storage.RunRecord) *string {
	if run == nil || run.CheckpointJSON == nil {
		return nil
	}
	policy := readStringMap(parseJSONObject(run.CheckpointJSON), "resumePolicy")
	if policy == nil || strings.TrimSpace(*policy) == "" {
		return nil
	}
	return policy
}

func buildVerifiedActiveAgentByRunID(ctx context.Context, runtime RuntimeState, executions []storage.AgentExecutionRecord) map[string]*activeRunAgent {
	verifier, _ := runtime.(activeRunExecutionVerifier)
	if verifier == nil {
		return map[string]*activeRunAgent{}
	}
	grouped := groupActiveExecutionsByRunID(executions, true, func(execution storage.AgentExecutionRecord) bool {
		matches, running, err := verifier.ExecutionMatchesProcess(ctx, execution, int(*execution.PID))
		return err == nil && running && matches
	})
	return buildActiveRunAgents(grouped)
}

func buildActiveAgentByRunID(executions []storage.AgentExecutionRecord) map[string]*activeRunAgent {
	return buildActiveRunAgents(groupActiveExecutionsByRunID(executions, false, func(storage.AgentExecutionRecord) bool {
		return true
	}))
}

func groupActiveExecutionsByRunID(executions []storage.AgentExecutionRecord, requirePID bool, include func(storage.AgentExecutionRecord) bool) map[string][]storage.AgentExecutionRecord {
	grouped := make(map[string][]storage.AgentExecutionRecord)
	for _, execution := range executions {
		if execution.RunID == nil || strings.TrimSpace(*execution.RunID) == "" {
			continue
		}
		if requirePID && (execution.PID == nil || *execution.PID <= 0) {
			continue
		}
		if !include(execution) {
			continue
		}
		runID := *execution.RunID
		grouped[runID] = append(grouped[runID], execution)
	}
	return grouped
}

func buildActiveRunAgents(grouped map[string][]storage.AgentExecutionRecord) map[string]*activeRunAgent {
	result := make(map[string]*activeRunAgent, len(grouped))
	for runID, bucket := range grouped {
		sort.Slice(bucket, func(i, j int) bool {
			if bucket[i].StartedAt != bucket[j].StartedAt {
				return bucket[i].StartedAt > bucket[j].StartedAt
			}
			return bucket[i].ID > bucket[j].ID
		})
		primary := bucket[0]
		result[runID] = &activeRunAgent{
			Active:          true,
			ActiveCount:     len(bucket),
			ExecutionID:     primary.ID,
			Vendor:          primary.Vendor,
			PID:             primary.PID,
			StartedAt:       primary.StartedAt,
			LastHeartbeatAt: primary.LastHeartbeatAt,
			HeartbeatCount:  primary.HeartbeatCount,
			Status:          primary.Status,
		}
	}

	return result
}

func preferredActiveRunAgent(verified *activeRunAgent, fallback *activeRunAgent) *activeRunAgent {
	if verified != nil {
		return verified
	}
	return fallback
}

func isPlausiblyLiveActiveRun(run storage.RunRecord, loop storage.LoopRecord, latestRun *storage.RunRecord, hasActiveAgent bool, now time.Time) bool {
	if latestRun == nil || latestRun.ID != run.ID {
		return false
	}
	if !domain.IsActiveLoopStatus(domain.LoopStatus(loop.Status)) {
		return false
	}
	if hasActiveAgent {
		return true
	}
	return runHeartbeatIsRecent(run, now, activeRunHeartbeatTTL)
}

func runHeartbeatIsRecent(run storage.RunRecord, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	heartbeatAt := firstNonEmptyString(run.LastHeartbeatAt, stringPtrOrNil(run.UpdatedAt), stringPtrOrNil(run.StartedAt))
	if heartbeatAt == nil || strings.TrimSpace(*heartbeatAt) == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*heartbeatAt))
	if err != nil {
		return false
	}
	return !parsed.UTC().Before(now.UTC().Add(-ttl))
}

func runningLoopWithoutRunIsFresh(loop storage.LoopRecord, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	activityAt := firstNonEmptyString(loop.LastRunAt, loop.NextRunAt, stringPtrOrNil(loop.UpdatedAt), stringPtrOrNil(loop.CreatedAt))
	if activityAt == nil || strings.TrimSpace(*activityAt) == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*activityAt))
	if err != nil {
		return false
	}
	return !parsed.UTC().Before(now.UTC().Add(-ttl))
}

func loadProjectNamesByIDForActiveRunTargets(ctx context.Context, repo *storage.ProjectsRepository, loopsList []storage.LoopRecord) (map[string]string, error) {
	if !hasProjectActiveRunTarget(loopsList) {
		return map[string]string{}, nil
	}
	projectsList, err := repo.List(ctx)
	if err != nil {
		return nil, err
	}
	return projectNamesByID(projectsList), nil
}

func hasProjectActiveRunTarget(loopsList []storage.LoopRecord) bool {
	for _, loop := range loopsList {
		if loop.TargetType == string(domain.LoopTargetTypeProject) {
			return true
		}
	}
	return false
}

func projectNamesByID(projectsList []storage.ProjectRecord) map[string]string {
	result := make(map[string]string, len(projectsList))
	for _, project := range projectsList {
		result[project.ID] = strings.TrimSpace(project.Name)
	}
	return result
}

func (h *Handler) tryBuildActiveRunTarget(loop storage.LoopRecord, projectNamesByID map[string]string) (activeRunTarget, bool, error) {
	switch loop.TargetType {
	case string(domain.LoopTargetTypeProject):
		projectID := ""
		if loop.TargetID != nil {
			projectID = strings.TrimSpace(*loop.TargetID)
			if strings.HasPrefix(projectID, "project:") {
				projectID = strings.TrimPrefix(projectID, "project:")
			}
		}
		if projectID == "" {
			return activeRunTarget{}, false, nil
		}
		label := projectID
		if name := strings.TrimSpace(projectNamesByID[projectID]); name != "" {
			label = name
		}
		return activeRunTarget{Type: string(domain.LoopTargetTypeProject), ProjectID: &projectID, Label: label}, true, nil
	case string(domain.LoopTargetTypeIssue):
		if loop.Repo == nil || loop.TargetID == nil {
			return activeRunTarget{}, false, nil
		}
		issueNumber, err := parseIssueNumber(*loop.TargetID)
		if err != nil || issueNumber <= 0 {
			return activeRunTarget{}, false, nil
		}
		repo := *loop.Repo
		return activeRunTarget{Type: string(domain.LoopTargetTypeIssue), Repo: &repo, IssueNumber: &issueNumber, Label: fmt.Sprintf("%s#%d", repo, issueNumber)}, true, nil
	default:
		if loop.Repo == nil || loop.PRNumber == nil {
			return activeRunTarget{}, false, nil
		}
		repo := *loop.Repo
		prNumber := *loop.PRNumber
		return activeRunTarget{Type: string(domain.LoopTargetTypePullRequest), Repo: &repo, PRNumber: &prNumber, Label: fmt.Sprintf("%s#%d", repo, prNumber)}, true, nil
	}
}

func readActiveRunsQuery(values url.Values) (activeRunsQuery, error) {
	query := activeRunsQuery{
		All:       strings.EqualFold(strings.TrimSpace(values.Get("all")), "true"),
		Status:    strings.TrimSpace(values.Get("status")),
		Type:      strings.TrimSpace(values.Get("type")),
		ProjectID: strings.TrimSpace(values.Get("projectId")),
		Repo:      strings.TrimSpace(values.Get("repo")),
	}
	if prNumberText := strings.TrimSpace(values.Get("prNumber")); prNumberText != "" {
		prNumber, err := parsePositiveInt64(prNumberText, "prNumber")
		if err != nil {
			return activeRunsQuery{}, err
		}
		query.PRNumber = &prNumber
	}
	if (query.Repo == "") != (query.PRNumber == nil) {
		return activeRunsQuery{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo and prNumber must be provided together"}
	}
	return query, nil
}

func parsePositiveInt64(value, fieldName string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return 0, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s must be a positive integer", fieldName)}
	}
	return parsed, nil
}

func matchesActiveRunQuery(item activeRunView, query activeRunsQuery) bool {
	if query.Status != "" && item.Status != query.Status && item.DisplayStatus != query.Status && item.LoopStatus != query.Status {
		return false
	}
	if query.Type != "" && item.Type != query.Type {
		return false
	}
	if query.ProjectID != "" && item.ProjectID != query.ProjectID {
		return false
	}
	if query.Repo != "" || query.PRNumber != nil {
		if item.Target.Type != string(domain.LoopTargetTypePullRequest) || item.Target.Repo == nil || item.Target.PRNumber == nil {
			return false
		}
		if *item.Target.Repo != query.Repo || *item.Target.PRNumber != *query.PRNumber {
			return false
		}
	}
	return true
}

func compareActiveRunViews(left, right activeRunView) int {
	leftRunning := 0
	if left.Status == string(domain.RunStatusRunning) {
		leftRunning = 1
	}
	rightRunning := 0
	if right.Status == string(domain.RunStatusRunning) {
		rightRunning = 1
	}
	if leftRunning != rightRunning {
		return rightRunning - leftRunning
	}

	leftAgent := 0
	if left.Agent != nil {
		leftAgent = 1
	}
	rightAgent := 0
	if right.Agent != nil {
		rightAgent = 1
	}
	if leftAgent != rightAgent {
		return rightAgent - leftAgent
	}

	leftStarted := derefString(firstNonEmptyString(left.EndedAt, left.StartedAt))
	rightStarted := derefString(firstNonEmptyString(right.EndedAt, right.StartedAt))
	if leftStarted != rightStarted {
		if leftStarted > rightStarted {
			return -1
		}
		return 1
	}

	leftKey := left.LoopID
	if left.RunID != nil {
		leftKey = *left.RunID
	}
	rightKey := right.LoopID
	if right.RunID != nil {
		rightKey = *right.RunID
	}
	if leftKey < rightKey {
		return -1
	}
	if leftKey > rightKey {
		return 1
	}
	return 0
}

func buildWorktreeSummary(loop storage.LoopRecord, run storage.RunRecord) *activeRunWorktree {
	checkpoint := parseJSONObject(run.CheckpointJSON)
	checkpointWorktree := readObject(checkpoint, "worktree")
	loopMetadata := parseJSONObject(loop.MetadataJSON)
	path := firstNonEmptyString(readObjectString(checkpointWorktree, "path"), readStringMap(loopMetadata, "worktreePath"))
	if path == nil {
		return nil
	}
	return &activeRunWorktree{
		ID:     firstNonEmptyString(readObjectString(checkpointWorktree, "id"), readStringMap(loopMetadata, "worktreeId")),
		Path:   *path,
		Branch: firstNonEmptyString(readObjectString(checkpointWorktree, "branch"), readStringMap(loopMetadata, "branch")),
	}
}

func (h *Handler) buildLoopRouteResponse(r *http.Request, path string) (any, error) {
	parts := strings.Split(strings.TrimPrefix(path, apiBasePath+"/loops/"), "/")
	selector, err := urlPathSegment(parts, 0)
	if err != nil {
		return nil, err
	}
	if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}

	loop, err := h.resolveLoop(r.Context(), selector)
	if err != nil {
		return nil, err
	}

	if len(parts) == 1 || strings.TrimSpace(parts[1]) == "" {
		if r.Method != http.MethodGet {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.serializeLoopWithDiagnostics(r.Context(), loop)
	}

	subresource := parts[1]
	switch subresource {
	case "logs":
		if r.Method != http.MethodGet {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.buildLoopLogsResponse(r.Context(), loop)
	case "start":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.mutateLoopStatus(r.Context(), loop.ID, domain.LoopStatusRunning)
	case "pause":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.mutateLoopStatus(r.Context(), loop.ID, domain.LoopStatusPaused)
	case "retry":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.retryLoop(r.Context(), r, loop.ID, false)
	case "respond":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.respondToLoop(r.Context(), r, loop.ID)
	case "takeover":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.takeoverLoop(r.Context(), loop.ID)
	case "handback":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.handbackLoop(r.Context(), r, loop.ID)
	default:
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}
}

func isFollowLoopLogsRequest(r *http.Request, path string) bool {
	if r.Method != http.MethodGet || !strings.HasSuffix(path, "/logs") {
		return false
	}
	value := strings.TrimSpace(r.URL.Query().Get("follow"))
	return value == "1" || strings.EqualFold(value, "true")
}

func (h *Handler) streamLoopLogsRoute(w http.ResponseWriter, r *http.Request, path string, requestID string) error {
	parts := strings.Split(strings.TrimPrefix(path, apiBasePath+"/loops/"), "/")
	selector, err := urlPathSegment(parts, 0)
	if err != nil {
		return err
	}
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != "logs" {
		return apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}

	loop, err := h.resolveLoop(r.Context(), selector)
	if err != nil {
		return err
	}

	return h.streamLoopLogs(w, r, requestID, loop, queryBool(r.URL.Query(), "stderr"))
}

func (h *Handler) streamLoopLogs(w http.ResponseWriter, r *http.Request, requestID string, loop storage.LoopRecord, stderr bool) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Streaming is not supported by this response writer"}
	}

	current, err := h.buildLoopLogsResponse(r.Context(), loop)
	if err != nil {
		return err
	}

	w.Header().Set(requestIDHeaderName, requestID)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := writeSSEEvent(w, flusher, "snapshot", current); err != nil {
		return nil
	}

	observedRunID := ""
	if current.Run != nil {
		observedRunID = current.Run.RunID
	}
	previousExecutionID, previousContent := loopLogsStreamState(current, stderr)
	if shouldTerminateLoopLogsFollow(current, observedRunID) {
		_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
		return nil
	}

	ticker := time.NewTicker(loopLogsFollowPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-ticker.C:
		}

		current, err = h.buildLoopLogsResponse(r.Context(), loop)
		if err != nil {
			continue
		}
		if observedRunID == "" && current.Run != nil {
			observedRunID = current.Run.RunID
		}
		if shouldTerminateLoopLogsFollowBeforeChunk(current, observedRunID) {
			_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
			return nil
		}

		nextExecutionID, nextContent := loopLogsStreamState(current, stderr)
		chunk := appendedLogChunk(previousExecutionID, previousContent, nextExecutionID, nextContent)
		if chunk != "" {
			event := loopLogsFollowChunkEvent{Content: chunk}
			if current.Run != nil {
				event.RunID = &current.Run.RunID
				event.CurrentStep = current.Run.CurrentStep
			}
			if current.Agent != nil {
				event.ExecutionID = &current.Agent.ExecutionID
				event.Vendor = &current.Agent.Vendor
				event.PID = current.Agent.PID
				event.Status = &current.Agent.Status
			}
			if err := writeSSEEvent(w, flusher, "chunk", event); err != nil {
				return nil
			}
		}

		previousExecutionID, previousContent = nextExecutionID, nextContent
		if shouldTerminateLoopLogsFollow(current, observedRunID) {
			_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
			return nil
		}
	}
}

func queryBool(values url.Values, key string) bool {
	value := strings.TrimSpace(values.Get(key))
	return value == "1" || strings.EqualFold(value, "true")
}

func writeSSEEvent(w io.Writer, flusher http.Flusher, event string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func loopLogsStreamState(resp loopLogsResponse, stderr bool) (string, string) {
	if resp.Agent == nil {
		return "", ""
	}
	content := resp.Agent.Stdout
	if stderr || shouldDefaultLoopLogsStreamToStderr(resp) {
		content = resp.Agent.Stderr
	}
	return resp.Agent.ExecutionID, content
}

func shouldDefaultLoopLogsStreamToStderr(resp loopLogsResponse) bool {
	if resp.Agent == nil {
		return false
	}
	return strings.TrimSpace(resp.Agent.Stdout) == "" && strings.TrimSpace(resp.Agent.Stderr) != ""
}

func appendedLogChunk(previousExecutionID, previousContent, currentExecutionID, currentContent string) string {
	if currentExecutionID == "" {
		return ""
	}
	if previousExecutionID == "" || currentExecutionID != previousExecutionID {
		return currentContent
	}
	if currentContent == previousContent {
		return ""
	}
	if strings.HasPrefix(currentContent, previousContent) {
		return currentContent[len(previousContent):]
	}
	return currentContent
}

func shouldTerminateLoopLogsFollow(resp loopLogsResponse, observedRunID string) bool {
	if observedRunID == "" {
		if resp.Run == nil {
			return !domain.IsActiveLoopStatus(domain.LoopStatus(resp.LoopStatus))
		}
		observedRunID = resp.Run.RunID
	}
	if resp.Run == nil {
		return true
	}
	if resp.Run.RunID != observedRunID {
		return true
	}
	return domain.IsTerminalRunStatus(domain.RunStatus(resp.Run.Status))
}

func shouldTerminateLoopLogsFollowBeforeChunk(resp loopLogsResponse, observedRunID string) bool {
	if !shouldTerminateLoopLogsFollow(resp, observedRunID) {
		return false
	}
	if observedRunID == "" {
		return resp.Run == nil
	}
	if resp.Run == nil {
		return true
	}
	return resp.Run.RunID != observedRunID
}

type createLoopRequest struct {
	ProjectID   *string         `json:"projectId"`
	Type        *string         `json:"type"`
	TargetType  *string         `json:"targetType"`
	TargetID    *string         `json:"targetId"`
	Repo        *string         `json:"repo"`
	PRNumber    *int64          `json:"prNumber"`
	IssueNumber *int64          `json:"issueNumber"`
	Status      *string         `json:"status"`
	Force       *bool           `json:"force"`
	Metadata    json.RawMessage `json:"metadata"`
}

type createWorkerRequest struct {
	ProjectID   *string `json:"projectId"`
	Title       *string `json:"title"`
	Prompt      *string `json:"prompt"`
	SpecPath    *string `json:"specPath"`
	Repo        *string `json:"repo"`
	BaseBranch  *string `json:"baseBranch"`
	PRNumber    *int64  `json:"prNumber"`
	IssueNumber *int64  `json:"issueNumber"`
	Force       *bool   `json:"force"`
}

type createPlannerRequest struct {
	ProjectID   *string `json:"projectId"`
	IssueNumber *int64  `json:"issueNumber"`
	Force       *bool   `json:"force"`
}

type workerCreateResponse struct {
	loopResponse
	Title       string  `json:"title"`
	Prompt      *string `json:"prompt"`
	SpecPath    *string `json:"specPath"`
	BaseBranch  string  `json:"baseBranch"`
	IssueNumber *int64  `json:"issueNumber,omitempty"`
	Reused      bool    `json:"reused,omitempty"`
}

type plannerCreateResponse struct {
	loopResponse
	IssueNumber int64 `json:"issueNumber"`
}

func (h *Handler) buildCreateLoopResponse(r *http.Request) (loopResponse, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}

	var body createLoopRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "Request body must be valid JSON"}
	}

	projectID := strings.TrimSpace(derefString(body.ProjectID))
	if projectID == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "projectId is required"}
	}

	loopType := strings.TrimSpace(derefString(body.Type))
	if loopType == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "type is required"}
	}
	if err := domain.AssertKnownLoopType(domain.LoopType(loopType)); err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
	}

	targetType := strings.TrimSpace(derefString(body.TargetType))
	if targetType == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "targetType is required"}
	}

	status := strings.TrimSpace(derefString(body.Status))
	if status == "" {
		status = string(domain.LoopStatusRunning)
	}
	if err := domain.AssertKnownLoopStatus(domain.LoopStatus(status)); err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
	}

	if (loopType == string(domain.LoopTypeReviewer) || loopType == string(domain.LoopTypeFixer) || loopType == string(domain.LoopTypeWorker) || loopType == string(domain.LoopTypePlanner)) && !isCodingRoleAgentConfigured(h.effectiveConfig(), loopType) {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot create %s loop without config.agent.vendor", loopType)}
	}

	target, err := buildLoopTarget(targetType, body)
	if err != nil {
		return loopResponse{}, err
	}
	if err := domain.AssertLoopTypeMatchesTarget(domain.LoopType(loopType), target); err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
	}

	metadataJSON, err := normalizeMetadataJSON(body.Metadata)
	if err != nil {
		return loopResponse{}, err
	}
	if domain.LoopType(loopType) == domain.LoopTypePlanner {
		metadataJSON, err = manualPlannerMetadataJSON(metadataJSON, target.IssueNumber)
		if err != nil {
			return loopResponse{}, err
		}
	}
	now := h.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	if domain.LoopType(loopType) == domain.LoopTypeFixer {
		metadataJSON, err = manualFixerMetadataJSON(metadataJSON, nowISO)
		if err != nil {
			return loopResponse{}, err
		}
	}
	if domain.LoopType(loopType) == domain.LoopTypeReviewer {
		metadataJSON, err = reviewerLoopMetadataJSON(metadataJSON, h.context.Config.Roles.Reviewer.Behavior, target, nowISO, derefBool(body.Force))
		if err != nil {
			return loopResponse{}, err
		}
	}
	if services.Repositories == nil || services.Repositories.Projects == nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	project, err := requireActiveProjectRecord(r.Context(), services.Repositories.Projects, projectID)
	if err != nil {
		return loopResponse{}, err
	}
	if err := validateLoopTargetProjectCompatibility(projectID, parseProjectMetadata(project.MetadataJSON), target); err != nil {
		return loopResponse{}, err
	}
	if err := h.validateManualHoldBypassForLoopTarget(r.Context(), projectID, domain.LoopType(loopType), target, derefBool(body.Force)); err != nil {
		return loopResponse{}, err
	}

	// Share the same-target lock with discard+retry so create cannot enqueue a
	// new active loop for this target between discard preflight and requeue.
	candidateStatusForLock := domain.LoopStatus(status)
	if (domain.LoopType(loopType) == domain.LoopTypeReviewer || domain.LoopType(loopType) == domain.LoopTypeFixer || domain.LoopType(loopType) == domain.LoopTypeWorker) && candidateStatusForLock == domain.LoopStatusRunning {
		candidateStatusForLock = domain.LoopStatusQueued
	}
	unlockTarget := h.lockLoopTargetForStatus(projectID, domain.LoopType(loopType), target, candidateStatusForLock)
	defer unlockTarget()

	record, err := storage.WithTransactionValue(r.Context(), services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		transactionRepos := storage.NewRepositories(tx)
		_, err := requireActiveProjectRecord(r.Context(), transactionRepos.Projects, projectID)
		if err != nil {
			return storage.LoopRecord{}, err
		}

		existing, err := transactionRepos.Loops.List(r.Context())
		if err != nil {
			return storage.LoopRecord{}, err
		}
		candidateStatus := domain.LoopStatus(status)
		if err := assertUniqueActiveLoopCompat(existing, "", projectID, domain.LoopType(loopType), target, candidateStatus); err != nil {
			return storage.LoopRecord{}, err
		}

		seq, err := transactionRepos.Loops.AllocateSeq(r.Context())
		if err != nil {
			return storage.LoopRecord{}, err
		}

		record := storage.LoopRecord{
			ID:           generateRequestID(),
			Seq:          seq,
			ProjectID:    projectID,
			Type:         loopType,
			TargetType:   targetType,
			TargetID:     loopTargetIDCompat(target),
			Repo:         repoFromTargetCompat(target),
			PRNumber:     prNumberFromTargetCompat(target),
			Status:       status,
			ConfigJSON:   nil,
			MetadataJSON: metadataJSON,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if (domain.LoopType(loopType) == domain.LoopTypeReviewer || domain.LoopType(loopType) == domain.LoopTypeFixer || domain.LoopType(loopType) == domain.LoopTypeWorker) && candidateStatus == domain.LoopStatusRunning {
			record.Status = string(domain.LoopStatusQueued)
			candidateStatus = domain.LoopStatusQueued
		}
		if candidateStatus == domain.LoopStatusRunning {
			record.NextRunAt = &nowISO
		} else if candidateStatus == domain.LoopStatusQueued {
			record.NextRunAt = &nowISO
		}

		if err := transactionRepos.Loops.Upsert(r.Context(), record); err != nil {
			return storage.LoopRecord{}, err
		}

		shouldQueue := ((domain.LoopType(loopType) == domain.LoopTypeReviewer || domain.LoopType(loopType) == domain.LoopTypeFixer || domain.LoopType(loopType) == domain.LoopTypeWorker) && candidateStatus == domain.LoopStatusQueued) || (domain.LoopType(loopType) == domain.LoopTypePlanner && (candidateStatus == domain.LoopStatusRunning || candidateStatus == domain.LoopStatusQueued))
		if shouldQueue {
			queueRecord, ok, queueErr := buildQueuedLoopQueueRecordCompat(record, target, nowISO, metadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
			if queueErr != nil {
				return storage.LoopRecord{}, queueErr
			}
			if ok {
				existingQueue, findErr := transactionRepos.Queue.FindActiveByDedupe(r.Context(), queueRecord.DedupeKey)
				if findErr != nil {
					return storage.LoopRecord{}, findErr
				}
				if existingQueue == nil {
					persistedQueue, createdQueue, upsertQueueErr := transactionRepos.Queue.CreateOrGetActiveByDedupe(r.Context(), queueRecord)
					if upsertQueueErr != nil {
						return storage.LoopRecord{}, upsertQueueErr
					}
					if !createdQueue && persistedQueue.ID != queueRecord.ID {
						return storage.LoopRecord{}, fmt.Errorf("active loop already exists for dedupe key %s", queueRecord.DedupeKey)
					}
				}
			}
		}

		return record, nil
	})
	if err != nil {
		var typed apiError
		if asAPIError(err, &typed) {
			return loopResponse{}, typed
		}
		return loopResponse{}, mapLoopCreateError(err)
	}
	shouldTriggerScheduler := ((record.Type == string(domain.LoopTypeReviewer) || record.Type == string(domain.LoopTypeFixer) || record.Type == string(domain.LoopTypeWorker)) && record.Status == string(domain.LoopStatusQueued)) || (record.Type == string(domain.LoopTypePlanner) && (record.Status == string(domain.LoopStatusRunning) || record.Status == string(domain.LoopStatusQueued)))
	if shouldTriggerScheduler && h.context.TriggerSchedulerTick != nil {
		h.context.TriggerSchedulerTick()
	}

	return serializeLoop(record), nil
}

func (h *Handler) buildWorkersCreateResponse(r *http.Request) (workerCreateResponse, error) {
	if r.Method != http.MethodPost {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/workers")}
	}
	if !isCodingRoleAgentConfigured(h.effectiveConfig(), config.CodingRoleWorker) {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: "Cannot create worker loop without config.agent.vendor"}
	}

	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}

	body := createWorkerRequest{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "Request body must be valid JSON"}
	}

	prompt := normalizeOptionalString(body.Prompt)
	specPath := normalizeOptionalString(body.SpecPath)
	prNumber := normalizePositiveInt64Ptr(body.PRNumber)
	issueNumber := normalizePositiveInt64Ptr(body.IssueNumber)
	modeCount := 0
	if prNumber != nil {
		modeCount++
	}
	if issueNumber != nil {
		modeCount++
	}
	if prompt != nil || specPath != nil {
		modeCount++
	}
	if modeCount == 0 {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "prompt or specPath is required unless prNumber or issueNumber is provided"}
	}
	if modeCount > 1 {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "worker accepts exactly one input mode: prompt/specPath, prNumber, or issueNumber"}
	}

	project, err := h.resolveWorkerProject(r.Context(), resolveWorkerProjectInput{
		ProjectID: normalizeOptionalString(body.ProjectID),
		Repo:      normalizeOptionalString(body.Repo),
		PRNumber:  prNumber,
	})
	if err != nil {
		return workerCreateResponse{}, err
	}
	projectID := project.ID

	repo := normalizeOptionalString(body.Repo)
	if repo == nil {
		repo = stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")
	}
	if repo == nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo is required"}
	}

	baseBranch := normalizeOptionalString(body.BaseBranch)
	if baseBranch == nil {
		baseBranch = normalizeOptionalString(project.BaseBranch)
	}
	if baseBranch == nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "baseBranch is required"}
	}
	requestedIssueTarget := (*domain.LoopTarget)(nil)
	if issueNumber != nil {
		requestedIssueTarget = &domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, Repo: *repo, IssueNumber: *issueNumber}
	}

	effectivePRNumber := (*int64)(nil)
	if prNumber != nil {
		resolved, resolveErr := h.requirePullRequestTarget(r.Context(), requirePullRequestTargetInput{ProjectID: projectID, Repo: *repo, PRNumber: *prNumber})
		if resolveErr != nil {
			return workerCreateResponse{}, resolveErr
		}
		effectivePRNumber = &resolved
	}

	planner := (*workerPlannerMatch)(nil)
	if issueNumber != nil {
		planner, err = h.maybeFindPlannerLoopForIssue(r.Context(), findPlannerLoopForIssueInput{ProjectID: projectID, Repo: *repo, IssueNumber: *issueNumber})
		if err != nil {
			return workerCreateResponse{}, err
		}
	}
	if effectivePRNumber == nil && planner != nil {
		effectivePRNumber = planner.PRNumber
	}
	effectiveSpecPath := specPath
	if effectiveSpecPath == nil && planner != nil {
		effectiveSpecPath = planner.SpecPath
	}

	title := strings.TrimSpace(derefString(body.Title))
	if title == "" {
		title = deriveWorkerTitle(prompt, effectiveSpecPath, repo, effectivePRNumber, issueNumber)
	}
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	targetType := string(domain.LoopTargetTypeProject)
	targetID := "project:" + projectID
	target := domain.LoopTarget{TargetType: domain.LoopTargetTypeProject, ProjectID: projectID}
	if effectivePRNumber != nil {
		targetType = string(domain.LoopTargetTypePullRequest)
		targetID = fmt.Sprintf("pr:%s:%d", *repo, *effectivePRNumber)
		target = domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: *repo, PRNumber: *effectivePRNumber}
	} else if issueNumber != nil {
		targetType = string(domain.LoopTargetTypeIssue)
		targetID = fmt.Sprintf("issue:%s:%d", *repo, *issueNumber)
		target = domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, Repo: *repo, IssueNumber: *issueNumber}
	}
	if err := validateLoopTargetProjectCompatibility(projectID, parseProjectMetadata(project.MetadataJSON), target); err != nil {
		return workerCreateResponse{}, err
	}
	if requestedIssueTarget != nil {
		if err := validateLoopTargetProjectCompatibility(projectID, parseProjectMetadata(project.MetadataJSON), *requestedIssueTarget); err != nil {
			return workerCreateResponse{}, err
		}
		if err := h.validateManualHoldBypassForLoopTarget(r.Context(), projectID, domain.LoopTypeWorker, *requestedIssueTarget, derefBool(body.Force)); err != nil {
			return workerCreateResponse{}, err
		}
	}
	if requestedIssueTarget == nil || target.TargetType != requestedIssueTarget.TargetType || target.Repo != requestedIssueTarget.Repo || target.IssueNumber != requestedIssueTarget.IssueNumber {
		if err := h.validateManualHoldBypassForLoopTarget(r.Context(), projectID, domain.LoopTypeWorker, target, derefBool(body.Force)); err != nil {
			return workerCreateResponse{}, err
		}
	}

	workerPayload := struct {
		Title       string  `json:"title"`
		Prompt      *string `json:"prompt"`
		SpecPath    *string `json:"specPath"`
		Repo        string  `json:"repo"`
		BaseBranch  string  `json:"baseBranch"`
		IssueNumber *int64  `json:"issueNumber,omitempty"`
		PRNumber    *int64  `json:"prNumber,omitempty"`
	}{
		Title:       title,
		Prompt:      prompt,
		SpecPath:    effectiveSpecPath,
		Repo:        *repo,
		BaseBranch:  *baseBranch,
		IssueNumber: issueNumber,
		PRNumber:    effectivePRNumber,
	}
	payloadJSONBytes, err := json.Marshal(struct {
		Worker any `json:"worker"`
	}{
		Worker: workerPayload,
	})
	if err != nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	queuePayloadJSONBytes, err := json.Marshal(workerPayload)
	if err != nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	metadataJSON := string(payloadJSONBytes)
	reusedWorkerLoop := false

	// Issue-worker reuse enqueues the existing loop (same as start requeue).
	// Take the shared per-loop retry lock before the TX so discard+retry cannot
	// wipe the managed worktree after reuse preflight/enqueue races in.
	// Pre-scan is best-effort identity for the lock; the TX re-evaluates reuse.
	if issueNumber != nil && requestedIssueTarget != nil {
		existing, listErr := services.Repositories.Loops.List(r.Context())
		if listErr != nil {
			return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: listErr.Error()}
		}
		if existingLoop, _, ok, reuseErr := reusableWorkerLoopForIssueRequestCompat(existing, projectID, *requestedIssueTarget, target); reuseErr != nil {
			return workerCreateResponse{}, reuseErr
		} else if ok {
			unlock := h.lockLoopRetry(existingLoop.ID)
			defer unlock()
		}
	}
	// New worker create (and non-reuse paths) share the same-target lock with
	// discard+retry so a concurrent create for this target cannot pass unique
	// checks after discard preflight and leave a wiped worktree.
	unlockWorkerTarget := h.lockLoopTargetForStatus(projectID, domain.LoopTypeWorker, target, domain.LoopStatusQueued)
	defer unlockWorkerTarget()

	// Issue-worker reuse publishes claimable queue work inside the TX. Clear the
	// sticky stop gate before that TX so a concurrent scheduler tick cannot claim
	// the reused worker and fail AgentExecutor.Start with ErrSpawnLoopStopping.
	// Track for restore when the TX aborts or does not queue the reused loop.
	reuseStopGateLoopID := ""
	reuseGateWasActive := false
	if issueNumber != nil && requestedIssueTarget != nil {
		existing, listErr := services.Repositories.Loops.List(r.Context())
		if listErr == nil {
			if existingLoop, _, ok, reuseErr := reusableWorkerLoopForIssueRequestCompat(existing, projectID, *requestedIssueTarget, target); reuseErr == nil && ok {
				reuseStopGateLoopID = existingLoop.ID
				if services.ActiveExecutions != nil {
					// Clear and sample under one lock so a concurrent BeginLoopStop
					// cannot insert a gate that we delete without recording for restore.
					reuseGateWasActive = services.ActiveExecutions.ClearLoopStop(existingLoop.ID)
				}
			}
		}
	}
	restoreReuseStopGate := func() error {
		if reuseGateWasActive && reuseStopGateLoopID != "" && services.ActiveExecutions != nil {
			return services.ActiveExecutions.RestoreLoopStop(reuseStopGateLoopID)
		}
		return nil
	}

	record, err := storage.WithTransactionValue(r.Context(), services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)

		existing, listErr := repos.Loops.List(r.Context())
		if listErr != nil {
			return storage.LoopRecord{}, listErr
		}
		if issueNumber != nil {
			if existingLoop, existingTarget, ok, reuseErr := reusableWorkerLoopForIssueRequestCompat(existing, projectID, *requestedIssueTarget, target); reuseErr != nil {
				return storage.LoopRecord{}, reuseErr
			} else if ok {
				reusedWorkerLoop = true
				// Ensure gate is open even when the pre-TX scan missed this loop;
				// still before commit so the queue item is not yet claimable.
				if services.ActiveExecutions != nil {
					if reuseStopGateLoopID == "" {
						reuseStopGateLoopID = existingLoop.ID
					}
					// Clear+report under one lock: looper stop may establish the gate
					// after the pre-TX clear saw it inactive. Without this return
					// value, TX abort restore would skip (flag still false).
					if services.ActiveExecutions.ClearLoopStop(existingLoop.ID) {
						reuseGateWasActive = true
					}
				}
				resumed, resumeErr := h.resumeReusableWorkerLoopCompat(r.Context(), repos, existingLoop, existingTarget, nowISO, derefBool(body.Force))
				if resumeErr != nil {
					return storage.LoopRecord{}, resumeErr
				}
				return resumed, nil
			}
		}
		if uniqueErr := assertUniqueActiveLoopCompat(existing, "", projectID, domain.LoopTypeWorker, target, domain.LoopStatusQueued); uniqueErr != nil {
			return storage.LoopRecord{}, uniqueErr
		}

		seq, seqErr := repos.Loops.AllocateSeq(r.Context())
		if seqErr != nil {
			return storage.LoopRecord{}, seqErr
		}

		record := storage.LoopRecord{
			ID:           generateRequestID(),
			Seq:          seq,
			ProjectID:    projectID,
			Type:         string(domain.LoopTypeWorker),
			TargetType:   targetType,
			TargetID:     &targetID,
			Repo:         repo,
			PRNumber:     effectivePRNumber,
			Status:       string(domain.LoopStatusQueued),
			ConfigJSON:   nil,
			MetadataJSON: &metadataJSON,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if upsertErr := repos.Loops.Upsert(r.Context(), record); upsertErr != nil {
			return storage.LoopRecord{}, upsertErr
		}

		projectIDCopy := projectID
		loopID := record.ID
		dedupeKey := "worker:" + loopID
		lockKey := "worker:" + loopID
		if effectivePRNumber != nil {
			dedupeKey = fmt.Sprintf("worker:%s:%s:%d", projectID, *repo, *effectivePRNumber)
			lockKey = storage.PullRequestLockKey(projectID, *repo, *effectivePRNumber)
		} else if issueNumber != nil {
			dedupeKey = fmt.Sprintf("worker:%s:%s:%d", projectID, *repo, *issueNumber)
			lockKey = storage.IssueLockKey(projectID, *repo, *issueNumber)
		}
		payloadJSON := string(queuePayloadJSONBytes)
		queueRecord := storage.QueueItemRecord{
			ID:          generateRequestID(),
			ProjectID:   &projectIDCopy,
			LoopID:      &loopID,
			Type:        string(domain.LoopTypeWorker),
			TargetType:  targetType,
			TargetID:    targetID,
			Repo:        repo,
			PRNumber:    effectivePRNumber,
			DedupeKey:   dedupeKey,
			Priority:    storage.QueuePriorityWorker,
			Status:      "queued",
			AvailableAt: nowISO,
			Attempts:    0,
			MaxAttempts: int64(h.context.Config.Scheduler.RetryMaxAttempts),
			LockKey:     &lockKey,
			PayloadJSON: &payloadJSON,
			CreatedAt:   nowISO,
			UpdatedAt:   nowISO,
		}
		if upsertQueueErr := repos.Queue.Upsert(r.Context(), queueRecord); upsertQueueErr != nil {
			return storage.LoopRecord{}, upsertQueueErr
		}

		return record, nil
	})
	if err != nil {
		if restoreErr := restoreReuseStopGate(); restoreErr != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				typed.message = errors.Join(err, restoreErr).Error()
				return workerCreateResponse{}, typed
			}
			return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: errors.Join(err, restoreErr).Error()}
		}
		var typed apiError
		if asAPIError(err, &typed) {
			return workerCreateResponse{}, typed
		}
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	// Pre-cleared for a reuse that did not become claimable queued work: restore.
	if !reusedWorkerLoop || record.Status != string(domain.LoopStatusQueued) {
		if restoreErr := restoreReuseStopGate(); restoreErr != nil {
			return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: restoreErr.Error()}
		}
	}
	if h.context.TriggerSchedulerTick != nil {
		if !reusedWorkerLoop || record.Status == string(domain.LoopStatusQueued) {
			h.context.TriggerSchedulerTick()
		}
	}

	if reusedWorkerLoop {
		title, prompt, effectiveSpecPath, baseBranch, issueNumber = reusedWorkerResponseFields(record, title, prompt, effectiveSpecPath, baseBranch, issueNumber)
	}

	response := workerCreateResponse{
		loopResponse: serializeLoop(record),
		Title:        title,
		Prompt:       prompt,
		SpecPath:     effectiveSpecPath,
		BaseBranch:   derefString(baseBranch),
		IssueNumber:  issueNumber,
		Reused:       reusedWorkerLoop,
	}

	return response, nil
}

func (h *Handler) validateManualHoldBypassForLoopTarget(ctx context.Context, projectID string, loopType domain.LoopType, target domain.LoopTarget, force bool) error {
	if force || (loopType != domain.LoopTypePlanner && loopType != domain.LoopTypeWorker && loopType != domain.LoopTypeReviewer && loopType != domain.LoopTypeFixer) {
		return nil
	}
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.Projects == nil {
		return nil
	}
	project, err := requireActiveProjectRecord(ctx, services.Repositories.Projects, projectID)
	if err != nil {
		return err
	}
	// Hold preflight is best-effort at create time: when we cannot reliably talk to
	// GitHub from this handler context (missing repo path, missing gh path, etc.) we
	// skip validation rather than blocking manual creation for unrelated local setup.
	if strings.TrimSpace(project.RepoPath) == "" {
		return nil
	}
	if _, err := os.Stat(project.RepoPath); err != nil {
		return nil
	}
	ghPath := strings.TrimSpace(derefString(h.context.Config.Tools.GHPath))
	if ghPath == "" {
		return nil
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: ghPath, CWD: project.RepoPath, GHRun: shell.Run})
	labels := []string(nil)
	switch target.TargetType {
	case domain.LoopTargetTypeIssue:
		detail, err := gh.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: target.Repo, IssueNumber: target.IssueNumber, CWD: project.RepoPath})
		if err != nil {
			return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("refresh target before manual loop create: %v", err)}
		}
		labels = detail.Labels
	case domain.LoopTargetTypePullRequest:
		detail, err := gh.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: target.Repo, PRNumber: target.PRNumber, CWD: project.RepoPath})
		if err != nil {
			return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("refresh target before manual loop create: %v", err)}
		}
		labels = detail.Labels
	default:
		return nil
	}
	if !domain.IsAutoLaneHeld(loopType, labels) {
		return nil
	}
	return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("target is currently held for %s; rerun with --force to bypass hold", loopType)}
}

func derefBool(value *bool) bool {
	return value != nil && *value
}

func reusableWorkerLoopForIssueRequestCompat(existing []storage.LoopRecord, projectID string, issueTarget, effectiveTarget domain.LoopTarget) (storage.LoopRecord, domain.LoopTarget, bool, error) {
	for _, loop := range existing {
		if loop.ProjectID != projectID || loop.Type != string(domain.LoopTypeWorker) {
			continue
		}
		status := domain.LoopStatus(loop.Status)
		if !domain.IsConflictingActiveLoopStatus(status) {
			continue
		}
		loopTarget, err := loopTargetFromRecordCompat(loop)
		if err != nil {
			return storage.LoopRecord{}, domain.LoopTarget{}, false, err
		}
		key := loopTargetKeyFromRecordCompat(loop)
		if key != loopTargetKeyCompat(issueTarget) && key != loopTargetKeyCompat(effectiveTarget) {
			continue
		}
		return loop, loopTarget, true, nil
	}

	return storage.LoopRecord{}, domain.LoopTarget{}, false, nil
}

func (h *Handler) resumeReusableWorkerLoopCompat(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, target domain.LoopTarget, nowISO string, force bool) (storage.LoopRecord, error) {
	status := domain.LoopStatus(loop.Status)
	if force && status == domain.LoopStatusRunning {
		return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeLoopConflict, status: http.StatusConflict, message: fmt.Sprintf("Cannot force reuse running worker loop %s", loop.ID)}
	}
	if force {
		normalized, err := forceManualWorkerLoopStateCompat(ctx, repos, loop, nowISO)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		loop = normalized
	}
	shouldQueue := status == domain.LoopStatusIdle || status == domain.LoopStatusPaused || status == domain.LoopStatusQueued
	if status == domain.LoopStatusIdle || status == domain.LoopStatusPaused {
		if err := domain.AssertLoopStatusTransition(status, domain.LoopStatusQueued); err != nil {
			return storage.LoopRecord{}, err
		}
		loop.Status = string(domain.LoopStatusQueued)
		loop.NextRunAt = &nowISO
		loop.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			return storage.LoopRecord{}, err
		}
	}

	if shouldQueue {
		requeued, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, loop.ID, nowISO)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if requeued == 0 {
			activeQueue, findErr := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
			if findErr != nil {
				return storage.LoopRecord{}, findErr
			}
			if activeQueue == nil {
				latestQueue, latestErr := repos.Queue.GetLatestByLoopID(ctx, loop.ID)
				if latestErr != nil {
					return storage.LoopRecord{}, latestErr
				}
				if latestQueue != nil {
					if latestQueue.DedupeKey != "" {
						activeDedupe, dedupeErr := repos.Queue.FindActiveByDedupe(ctx, latestQueue.DedupeKey)
						if dedupeErr != nil {
							return storage.LoopRecord{}, dedupeErr
						}
						if activeDedupe != nil {
							return loop, nil
						}
					}
					replacement := *latestQueue
					replacement.ID = generateRequestID()
					replacement.Status = "queued"
					replacement.AvailableAt = nowISO
					replacement.Attempts = 0
					replacement.ClaimedBy = nil
					replacement.ClaimedAt = nil
					replacement.StartedAt = nil
					replacement.FinishedAt = nil
					replacement.LastError = nil
					replacement.LastErrorKind = nil
					replacement.CreatedAt = nowISO
					replacement.UpdatedAt = nowISO
					if force {
						replacement.PayloadJSON = forcedManualWorkerQueuePayloadJSONCompat(replacement.PayloadJSON)
					}
					if _, _, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, replacement); err != nil {
						return storage.LoopRecord{}, err
					}
				} else {
					queueRecord, ok, queueErr := buildQueuedLoopQueueRecordCompat(loop, target, nowISO, loop.MetadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
					if queueErr != nil {
						return storage.LoopRecord{}, queueErr
					}
					if ok {
						if force {
							queueRecord.PayloadJSON = forcedManualWorkerQueuePayloadJSONCompat(queueRecord.PayloadJSON)
						}
						if _, _, upsertQueueErr := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, queueRecord); upsertQueueErr != nil {
							return storage.LoopRecord{}, upsertQueueErr
						}
					}
				}
			} else if force {
				activeQueue.PayloadJSON = forcedManualWorkerQueuePayloadJSONCompat(activeQueue.PayloadJSON)
				activeQueue.UpdatedAt = nowISO
				if err := repos.Queue.Upsert(ctx, *activeQueue); err != nil {
					return storage.LoopRecord{}, err
				}
			}
		} else if force {
			activeQueue, findErr := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
			if findErr != nil {
				return storage.LoopRecord{}, findErr
			}
			if activeQueue != nil {
				activeQueue.PayloadJSON = forcedManualWorkerQueuePayloadJSONCompat(activeQueue.PayloadJSON)
				activeQueue.UpdatedAt = nowISO
				if err := repos.Queue.Upsert(ctx, *activeQueue); err != nil {
					return storage.LoopRecord{}, err
				}
			}
		}
	}

	return loop, nil
}

func forcedManualWorkerQueuePayloadJSONCompat(payloadJSON *string) *string {
	payload := parseJSONObject(payloadJSON)
	if len(payload) == 0 {
		return payloadJSON
	}
	if payload["autoDiscovered"] != true {
		return payloadJSON
	}
	delete(payload, "autoDiscovered")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return payloadJSON
	}
	text := string(encoded)
	return &text
}

func forceManualWorkerLoopStateCompat(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) (storage.LoopRecord, error) {
	metadataJSON := forcedManualWorkerMetadataJSONCompat(loop.MetadataJSON)
	if !stringPtrEqual(metadataJSON, loop.MetadataJSON) {
		loop.MetadataJSON = metadataJSON
		loop.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			return storage.LoopRecord{}, err
		}
	}
	if repos.Runs != nil {
		latestRun, err := repos.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if latestRun != nil {
			checkpointJSON := forcedManualWorkerCheckpointJSONCompat(latestRun.CheckpointJSON)
			if !stringPtrEqual(checkpointJSON, latestRun.CheckpointJSON) {
				latestRun.CheckpointJSON = checkpointJSON
				latestRun.UpdatedAt = nowISO
				if err := repos.Runs.Upsert(ctx, *latestRun); err != nil {
					return storage.LoopRecord{}, err
				}
			}
		}
	}
	return loop, nil
}

func forcedManualWorkerMetadataJSONCompat(metadataJSON *string) *string {
	metadata := parseJSONObject(metadataJSON)
	if len(metadata) == 0 {
		return metadataJSON
	}
	worker, _ := metadata["worker"].(map[string]any)
	if worker["autoDiscovered"] != true {
		return metadataJSON
	}
	delete(worker, "autoDiscovered")
	metadata["worker"] = worker
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return metadataJSON
	}
	text := string(encoded)
	return &text
}

func forcedManualWorkerCheckpointJSONCompat(checkpointJSON *string) *string {
	checkpoint := parseJSONObject(checkpointJSON)
	if len(checkpoint) == 0 {
		return checkpointJSON
	}
	work, _ := checkpoint["work"].(map[string]any)
	if work["autoDiscovered"] != true {
		return checkpointJSON
	}
	delete(work, "autoDiscovered")
	checkpoint["work"] = work
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return checkpointJSON
	}
	text := string(encoded)
	return &text
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func reusedWorkerResponseFields(loop storage.LoopRecord, fallbackTitle string, fallbackPrompt, fallbackSpecPath, fallbackBaseBranch *string, fallbackIssueNumber *int64) (string, *string, *string, *string, *int64) {
	metadata := parseJSONObject(loop.MetadataJSON)
	worker, _ := metadata["worker"].(map[string]any)
	title := fallbackTitle
	if value := readStringAny(worker["title"]); value != nil {
		title = *value
	}
	prompt := fallbackPrompt
	if value := readStringAny(worker["prompt"]); value != nil {
		prompt = value
	}
	specPath := fallbackSpecPath
	if value := readStringAny(worker["specPath"]); value != nil {
		specPath = value
	}
	baseBranch := fallbackBaseBranch
	if value := readStringAny(worker["baseBranch"]); value != nil {
		baseBranch = value
	}
	issueNumber := fallbackIssueNumber
	if value := int64MetadataPtr(worker, "issueNumber"); value != nil {
		issueNumber = value
	}
	return title, prompt, specPath, baseBranch, issueNumber
}

func (h *Handler) buildPlannersCreateResponse(r *http.Request) (plannerCreateResponse, error) {
	if r.Method != http.MethodPost {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/planners")}
	}
	if !isCodingRoleAgentConfigured(h.effectiveConfig(), config.CodingRolePlanner) {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: "Cannot create planner loop without config.agent.vendor"}
	}

	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}

	body := createPlannerRequest{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "Request body must be valid JSON"}
	}

	projectID := strings.TrimSpace(derefString(body.ProjectID))
	if projectID == "" {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "projectId is required"}
	}
	project, err := requireActiveProjectRecord(r.Context(), services.Repositories.Projects, projectID)
	if err != nil {
		return plannerCreateResponse{}, err
	}

	issueNumber := normalizePositiveInt64Ptr(body.IssueNumber)
	if issueNumber == nil {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "issueNumber must be a positive integer"}
	}

	repo := stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")
	if repo == nil {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "project repo is required"}
	}
	target := domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, Repo: *repo, IssueNumber: *issueNumber}
	if err := h.validateManualHoldBypassForLoopTarget(r.Context(), projectID, domain.LoopTypePlanner, target, derefBool(body.Force)); err != nil {
		return plannerCreateResponse{}, err
	}

	// Share same-target lock with discard+retry so planner uniqueness races
	// cannot interleave with requeue while discard mutates the worktree.
	unlockPlannerTarget := h.lockLoopTargetForStatus(projectID, domain.LoopTypePlanner, target, domain.LoopStatusRunning)
	defer unlockPlannerTarget()

	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	targetID := fmt.Sprintf("issue:%s:%d", *repo, *issueNumber)
	metadataJSONPtr, err := manualPlannerMetadataJSON(nil, *issueNumber)
	if err != nil {
		return plannerCreateResponse{}, err
	}
	metadataJSON := derefString(metadataJSONPtr)

	record, err := storage.WithTransactionValue(r.Context(), services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)
		seq, seqErr := repos.Loops.AllocateSeq(r.Context())
		if seqErr != nil {
			return storage.LoopRecord{}, seqErr
		}

		existing, listErr := repos.Loops.List(r.Context())
		if listErr != nil {
			return storage.LoopRecord{}, listErr
		}
		if uniqueErr := assertUniqueActiveLoopCompat(existing, "", projectID, domain.LoopTypePlanner, target, domain.LoopStatusRunning); uniqueErr != nil {
			return storage.LoopRecord{}, uniqueErr
		}

		record := storage.LoopRecord{
			ID:           generateRequestID(),
			Seq:          seq,
			ProjectID:    projectID,
			Type:         string(domain.LoopTypePlanner),
			TargetType:   string(domain.LoopTargetTypeIssue),
			TargetID:     &targetID,
			Repo:         repo,
			PRNumber:     nil,
			Status:       string(domain.LoopStatusRunning),
			ConfigJSON:   nil,
			MetadataJSON: &metadataJSON,
			NextRunAt:    &nowISO,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if upsertErr := repos.Loops.Upsert(r.Context(), record); upsertErr != nil {
			return storage.LoopRecord{}, upsertErr
		}

		queueRecord, ok, queueErr := buildQueuedLoopQueueRecordCompat(record, target, nowISO, &metadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
		if queueErr != nil {
			return storage.LoopRecord{}, queueErr
		}
		if !ok {
			return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "failed to build planner queue item"}
		}
		if upsertQueueErr := repos.Queue.Upsert(r.Context(), queueRecord); upsertQueueErr != nil {
			return storage.LoopRecord{}, upsertQueueErr
		}

		return record, nil
	})
	if err != nil {
		var typed apiError
		if asAPIError(err, &typed) {
			return plannerCreateResponse{}, typed
		}
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if h.context.TriggerSchedulerTick != nil {
		h.context.TriggerSchedulerTick()
	}

	return plannerCreateResponse{loopResponse: serializeLoop(record), IssueNumber: *issueNumber}, nil
}

type resolveWorkerProjectInput struct {
	ProjectID *string
	Repo      *string
	PRNumber  *int64
}

func (h *Handler) resolveWorkerProject(ctx context.Context, input resolveWorkerProjectInput) (storage.ProjectRecord, error) {
	services := h.context.Runtime.Services()
	if input.ProjectID != nil {
		project, err := requireActiveProjectRecord(ctx, services.Repositories.Projects, *input.ProjectID)
		if err != nil {
			return storage.ProjectRecord{}, err
		}
		if input.Repo != nil {
			configuredRepo := strings.TrimSpace(derefString(stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")))
			requestedRepo := strings.TrimSpace(*input.Repo)
			if configuredRepo != "" && configuredRepo != requestedRepo {
				if input.PRNumber != nil {
					return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodePullRequestProjectMismatch, status: http.StatusConflict, message: fmt.Sprintf("Pull request %s#%d does not belong to project %s", requestedRepo, *input.PRNumber, *input.ProjectID)}
				}
				return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("project %s is configured for repo %s, not %s", *input.ProjectID, configuredRepo, requestedRepo)}
			}
		}
		return *project, nil
	}

	if input.Repo != nil && input.PRNumber != nil {
		requestedRepo := strings.TrimSpace(*input.Repo)
		snapshots, err := services.Repositories.PullRequestSnapshots.List(ctx)
		if err != nil {
			return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		matchedProjectIDs := map[string]struct{}{}
		for _, snapshot := range snapshots {
			if snapshot.Repo == requestedRepo && snapshot.PRNumber == *input.PRNumber {
				project, getErr := services.Repositories.Projects.GetByID(ctx, snapshot.ProjectID)
				if getErr != nil {
					return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: getErr.Error()}
				}
				if project != nil && !project.Archived {
					configuredRepo := strings.TrimSpace(derefString(stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")))
					if configuredRepo != "" && configuredRepo != requestedRepo {
						continue
					}
					matchedProjectIDs[snapshot.ProjectID] = struct{}{}
				}
			}
		}
		if len(matchedProjectIDs) > 1 {
			return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeProjectAmbiguous, status: http.StatusConflict, message: fmt.Sprintf("Multiple projects match pull request %s#%d; pass projectId explicitly", *input.Repo, *input.PRNumber)}
		}
		for projectID := range matchedProjectIDs {
			project, getErr := services.Repositories.Projects.GetByID(ctx, projectID)
			if getErr != nil {
				return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: getErr.Error()}
			}
			if project != nil {
				return *project, nil
			}
		}
	}

	if input.Repo != nil {
		projectsList, err := services.Repositories.Projects.List(ctx)
		if err != nil {
			return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		matches := make([]storage.ProjectRecord, 0)
		for _, candidate := range projectsList {
			if candidate.Archived {
				continue
			}
			candidateRepo := stringMetadataPtr(parseProjectMetadata(candidate.MetadataJSON), "repo")
			if candidateRepo != nil && *candidateRepo == *input.Repo {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeProjectAmbiguous, status: http.StatusConflict, message: fmt.Sprintf("Multiple projects match repo %s; pass projectId explicitly", *input.Repo)}
		}
	}

	return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "projectId is required unless it can be resolved from repo/prNumber"}
}

type requirePullRequestTargetInput struct {
	ProjectID string
	Repo      string
	PRNumber  int64
}

func (h *Handler) requirePullRequestTarget(ctx context.Context, input requirePullRequestTargetInput) (int64, error) {
	services := h.context.Runtime.Services()
	project, err := requireActiveProjectRecord(ctx, services.Repositories.Projects, input.ProjectID)
	if err != nil {
		return 0, err
	}
	projectRepo := stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")
	if projectRepo == nil || *projectRepo != input.Repo {
		return 0, apiError{code: pkgapi.ErrorCodePullRequestProjectMismatch, status: http.StatusConflict, message: fmt.Sprintf("Pull request %s#%d does not belong to project %s", input.Repo, input.PRNumber, input.ProjectID)}
	}
	snapshot, err := services.Repositories.PullRequestSnapshots.GetLatestByProject(ctx, input.ProjectID, input.Repo, input.PRNumber)
	if err != nil {
		return 0, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if snapshot == nil {
		return 0, apiError{code: pkgapi.ErrorCodePullRequestNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Pull request not found: %s#%d", input.Repo, input.PRNumber)}
	}
	if snapshot.ProjectID != input.ProjectID {
		return 0, apiError{code: pkgapi.ErrorCodePullRequestProjectMismatch, status: http.StatusConflict, message: fmt.Sprintf("Pull request %s#%d does not belong to project %s", input.Repo, input.PRNumber, input.ProjectID)}
	}
	return snapshot.PRNumber, nil
}

type findPlannerLoopForIssueInput struct {
	ProjectID   string
	Repo        string
	IssueNumber int64
}

type workerPlannerMatch struct {
	PRNumber *int64
	SpecPath *string
}

func (h *Handler) maybeFindPlannerLoopForIssue(ctx context.Context, input findPlannerLoopForIssueInput) (*workerPlannerMatch, error) {
	loopsList, err := h.context.Runtime.Services().Repositories.Loops.List(ctx)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	targetID := fmt.Sprintf("issue:%s:%d", input.Repo, input.IssueNumber)
	for _, loop := range loopsList {
		if loop.ProjectID != input.ProjectID || loop.Type != string(domain.LoopTypePlanner) || loop.TargetType != string(domain.LoopTargetTypeIssue) || derefString(loop.TargetID) != targetID {
			continue
		}
		metadata := parseProjectMetadata(loop.MetadataJSON)
		prNumber := loop.PRNumber
		if prNumber == nil {
			prNumber = int64MetadataPtr(metadata, "prNumber")
		}
		match := &workerPlannerMatch{PRNumber: prNumber, SpecPath: stringMetadataPtr(metadata, "specPath")}
		if prNumber == nil {
			return &workerPlannerMatch{PRNumber: nil, SpecPath: match.SpecPath}, nil
		}
		isOpen, known, err := h.getPlannerPullRequestOpenState(ctx, input.ProjectID, input.Repo, *prNumber)
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		if known && !isOpen {
			return &workerPlannerMatch{PRNumber: nil, SpecPath: match.SpecPath}, nil
		}
		return match, nil
	}
	return nil, nil
}

func (h *Handler) isPlannerPullRequestOpen(ctx context.Context, projectID, repo string, prNumber int64) bool {
	isOpen, known, err := h.getPlannerPullRequestOpenState(ctx, projectID, repo, prNumber)
	return err == nil && known && isOpen
}

func (h *Handler) getPlannerPullRequestOpenState(ctx context.Context, projectID, repo string, prNumber int64) (bool, bool, error) {
	if prNumber <= 0 {
		return false, true, nil
	}
	snapshot, err := h.context.Runtime.Services().Repositories.PullRequestSnapshots.GetLatestByProject(ctx, projectID, repo, prNumber)
	if err != nil {
		return false, false, err
	}
	if snapshot == nil {
		return false, false, nil
	}
	payload := parseJSONObject(snapshot.PayloadJSON)
	detail, _ := payload["detail"].(map[string]any)
	state := firstNonEmptyString(readStringAny(detail["state"]), readStringAny(detail["State"]))
	if state == nil {
		return false, false, nil
	}
	return strings.EqualFold(*state, "open"), true, nil
}

func deriveWorkerTitle(prompt, specPath, repo *string, prNumber, issueNumber *int64) string {
	if prompt != nil {
		if len(*prompt) > 80 {
			return (*prompt)[:80]
		}
		return *prompt
	}
	if specPath != nil {
		return "Implement " + *specPath
	}
	if prNumber != nil && repo != nil {
		return fmt.Sprintf("Implement %s#%d", *repo, *prNumber)
	}
	if issueNumber != nil && repo != nil {
		return fmt.Sprintf("Implement %s#%d", *repo, *issueNumber)
	}
	return "Worker run"
}

func normalizePositiveInt64Ptr(value *int64) *int64 {
	if value == nil || *value <= 0 {
		return nil
	}
	v := *value
	return &v
}

func int64MetadataPtr(metadata map[string]any, key string) *int64 {
	value, ok := metadata[key]
	if !ok {
		return nil
	}
	floatValue, ok := value.(float64)
	if !ok || floatValue <= 0 || floatValue != float64(int64(floatValue)) {
		return nil
	}
	parsed := int64(floatValue)
	return &parsed
}

func (h *Handler) resolveLoop(ctx context.Context, selector string) (storage.LoopRecord, error) {
	services := h.context.Runtime.Services()
	normalized := strings.TrimSpace(selector)
	if normalized == "" {
		return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "loopId is required"}
	}

	if seq, err := strconv.ParseInt(normalized, 10, 64); err == nil {
		loop, lookupErr := services.Repositories.Loops.GetBySeq(ctx, seq)
		if lookupErr != nil {
			return storage.LoopRecord{}, lookupErr
		}
		if loop != nil {
			return *loop, nil
		}
	}

	loop, err := services.Repositories.Loops.GetByID(ctx, normalized)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	if loop == nil {
		return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", selector)}
	}

	return *loop, nil
}

func (h *Handler) mutateLoopStatus(ctx context.Context, loopID string, status domain.LoopStatus) (loopResponse, error) {
	// Running requeues from the latest failed/cancelled/manual_intervention item
	// and can start replacement work. Share the per-loop retry lock and the
	// same-target lock so this cannot race discard+retry between preflight and
	// destructive git reset — including when the requeued loop is a *different*
	// failed loop for the same PR/issue target.
	if status == domain.LoopStatusRunning {
		unlock := h.lockLoopRetry(loopID)
		defer unlock()
	}

	services := h.context.Runtime.Services()
	if status == domain.LoopStatusRunning {
		if services.Repositories == nil || services.Coordinator == nil {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
		}
		// Resolve target outside the TX so we can hold the target mutex for the
		// whole requeue window (same key as discard+retry / loop create).
		preflightLoop, err := services.Repositories.Loops.GetByID(ctx, loopID)
		if err != nil {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		if preflightLoop != nil {
			target, targetErr := loopTargetFromRecordCompat(*preflightLoop)
			if targetErr != nil {
				return loopResponse{}, targetErr
			}
			unlockTarget := h.lockLoopTarget(preflightLoop.ProjectID, domain.LoopType(preflightLoop.Type), target)
			defer unlockTarget()
		}
	}

	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	// Intentional re-activation (unpause / start): clear sticky stop gate before
	// the TX publishes claimable queue work. Clearing after commit races a
	// concurrent scheduler tick that can claim, pass parked checks, then fail
	// AgentExecutor.Start with ErrSpawnLoopStopping. Restore on TX failure like retryLoop.
	gateWasActive := false
	if status == domain.LoopStatusRunning && services.ActiveExecutions != nil {
		// Clear and report under one lock so abort restore covers any gate this
		// call removed (including one set by concurrent BeginLoopStop).
		gateWasActive = services.ActiveExecutions.ClearLoopStop(loopID)
	}
	restoreStopGate := func() error {
		if gateWasActive && services.ActiveExecutions != nil {
			return services.ActiveExecutions.RestoreLoopStop(loopID)
		}
		return nil
	}
	updated, err := storage.WithTransactionValue(ctx, services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if loop == nil {
			return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
		}
		if status == domain.LoopStatusRunning && strings.TrimSpace(loop.ProjectID) != "" {
			if _, err := requireActiveProjectRecord(ctx, repos.Projects, loop.ProjectID); err != nil {
				return storage.LoopRecord{}, err
			}
		}

		// Live vendor may have been removed while a coding loop was parked
		// (HITL awaiting_human, pause, etc.). Allow requeue when the latest
		// failed/interrupted run still carries a frozen agent_snapshot_json —
		// same sticky identity rule as retryLoop — so deliverHumanAnswer and
		// /start can resume without reconfiguring a live role agent.
		if status == domain.LoopStatusRunning {
			if err := h.assertCodingRoleResumeAgent(ctx, repos, *loop, "start"); err != nil {
				return storage.LoopRecord{}, err
			}
		}
		if status == domain.LoopStatusRunning && loop.Type == string(domain.LoopTypeReviewer) && isTerminalReviewerLoopRecord(*loop) {
			return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot start terminal reviewer loop: %s", loop.ID)}
		}

		if status == domain.LoopStatusRunning {
			target, targetErr := loopTargetFromRecordCompat(*loop)
			if targetErr != nil {
				return storage.LoopRecord{}, targetErr
			}
			existing, listErr := repos.Loops.List(ctx)
			if listErr != nil {
				return storage.LoopRecord{}, listErr
			}
			if uniqueErr := assertUniqueActiveLoopCompat(existing, loop.ID, loop.ProjectID, domain.LoopType(loop.Type), target, domain.LoopStatusRunning); uniqueErr != nil {
				return storage.LoopRecord{}, uniqueErr
			}
		}

		updated := *loop
		updated.Status = string(status)
		updated.UpdatedAt = nowISO
		if status == domain.LoopStatusRunning {
			updated.NextRunAt = &nowISO
		} else {
			updated.NextRunAt = nil
		}

		if err := repos.Loops.Upsert(ctx, updated); err != nil {
			return storage.LoopRecord{}, err
		}

		switch status {
		case domain.LoopStatusPaused:
			reason := "loop paused"
			if _, err := repos.Queue.CancelByLoop(ctx, updated.ID, nowISO, &reason); err != nil {
				return storage.LoopRecord{}, err
			}
		case domain.LoopStatusRunning:
			requeued, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, updated.ID, nowISO)
			if err != nil {
				return storage.LoopRecord{}, err
			}
			if requeued == 0 {
				activeQueue, err := repos.Queue.FindActiveByLoopID(ctx, updated.ID)
				if err != nil {
					return storage.LoopRecord{}, err
				}
				if activeQueue != nil {
					break
				}
				latestQueue, err := repos.Queue.GetLatestByLoopID(ctx, updated.ID)
				if err != nil {
					return storage.LoopRecord{}, err
				}
				target, targetErr := loopTargetFromRecordCompat(updated)
				if targetErr != nil {
					return storage.LoopRecord{}, targetErr
				}
				if latestQueue != nil {
					if latestQueue.Status == "queued" || latestQueue.Status == "running" {
						break
					}
					if latestQueue.DedupeKey != "" {
						activeQueue, err := repos.Queue.FindActiveByDedupe(ctx, latestQueue.DedupeKey)
						if err != nil {
							return storage.LoopRecord{}, err
						}
						if activeQueue != nil {
							break
						}
					}
					replacement := *latestQueue
					replacement.ID = generateRequestID()
					replacement.Status = "queued"
					replacement.AvailableAt = nowISO
					replacement.Attempts = 0
					replacement.ClaimedBy = nil
					replacement.ClaimedAt = nil
					replacement.StartedAt = nil
					replacement.FinishedAt = nil
					replacement.LastError = nil
					replacement.LastErrorKind = nil
					replacement.CreatedAt = nowISO
					replacement.UpdatedAt = nowISO
					if _, _, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, replacement); err != nil {
						return storage.LoopRecord{}, err
					}
				} else {
					queueRecord, ok, queueErr := buildQueuedLoopQueueRecordCompat(updated, target, nowISO, updated.MetadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
					if queueErr != nil {
						return storage.LoopRecord{}, queueErr
					}
					if ok {
						if _, _, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, queueRecord); err != nil {
							return storage.LoopRecord{}, err
						}
					}
				}
			}
		}

		return updated, nil
	})
	if err != nil {
		if restoreErr := restoreStopGate(); restoreErr != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				typed.message = errors.Join(err, restoreErr).Error()
				return loopResponse{}, typed
			}
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: errors.Join(err, restoreErr).Error()}
		}
		var typed apiError
		if asAPIError(err, &typed) {
			return loopResponse{}, typed
		}
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if status == domain.LoopStatusRunning && h.context.TriggerSchedulerTick != nil {
		h.context.TriggerSchedulerTick()
	}

	return serializeLoop(updated), nil
}

type takeoverLoopResponse struct {
	LoopID        string `json:"loopId"`
	Vendor        string `json:"vendor,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	WorktreePath  string `json:"worktreePath,omitempty"`
	Supported     bool   `json:"supported"`
	ResumeCommand string `json:"resumeCommand,omitempty"`
	Message       string `json:"message,omitempty"`
}

// takeoverLoop parks a loop for interactive human takeover and returns the exact
// command a human runs to resume the loop's agent session (same native session id,
// in the loop's worktree). The daemon's in-flight run is already stopped by the
// wired TakeoverLoop closure; here we only shape the response + resume command.
func (h *Handler) takeoverLoop(ctx context.Context, loopID string) (takeoverLoopResponse, error) {
	if h.context.TakeoverLoop == nil {
		return takeoverLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusServiceUnavailable, message: "Takeover is not available on this daemon"}
	}
	result, err := h.context.TakeoverLoop(ctx, loopID, "Taken over by a human via looper resume")
	if err != nil {
		return takeoverLoopResponse{}, err
	}
	resp := takeoverLoopResponse{
		LoopID:       result.LoopID,
		Vendor:       result.Vendor,
		SessionID:    result.SessionID,
		WorktreePath: result.WorktreePath,
	}
	vendor := config.AgentVendor(strings.TrimSpace(result.Vendor))
	// Global agent.params (especially command/args) are owned by agent.vendor.
	// Role runs already filter via ParamsForRoleVendor; takeover must do the same
	// so a Claude role session is not handed a global Codex wrapper resume line.
	params := agent.ParamsForRoleVendor(h.context.Config.Agent.Params, h.context.Config.Agent.Vendor, vendor, nil)
	cmdLine, ok := agent.InteractiveResumeCommandLine(agent.ExecutorConfig{Vendor: vendor, Params: params}, result.WorktreePath, result.SessionID)
	resp.Supported = ok
	if ok {
		resp.ResumeCommand = cmdLine
	} else {
		resp.Message = "Interactive takeover needs a captured session id and a supported agent (codex/claude); the loop is parked in human_takeover — hand it back with `looper handback` to resume the daemon."
	}
	return resp, nil
}

// handbackLoop re-arms a taken-over loop so the daemon resumes it. It stamps the
// loop with the native session id the human drove (so the next worker run resumes
// THAT session and sees their turns), clears any queue item that survived the
// takeover race, then re-arms via the shared retry path.
func (h *Handler) handbackLoop(ctx context.Context, r *http.Request, loopID string) (any, error) {
	// Reject discard before any handback mutation. retryLoop is shared with
	// /retry, but handback must never wipe the human's interactive worktree edits
	// even if an API client includes discardWorktreeChanges on the handback body.
	if discardRequested, err := retryRequestRequestsDiscard(r); err != nil {
		return nil, err
	} else if discardRequested {
		return nil, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: "discardWorktreeChanges is not allowed on handback; human interactive worktree edits must be preserved (retry with --discard-worktree-changes after handback if needed)",
		}
	}

	services := h.context.Runtime.Services()
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	_, err := storage.WithTransactionValue(ctx, services.Coordinator.DB(), nil, func(tx *sql.Tx) (struct{}, error) {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return struct{}{}, err
		}
		if loop == nil {
			return struct{}{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
		}
		if execution, err := repos.AgentExecutions.GetLatestByLoopID(ctx, loopID); err == nil && execution != nil && execution.NativeSessionID != nil && strings.TrimSpace(*execution.NativeSessionID) != "" {
			meta, werr := loops.WriteTakeoverResume(loop.MetadataJSON, loops.TakeoverResume{SessionID: strings.TrimSpace(*execution.NativeSessionID)})
			if werr == nil {
				loop.MetadataJSON = &meta
				loop.UpdatedAt = nowISO
				if err := repos.Loops.Upsert(ctx, *loop); err != nil {
					return struct{}{}, err
				}
			}
		}
		reason := "Cleared for takeover handback"
		if _, err := repos.Queue.CancelByLoop(ctx, loopID, nowISO, &reason); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	// Handback reuses retry re-arm; fromHandback also rejects discard if body is
	// re-read after a client races another field in (defense in depth).
	return h.retryLoop(ctx, r, loopID, true)
}

// retryRequestRequestsDiscard peeks at a retry/handback JSON body for
// discardWorktreeChanges without consuming the request for a later retryLoop decode.
func retryRequestRequestsDiscard(r *http.Request) (bool, error) {
	if r == nil || r.Body == nil {
		return false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	if err != nil {
		return false, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Invalid retry request: %v", err)}
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return false, nil
	}
	var body retryLoopRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		return false, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Invalid retry request: %v", err)}
	}
	return body.DiscardWorktreeChanges != nil && *body.DiscardWorktreeChanges, nil
}

type respondLoopRequest struct {
	Answer string `json:"answer"`
}

// respondToLoop delivers a human's answer to a loop suspended mid-run as
// awaiting_human: it validates the loop is awaiting a human, stores the answer
// on the loop's HITL metadata, and transitions the loop back to running (which
// requeues it and triggers a scheduler tick) so the next run resumes the same
// agent session with the answer. This is the testable core of the HITL bridge.
func (h *Handler) respondToLoop(ctx context.Context, r *http.Request, loopID string) (loopResponse, error) {
	var body respondLoopRequest
	if r.Body != nil {
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Invalid respond request: %v", err)}
		}
	}
	return h.deliverHumanAnswer(ctx, loopID, body.Answer)
}

// deliverHumanAnswer is the shared core of the HITL respond path: it validates
// the loop is awaiting_human, stores the answer on the loop's HITL metadata, and
// transitions the loop back to running (requeue + scheduler tick). Both the
// /respond API endpoint and the Feishu card-action receiver call it.
func (h *Handler) deliverHumanAnswer(ctx context.Context, loopID string, rawAnswer string) (loopResponse, error) {
	answer := strings.TrimSpace(rawAnswer)
	if answer == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "respond requires a non-empty answer"}
	}

	services := h.context.Runtime.Services()
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	_, err := storage.WithTransactionValue(ctx, services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if loop == nil {
			return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
		}
		if loop.Status != string(domain.LoopStatusAwaitingHuman) {
			return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Loop %s is not awaiting a human (status: %s)", loopID, loop.Status)}
		}
		ask, _ := loops.ReadHITLAsk(loop.MetadataJSON)
		ask.Answer = answer
		ask.Status = "answered"
		ask.AnsweredAt = nowISO
		metadataJSON, err := loops.WriteHITLAsk(loop.MetadataJSON, ask)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		updated := *loop
		updated.MetadataJSON = stringPtrOrNil(metadataJSON)
		updated.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, updated); err != nil {
			return storage.LoopRecord{}, err
		}
		return updated, nil
	})
	if err != nil {
		var typed apiError
		if asAPIError(err, &typed) {
			return loopResponse{}, typed
		}
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	// Transition awaiting_human -> running (requeues + triggers a scheduler tick)
	// so the next claim resumes the run with the stored answer.
	return h.mutateLoopStatus(ctx, loopID, domain.LoopStatusRunning)
}

type feishuCardActionEnvelope struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	// Token is the Feishu app Verification Token, echoed by Feishu in every event
	// and card-action callback. It is the shared secret that proves the request
	// originated from Feishu rather than an arbitrary client. (v1 card-action /
	// url_verification carry it at top level; v2 events carry it in header.token.)
	Token  string `json:"token"`
	Action struct {
		Tag   string          `json:"tag"`
		Value json.RawMessage `json:"value"`
	} `json:"action"`
	// v2 event envelope, used for im.message.receive_v1 (a human typing a free-text
	// reply in the ask thread).
	Header struct {
		EventType string `json:"event_type"`
		Token     string `json:"token"`
	} `json:"header"`
	Event struct {
		Message struct {
			MessageID   string `json:"message_id"`
			RootID      string `json:"root_id"`
			ThreadID    string `json:"thread_id"`
			ChatID      string `json:"chat_id"`
			MessageType string `json:"message_type"`
			Content     string `json:"content"`
		} `json:"message"`
		Sender struct {
			SenderType string `json:"sender_type"`
			SenderID   struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
	} `json:"event"`
}

// handleFeishuCardActionRoute is the thin Feishu listener (receive side of the
// app-bot integration whose send side ships in the notifier). It receives a
// card-action callback when a human clicks an option button on an ask-card, maps
// the button value {loopSeq, answer} to the awaiting loop, and calls the shared
// respond logic in-process. It also answers Feishu's url_verification challenge.
// The whole route is gated by hitl.enabled.
//
// Transport choice: this uses the card-action WEBHOOK RECEIVER over looper's
// existing HTTP server rather than the larksuite long-connection WS SDK, to
// avoid a heavy new dependency. Point the Feishu app's event/card-callback URL
// at <daemon>/api/v1/hitl/feishu. Typed free-text replies (message events) are a
// documented future extension; button clicks are handled today.
func (h *Handler) handleFeishuCardActionRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method != http.MethodPost {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: "Feishu card-action route requires POST"})
		return
	}
	var raw []byte
	if r.Body != nil {
		defer r.Body.Close()
		raw, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	}
	var envelope feishuCardActionEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "invalid Feishu callback body"})
		return
	}
	// Resolve the configured Feishu Verification Token (a shared secret Feishu
	// echoes in every callback). This is the ONLY origin check on this route, and
	// it is independent of authMode because Feishu's servers cannot send a looper
	// Bearer token.
	expectedToken := ""
	if envName := strings.TrimSpace(h.context.Config.Notifications.Webhook.VerificationTokenEnv); envName != "" {
		expectedToken = strings.TrimSpace(os.Getenv(envName))
	}
	// v1 card-action / url_verification carry the token at the top level; v2 events
	// carry it in header.token.
	presentedToken := strings.TrimSpace(envelope.Token)
	if presentedToken == "" {
		presentedToken = strings.TrimSpace(envelope.Header.Token)
	}
	tokenMatches := expectedToken != "" && subtle.ConstantTimeCompare([]byte(presentedToken), []byte(expectedToken)) == 1

	// Feishu URL-verification handshake: echo the challenge verbatim. When a token
	// is configured, require it to match even for the handshake. This path produces
	// no work and must succeed while admission is starting/stopping/degraded so
	// Feishu can register or revalidate the callback URL.
	if envelope.Type == "url_verification" {
		if expectedToken != "" && !tokenMatches {
			h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeUnauthorized, status: http.StatusUnauthorized, message: "Feishu verification token mismatch"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
		return
	}
	// Real card actions and thread replies mutate HITL state; require admission.
	if typed, denied := h.admissionMutationDenial(); denied {
		h.writeError(w, requestID, typed)
		return
	}
	if !h.context.Config.HITL.Enabled {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusForbidden, message: "hitl.enabled is false"})
		return
	}
	// A card action delivers human-authored text into an agent's coding session, so
	// it MUST be authenticated. Fail closed: require a configured, matching
	// verification token — otherwise any client that can reach this route could
	// inject arbitrary answers into any awaiting_human loop.
	if expectedToken == "" {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusForbidden, message: "Feishu card-action callback requires notifications.webhook.verificationTokenEnv to be configured"})
		return
	}
	if !tokenMatches {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeUnauthorized, status: http.StatusUnauthorized, message: "Feishu verification token mismatch"})
		return
	}
	// A human typing a free-text reply in the ask thread arrives as a message event
	// rather than a card action — route it to the free-text handler.
	if envelope.Header.EventType == "im.message.receive_v1" {
		h.handleFeishuThreadReply(w, r, requestID, envelope)
		return
	}
	var value struct {
		LoopSeq string `json:"loopSeq"`
		Answer  string `json:"answer"`
	}
	if len(envelope.Action.Value) > 0 {
		_ = json.Unmarshal(envelope.Action.Value, &value)
	}
	loopSeq := strings.TrimSpace(value.LoopSeq)
	answer := strings.TrimSpace(value.Answer)
	if loopSeq == "" || answer == "" {
		h.writeError(w, requestID, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "card action must carry value.loopSeq and value.answer"})
		return
	}
	loop, err := h.resolveLoop(r.Context(), loopSeq)
	if err != nil {
		var typed apiError
		if !asAPIError(err, &typed) {
			typed = internalServerError(err)
		}
		h.writeError(w, requestID, typed)
		return
	}
	if _, err := h.deliverHumanAnswer(r.Context(), loop.ID, answer); err != nil {
		var typed apiError
		if !asAPIError(err, &typed) {
			typed = internalServerError(err)
		}
		h.writeError(w, requestID, typed)
		return
	}
	h.writeSuccess(w, requestID, map[string]any{"loopSeq": loopSeq, "delivered": true})
}

// handleFeishuThreadReply consumes a human's free-text reply typed in an ask
// thread (a Feishu im.message.receive_v1 event). It reverse-maps the thread root
// to the loop that asked and delivers the typed text as the answer — the lossless,
// type-anything counterpart to clicking an option button. Ordinary thread chatter
// (no matching awaiting_human loop) is ignored with 200 so Feishu stops retrying.
func (h *Handler) handleFeishuThreadReply(w http.ResponseWriter, r *http.Request, requestID string, envelope feishuCardActionEnvelope) {
	msg := envelope.Event.Message
	if msg.MessageType != "text" {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "non-text message"})
		return
	}
	rootID := strings.TrimSpace(msg.RootID)
	if rootID == "" {
		rootID = strings.TrimSpace(msg.ThreadID)
	}
	if rootID == "" {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "not a thread reply"})
		return
	}
	var textContent struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(msg.Content), &textContent)
	answer := strings.TrimSpace(textContent.Text)
	if answer == "" {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "empty text"})
		return
	}
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.FeishuThreads == nil {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "thread mapping unavailable"})
		return
	}
	loopID, err := services.Repositories.FeishuThreads.LoopByRoot(r.Context(), rootID)
	if err != nil || strings.TrimSpace(loopID) == "" {
		h.writeSuccess(w, requestID, map[string]any{"delivered": false, "reason": "no loop for thread"})
		return
	}
	// deliverHumanAnswer only accepts an awaiting_human loop, so this naturally
	// drops the bot's own thread posts, replies after the loop resumed, and any
	// duplicate Feishu retries.
	if _, err := h.deliverHumanAnswer(r.Context(), loopID, answer); err != nil {
		h.writeSuccess(w, requestID, map[string]any{"loopId": loopID, "delivered": false, "reason": "loop not awaiting a human"})
		return
	}
	h.writeSuccess(w, requestID, map[string]any{"loopId": loopID, "delivered": true})
}

// retryLoop re-arms a loop for another scheduler pass. fromHandback is true when
// invoked via /loops/{id}/handback so discardWorktreeChanges is rejected: that
// path preserves human interactive edits in the worktree for the resumed session.
func (h *Handler) retryLoop(ctx context.Context, r *http.Request, loopID string, fromHandback bool) (retryLoopResponse, error) {
	var body retryLoopRequest
	if r.Body != nil {
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Invalid retry request: %v", err)}
		}
	}
	mode := strings.TrimSpace(body.Mode)
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "resume" && mode != "rediscover" {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Unsupported retry mode: %s", mode)}
	}
	if mode != "auto" {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusNotImplemented, message: fmt.Sprintf("Retry mode %s is not implemented safely yet; use mode auto", mode)}
	}
	resetAttempts := true
	if body.ResetAttempts != nil {
		resetAttempts = *body.ResetAttempts
	}
	if !resetAttempts {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "resetAttempts=false is not supported for explicit operator retry"}
	}
	discardWorktreeChanges := body.DiscardWorktreeChanges != nil && *body.DiscardWorktreeChanges
	if discardWorktreeChanges && fromHandback {
		return retryLoopResponse{}, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: "discardWorktreeChanges is not allowed on handback; human interactive worktree edits must be preserved (retry with --discard-worktree-changes after handback if needed)",
		}
	}

	// Serialize per-loop retry with start/requeue so discard cannot race another
	// retry or /loops/{id}/start that enqueues replacement work between preflight
	// and reset (or a scheduler-started run for that replacement).
	unlock := h.lockLoopRetry(loopID)
	defer unlock()

	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())

	// Always resolve the loop target and hold the same-target lock for the whole
	// retry window — not only when discarding. Regular retry and /start for a
	// *different* failed loop on this target would otherwise create an active
	// queue item after discard preflight and before git reset, then the discard
	// TX would conflict after the worktree was already wiped.
	preflightLoop, err := services.Repositories.Loops.GetByID(ctx, loopID)
	if err != nil {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if preflightLoop == nil {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
	}
	target, targetErr := loopTargetFromRecordCompat(*preflightLoop)
	if targetErr != nil {
		return retryLoopResponse{}, targetErr
	}
	unlockTarget := h.lockLoopTarget(preflightLoop.ProjectID, domain.LoopType(preflightLoop.Type), target)
	defer unlockTarget()

	// Opt-in discard runs before requeue so git mutation stays outside the
	// queue transaction. Every non-mutating retry blocker must pass first so a
	// later precondition failure never leaves discarded worktree changes
	// without creating a replacement queue item.
	var worktreeDiscard *worktreeDiscardResult
	if discardWorktreeChanges {
		// Runtime HITL poll requeues awaiting_human loops without the API lock
		// (hitl_github_poll / Feishu helpers). Refuse discard so a poll-delivered
		// answer cannot requeue between preflight and git reset, wiping the
		// worktree for the answered continuation when the retry TX then conflicts.
		// human_takeover pins the same worktree for interactive human edits;
		// /handback already rejects discard, and direct /retry must match that.
		if err := rejectDiscardWhileParkedForHuman(preflightLoop.Status, loopID); err != nil {
			return retryLoopResponse{}, err
		}

		if err := h.assertLoopRetryPreconditions(ctx, services.Repositories, *preflightLoop, nowISO); err != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		// Same-type uniqueness is not enough for discard: PR worktrees are shared
		// across fixer/reviewer/worker. An already queued/running/waiting/
		// human_takeover sibling is not held by the target mutex (that only
		// serializes mutations), so refuse git reset/clean while any worktree-
		// owning sibling holds the PR checkout.
		if err := h.assertDiscardSharedPRWorktreeClear(ctx, services.Repositories, *preflightLoop); err != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}

		// Recheck immediately before git mutation as defense in depth. Runtime
		// free-text enqueue now shares LockLoopRequeue with this path, so the
		// common race is serialized; this snapshot still catches any unlocked
		// requeue injected under discardBeforeGitHook in tests (or future
		// callers that forget the shared guard).
		if h.discardBeforeGitHook != nil {
			h.discardBeforeGitHook(loopID)
		}
		freshLoop, freshErr := services.Repositories.Loops.GetByID(ctx, loopID)
		if freshErr != nil {
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: freshErr.Error()}
		}
		if freshLoop == nil {
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
		}
		if err := rejectDiscardWhileParkedForHuman(freshLoop.Status, loopID); err != nil {
			return retryLoopResponse{}, err
		}
		if err := h.assertLoopRetryPreconditions(ctx, services.Repositories, *freshLoop, nowISO); err != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		if err := h.assertDiscardSharedPRWorktreeClear(ctx, services.Repositories, *freshLoop); err != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		preflightLoop = freshLoop

		discardResult, discardErr := h.discardLoopWorktreeChanges(ctx, services, *preflightLoop)
		if discardErr != nil {
			var typed apiError
			if asAPIError(discardErr, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: discardErr.Error()}
		}
		worktreeDiscard = &discardResult
	}

	type retryResult struct {
		loop        storage.LoopRecord
		queueItemID *string
	}
	// Clear the sticky stop gate before the queue item becomes claimable.
	// Clearing after the TX commit races a concurrent scheduler tick that can
	// claim the new item, pass the parked check (loop is queued), then fail
	// AgentExecutor.Start with ErrSpawnLoopStopping and back off the retry.
	// If the TX fails (or publishes no replacement work), restore the gate so
	// a failed retry cannot reopen AdmitSpawn for stale pre-stop runners.
	gateWasActive := false
	if services.ActiveExecutions != nil {
		// Clear and report under one lock so abort restore covers any gate this
		// call removed (including one set by concurrent BeginLoopStop).
		gateWasActive = services.ActiveExecutions.ClearLoopStop(loopID)
	}
	restoreStopGate := func() error {
		if gateWasActive && services.ActiveExecutions != nil {
			return services.ActiveExecutions.RestoreLoopStop(loopID)
		}
		return nil
	}
	if h.retryAfterClearStopGateHook != nil {
		h.retryAfterClearStopGateHook(loopID)
	}
	result, err := storage.WithTransactionValue(ctx, services.Coordinator.DB(), nil, func(tx *sql.Tx) (retryResult, error) {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return retryResult{}, err
		}
		if loop == nil {
			return retryResult{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
		}
		if err := h.assertLoopRetryPreconditions(ctx, repos, *loop, nowISO); err != nil {
			return retryResult{}, err
		}
		// When discard already mutated the worktree, re-check shared-PR siblings
		// inside the TX so a concurrent runtime requeue/create that raced past
		// preflight cannot leave both an active sibling and a successful retry.
		if discardWorktreeChanges {
			if err := h.assertDiscardSharedPRWorktreeClear(ctx, repos, *loop); err != nil {
				return retryResult{}, err
			}
		}

		target, targetErr := loopTargetFromRecordCompat(*loop)
		if targetErr != nil {
			return retryResult{}, targetErr
		}
		latestQueue, err := repos.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return retryResult{}, err
		}

		queueLoop := *loop
		queueLoop.Status = string(domain.LoopStatusQueued)
		queueLoop.NextRunAt = &nowISO
		queueLoop.UpdatedAt = nowISO
		if queueLoop.Type == string(domain.LoopTypeReviewer) {
			metadataJSON, metadataErr := resetReviewerLoopRetryMetadata(queueLoop.MetadataJSON)
			if metadataErr != nil {
				return retryResult{}, metadataErr
			}
			queueLoop.MetadataJSON = metadataJSON
		}
		var queueRecord storage.QueueItemRecord
		var ok bool
		if latestQueue != nil {
			queueRecord = *latestQueue
			queueRecord.ID = generateRequestID()
			queueRecord.Status = "queued"
			queueRecord.AvailableAt = nowISO
			if resetAttempts {
				queueRecord.Attempts = 0
			}
			queueRecord.ClaimedBy = nil
			queueRecord.ClaimedAt = nil
			queueRecord.StartedAt = nil
			queueRecord.FinishedAt = nil
			queueRecord.LastError = nil
			queueRecord.LastErrorKind = nil
			queueRecord.CreatedAt = nowISO
			queueRecord.UpdatedAt = nowISO
			ok = true
		} else {
			built, builtOK, queueErr := buildQueuedLoopQueueRecordCompat(queueLoop, target, nowISO, queueLoop.MetadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
			if queueErr != nil {
				return retryResult{}, queueErr
			}
			queueRecord = built
			ok = builtOK
		}
		if !ok {
			return retryResult{loop: *loop}, nil
		}
		// Dedupe is already asserted by assertLoopRetryPreconditions; re-check
		// inside the transaction for races between preflight and commit.
		if queueRecord.DedupeKey != "" {
			activeDedupe, err := repos.Queue.FindActiveByDedupe(ctx, queueRecord.DedupeKey)
			if err != nil {
				return retryResult{}, err
			}
			if activeDedupe != nil {
				return retryResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusConflict, message: fmt.Sprintf("Cannot retry loop %s while dedupe queue item %s is active", loop.ID, activeDedupe.ID)}
			}
		}

		updated := queueLoop
		if err := repos.Loops.Upsert(ctx, updated); err != nil {
			return retryResult{}, err
		}
		persisted, _, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, queueRecord)
		if err != nil {
			return retryResult{}, err
		}
		return retryResult{loop: updated, queueItemID: &persisted.ID}, nil
	})
	if err != nil {
		if restoreErr := restoreStopGate(); restoreErr != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				typed.message = errors.Join(err, restoreErr).Error()
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: errors.Join(err, restoreErr).Error()}
		}
		var typed apiError
		if asAPIError(err, &typed) {
			return retryLoopResponse{}, typed
		}
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if result.queueItemID == nil {
		// No replacement work published; keep sticky stop closed if it was.
		if restoreErr := restoreStopGate(); restoreErr != nil {
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: restoreErr.Error()}
		}
	}
	if h.context.TriggerSchedulerTick != nil {
		h.context.TriggerSchedulerTick()
	}
	return retryLoopResponse{
		Loop:                   serializeLoop(result.loop),
		QueueItemID:            result.queueItemID,
		Mode:                   mode,
		ResetAttempts:          resetAttempts,
		DiscardWorktreeChanges: discardWorktreeChanges,
		WorktreeDiscard:        worktreeDiscard,
	}, nil
}

func (h *Handler) buildLoopLogsResponse(ctx context.Context, loop storage.LoopRecord) (loopLogsResponse, error) {
	services := h.context.Runtime.Services()
	if latestLoop, err := services.Repositories.Loops.GetByID(ctx, loop.ID); err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	} else if latestLoop != nil {
		loop = *latestLoop
	}

	latestRun, err := services.Repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	return h.buildLogsResponseForRun(ctx, loop, latestRun)
}

func (h *Handler) buildRunLogsResponse(ctx context.Context, runID string) (loopLogsResponse, error) {
	services := h.context.Runtime.Services()
	run, err := services.Repositories.Runs.GetByID(ctx, runID)
	if err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if run == nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeRunNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Run not found: %s", runID)}
	}

	loop, err := services.Repositories.Loops.GetByID(ctx, run.LoopID)
	if err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if loop == nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found for run: %s", runID)}
	}

	return h.buildLogsResponseForRun(ctx, *loop, run)
}

func (h *Handler) buildLogsResponseForRun(ctx context.Context, loop storage.LoopRecord, run *storage.RunRecord) (loopLogsResponse, error) {
	services := h.context.Runtime.Services()
	var runPayload *loopLogsRunResponse
	var agentPayload *loopLogsAgentPayload
	if run != nil {
		runPayload = &loopLogsRunResponse{
			RunID:        run.ID,
			Status:       run.Status,
			CurrentStep:  run.CurrentStep,
			StartedAt:    run.StartedAt,
			EndedAt:      run.EndedAt,
			Summary:      run.Summary,
			ErrorMessage: run.ErrorMessage,
		}

		latestAgent, agentErr := services.Repositories.AgentExecutions.GetLatestByRunID(ctx, run.ID)
		if agentErr != nil {
			return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: agentErr.Error()}
		}
		if latestAgent != nil {
			stdout, stderr := parseAgentOutput(h.context.Config.Daemon.LogDir, latestAgent.OutputJSON)
			agentPayload = &loopLogsAgentPayload{
				ExecutionID:     latestAgent.ID,
				Vendor:          latestAgent.Vendor,
				Status:          latestAgent.Status,
				PID:             latestAgent.PID,
				StartedAt:       latestAgent.StartedAt,
				EndedAt:         latestAgent.EndedAt,
				HeartbeatCount:  latestAgent.HeartbeatCount,
				LastHeartbeatAt: latestAgent.LastHeartbeatAt,
				Summary:         latestAgent.Summary,
				ParseStatus:     latestAgent.ParseStatus,
				ErrorMessage:    latestAgent.ErrorMessage,
				Stdout:          stdout,
				Stderr:          stderr,
			}
		}
	}

	return loopLogsResponse{Seq: loop.Seq, LoopID: loop.ID, LoopType: loop.Type, LoopStatus: loop.Status, Run: runPayload, Agent: agentPayload}, nil
}

func serializeLoop(loop storage.LoopRecord) loopResponse {
	return loopResponse{
		ID:           loop.ID,
		Seq:          loop.Seq,
		ProjectID:    loop.ProjectID,
		Type:         loop.Type,
		TargetType:   loop.TargetType,
		TargetID:     loop.TargetID,
		Repo:         loop.Repo,
		PRNumber:     loop.PRNumber,
		Status:       loop.Status,
		ConfigJSON:   loop.ConfigJSON,
		MetadataJSON: loop.MetadataJSON,
		LastRunAt:    loop.LastRunAt,
		NextRunAt:    loop.NextRunAt,
		CreatedAt:    loop.CreatedAt,
		UpdatedAt:    loop.UpdatedAt,
	}
}

// serializeLoopWithDiagnostics loads latest queue/run and attaches attempt/error fields.
func (h *Handler) serializeLoopWithDiagnostics(ctx context.Context, loop storage.LoopRecord) (loopResponse, error) {
	view := serializeLoop(loop)
	services := h.context.Runtime.Services()
	var latestQueue *storage.QueueItemRecord
	var latestRun *storage.RunRecord
	if services.Repositories != nil && services.Repositories.Queue != nil {
		queue, err := services.Repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		latestQueue = queue
	}
	if services.Repositories != nil && services.Repositories.Runs != nil {
		run, err := services.Repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		latestRun = run
	}
	decorateLoopDiagnostics(&view, latestQueue, latestRun)
	return view, nil
}

func serializeRun(run storage.RunRecord) runResponse {
	return runResponse{
		ID:                run.ID,
		LoopID:            run.LoopID,
		Status:            run.Status,
		CurrentStep:       run.CurrentStep,
		LastCompletedStep: run.LastCompletedStep,
		CheckpointJSON:    run.CheckpointJSON,
		Summary:           run.Summary,
		ErrorMessage:      run.ErrorMessage,
		StartedAt:         run.StartedAt,
		LastHeartbeatAt:   run.LastHeartbeatAt,
		EndedAt:           run.EndedAt,
		CreatedAt:         run.CreatedAt,
		UpdatedAt:         run.UpdatedAt,
	}
}

func buildLoopTarget(targetType string, body createLoopRequest) (domain.LoopTarget, error) {
	switch targetType {
	case string(domain.LoopTargetTypeProject):
		targetID := normalizeProjectTargetID(derefString(body.TargetID))
		if targetID == "" {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "targetId is required"}
		}
		return domain.LoopTarget{TargetType: domain.LoopTargetTypeProject, ProjectID: targetID}, nil
	case string(domain.LoopTargetTypeIssue):
		if strings.TrimSpace(derefString(body.Repo)) == "" {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo is required"}
		}
		issueNumber := int64(0)
		if body.IssueNumber != nil {
			issueNumber = *body.IssueNumber
		} else {
			parsed, err := parseIssueNumber(strings.TrimSpace(derefString(body.TargetID)))
			if err != nil {
				return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "issueNumber is required"}
			}
			issueNumber = parsed
		}
		if issueNumber <= 0 {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "issueNumber must be a positive integer"}
		}
		return domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, Repo: strings.TrimSpace(derefString(body.Repo)), IssueNumber: issueNumber}, nil
	case string(domain.LoopTargetTypePullRequest):
		if strings.TrimSpace(derefString(body.Repo)) == "" {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo is required"}
		}
		if body.PRNumber == nil || *body.PRNumber <= 0 {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "prNumber must be a positive integer"}
		}
		return domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: strings.TrimSpace(derefString(body.Repo)), PRNumber: *body.PRNumber}, nil
	default:
		return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("loop.targetType must be one of: %s, %s, %s", domain.LoopTargetTypeProject, domain.LoopTargetTypePullRequest, domain.LoopTargetTypeIssue)}
	}
}

func normalizeMetadataJSON(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}

	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "metadata must be a JSON object"}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	text := string(encoded)
	return &text, nil
}

func manualPlannerMetadataJSON(existing *string, issueNumber int64) (*string, error) {
	metadata := map[string]any{}
	if existing != nil && strings.TrimSpace(*existing) != "" {
		if err := json.Unmarshal([]byte(*existing), &metadata); err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "metadata must be a JSON object"}
		}
	}
	metadata["issueNumber"] = issueNumber
	metadata["manual"] = true
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	text := string(encoded)
	return &text, nil
}

func manualFixerMetadataJSON(existing *string, nowISO string) (*string, error) {
	metadata := map[string]any{}
	if existing != nil && strings.TrimSpace(*existing) != "" {
		if err := json.Unmarshal([]byte(*existing), &metadata); err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "metadata must be a JSON object"}
		}
	}
	metadata["manual"] = true
	followUpdates, _ := metadata["followUpdates"].(bool)
	loopMeta, _ := metadata["loop"].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	metadata["followUpdates"] = followUpdates
	loopMeta["enabled"] = followUpdates
	if _, ok := loopMeta["status"].(string); !ok {
		loopMeta["status"] = "active"
	}
	if _, ok := loopMeta["startTime"].(string); !ok && strings.TrimSpace(nowISO) != "" {
		loopMeta["startTime"] = nowISO
	}
	metadata["loop"] = loopMeta
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	text := string(encoded)
	return &text, nil
}

func reviewerLoopMetadataJSON(existing *string, reviewerConfig config.ReviewerConfig, target domain.LoopTarget, nowISO string, force bool) (*string, error) {
	metadata := parseJSONObject(existing)
	if force {
		metadata["manual"] = true
	}
	loopMeta, _ := metadata["loop"].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	if _, ok := metadata["followUpdates"].(bool); !ok {
		if enabled, ok := loopMeta["enabled"].(bool); ok {
			metadata["followUpdates"] = enabled
		} else {
			metadata["followUpdates"] = reviewerConfig.Loop.EnabledByDefault
		}
	}
	if _, ok := loopMeta["enabled"].(bool); !ok {
		loopMeta["enabled"] = metadata["followUpdates"]
	}
	if _, ok := loopMeta["status"].(string); !ok {
		loopMeta["status"] = "active"
	}
	if _, ok := loopMeta["startTime"].(string); !ok {
		loopMeta["startTime"] = nowISO
	}
	loopMeta["scope"] = string(reviewerConfig.Scope)
	loopMeta["quietPeriodSeconds"] = reviewerConfig.Loop.QuietPeriodSeconds
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
	reviewEventsRaw, hasReviewEvents := metadata["reviewEvents"]
	reviewEventsMeta, _ := reviewEventsRaw.(map[string]any)
	if hasReviewEvents && reviewEventsMeta == nil {
		return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "reviewEvents must be a JSON object"}
	}
	if reviewEventsMeta == nil {
		reviewEventsMeta = map[string]any{}
	}
	if cleanRaw, present := reviewEventsMeta["clean"]; present {
		clean, ok := cleanRaw.(string)
		if !ok {
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "reviewEvents.clean must be COMMENT or APPROVE"}
		}
		if !isValidReviewerCleanReviewEvent(clean) {
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "reviewEvents.clean must be COMMENT or APPROVE"}
		}
	} else {
		reviewEventsMeta["clean"] = string(reviewerConfig.ReviewEvents.Clean)
	}
	if blockingRaw, present := reviewEventsMeta["blocking"]; present {
		blocking, ok := blockingRaw.(string)
		if !ok {
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "reviewEvents.blocking must be COMMENT or REQUEST_CHANGES"}
		}
		if !isValidReviewerBlockingReviewEvent(blocking) {
			return nil, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "reviewEvents.blocking must be COMMENT or REQUEST_CHANGES"}
		}
	} else {
		reviewEventsMeta["blocking"] = string(reviewerConfig.ReviewEvents.Blocking)
	}
	metadata["reviewEvents"] = reviewEventsMeta
	if target.Repo != "" {
		loopMeta["repo"] = target.Repo
	}
	if target.PRNumber > 0 {
		loopMeta["prNumber"] = target.PRNumber
	}
	metadata["loop"] = loopMeta
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	text := string(encoded)
	return &text, nil
}

func isValidReviewerCleanReviewEvent(value string) bool {
	switch config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(value))) {
	case config.ReviewerReviewEventComment, config.ReviewerReviewEventApprove:
		return true
	default:
		return false
	}
}

func removeDeprecatedReviewerLoopBudgetMetadata(loopMeta map[string]any) {
	for _, key := range deprecatedReviewerLoopBudgetMetadataKeys {
		delete(loopMeta, key)
	}
	reason, _ := loopMeta["terminationReason"].(string)
	if isDeprecatedReviewerLoopBudgetReason(reason) {
		delete(loopMeta, "terminationReason")
		if status, _ := loopMeta["status"].(string); status == "failed" || status == "terminated" {
			loopMeta["status"] = "active"
		}
	}
}

var deprecatedReviewerLoopBudgetMetadataKeys = []string{
	"maxIterationsPerPR",
	"maxIterationsPerHead",
	"maxWallClockSeconds",
	"maxConsecutiveFailures",
	"maxAgentExecutionsPerPR",
}

func isDeprecatedReviewerLoopBudgetReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "max_iterations_per_pr", "max_iterations_per_head", "max_wall_clock", "max_consecutive_failures", "max_agent_executions_per_pr":
		return true
	default:
		return false
	}
}

func isValidReviewerBlockingReviewEvent(value string) bool {
	switch config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(value))) {
	case config.ReviewerReviewEventComment, config.ReviewerReviewEventRequestChanges:
		return true
	default:
		return false
	}
}

func isTerminalReviewerLoopRecord(loop storage.LoopRecord) bool {
	if loop.Status == "terminated" || loop.Status == "stopped" || loop.Status == "failed" {
		return true
	}
	metadata := parseJSONObject(loop.MetadataJSON)
	loopMeta, _ := metadata["loop"].(map[string]any)
	status, _ := loopMeta["status"].(string)
	return status == "terminated" || status == "stopped" || status == "failed"
}

// rejectDiscardWhileParkedForHuman refuses discard+retry when the loop is
// parked for a human so interactive worktree edits stay intact. awaiting_human
// is blocked against HITL poll requeue races; human_takeover matches /handback
// which never honors discardWorktreeChanges.
func rejectDiscardWhileParkedForHuman(status, loopID string) error {
	switch status {
	case string(domain.LoopStatusAwaitingHuman):
		return apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusConflict,
			message: fmt.Sprintf("Cannot discard worktree changes while loop %s is awaiting_human; answer or cancel the HITL ask first, or retry without --discard-worktree-changes", loopID),
		}
	case string(domain.LoopStatusHumanTakeover):
		return apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusConflict,
			message: fmt.Sprintf("Cannot discard worktree changes while loop %s is human_takeover; hand back without --discard-worktree-changes first, or retry without discard", loopID),
		}
	default:
		return nil
	}
}

// assertDiscardSharedPRWorktreeClear refuses discard when another loop of any
// type already holds a worktree-owning status on the same pull-request target.
// assertUniqueActiveLoopCompat only conflicts same-type loops; managed PR
// worktrees (looper-fix-<project>-pr-N) are shared across fixer/reviewer/
// worker, so a sibling that is queued, running, waiting, failed, interrupted,
// or human_takeover would lose its checkout to git reset/clean. Non-PR targets
// and non-discard retries are unaffected.
func (h *Handler) assertDiscardSharedPRWorktreeClear(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord) error {
	if loop.TargetType != string(domain.LoopTargetTypePullRequest) {
		return nil
	}
	existing, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	return assertNoActiveSiblingPRWorktreeLoops(existing, loop)
}

// isDiscardBlockingSiblingPRStatus reports whether a sibling loop's status
// still pins the shared PR managed worktree. Includes IsConflictingActiveLoopStatus
// plus waiting/failed/interrupted: those are intentionally excluded from
// uniqueness conflicts (reviewer debounce can sit waiting while another type
// fails; failed/interrupted are retryable terminals), but worktree cleanup
// (protectsLoopStatus) and ownership still treat them as active owners whose
// checkpointed local state must not be wiped by a sibling discard retry.
func isDiscardBlockingSiblingPRStatus(status domain.LoopStatus) bool {
	if domain.IsConflictingActiveLoopStatus(status) || status == domain.LoopStatusWaiting {
		return true
	}
	return status == domain.LoopStatusFailed || status == domain.LoopStatusInterrupted
}

// assertNoActiveSiblingPRWorktreeLoops reports a conflict when any other
// worktree-owning loop on the same project+PR key exists, regardless of loop
// type. Used only for destructive discard preflight.
func assertNoActiveSiblingPRWorktreeLoops(existing []storage.LoopRecord, candidate storage.LoopRecord) error {
	if candidate.TargetType != string(domain.LoopTargetTypePullRequest) {
		return nil
	}
	candidateKey := loopTargetKeyFromRecordCompat(candidate)
	if candidateKey == "pull_request:" {
		return nil
	}
	for _, loop := range existing {
		if loop.ID == candidate.ID || loop.ProjectID != candidate.ProjectID {
			continue
		}
		if loop.TargetType != string(domain.LoopTargetTypePullRequest) {
			continue
		}
		if !isDiscardBlockingSiblingPRStatus(domain.LoopStatus(loop.Status)) {
			continue
		}
		if loopTargetKeyFromRecordCompat(loop) != candidateKey {
			continue
		}
		return apiError{
			code:   pkgapi.ErrorCodeLoopConflict,
			status: http.StatusConflict,
			message: fmt.Sprintf(
				"Cannot discard worktree changes for loop %s while active %s loop %s shares the same PR worktree (%s)",
				candidate.ID, loop.Type, loop.ID, candidateKey,
			),
		}
	}
	return nil
}

// assertLoopRetryPreconditions validates non-mutating retry blockers that must
// pass before any destructive worktree discard or requeue. Callers that discard
// dirty worktree state must invoke this first so a failed retry never deletes
// local changes without creating a replacement queue item.
func (h *Handler) assertLoopRetryPreconditions(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) error {
	if strings.TrimSpace(loop.ProjectID) != "" {
		if _, err := requireActiveProjectRecord(ctx, repos.Projects, loop.ProjectID); err != nil {
			return err
		}
	}
	if loop.Status == string(domain.LoopStatusStopped) || loop.Status == string(domain.LoopStatusTerminated) || loop.Status == string(domain.LoopStatusCompleted) {
		return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot retry terminal %s loop: %s", loop.Status, loop.ID)}
	}
	if loop.Type == string(domain.LoopTypeReviewer) {
		if terminalMetadataStatus := terminalReviewerRetryMetadataStatus(loop); terminalMetadataStatus != "" {
			return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot retry terminal reviewer metadata %s loop: %s", terminalMetadataStatus, loop.ID)}
		}
	}
	if err := h.assertCodingRoleRetryAgent(ctx, repos, loop); err != nil {
		return err
	}
	runningRuns, err := repos.Runs.ListByStatus(ctx, string(domain.RunStatusRunning))
	if err != nil {
		return err
	}
	for _, run := range runningRuns {
		if run.LoopID == loop.ID {
			return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusConflict, message: fmt.Sprintf("Cannot retry loop %s while a run is active", loop.ID)}
		}
	}
	activeQueue, err := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if activeQueue != nil {
		return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusConflict, message: fmt.Sprintf("Cannot retry loop %s while queue item %s is active", loop.ID, activeQueue.ID)}
	}

	target, targetErr := loopTargetFromRecordCompat(loop)
	if targetErr != nil {
		return targetErr
	}
	existing, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	if uniqueErr := assertUniqueActiveLoopCompat(existing, loop.ID, loop.ProjectID, domain.LoopType(loop.Type), target, domain.LoopStatusQueued); uniqueErr != nil {
		return uniqueErr
	}

	latestQueue, err := repos.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return err
	}
	dedupeKey := ""
	if latestQueue != nil {
		dedupeKey = latestQueue.DedupeKey
	} else {
		// Match requeue path: when there is no prior queue row, building the
		// replacement record can fail on target/repo requirements and must
		// block discard just as it blocks requeue.
		built, ok, queueErr := buildQueuedLoopQueueRecordCompat(loop, target, nowISO, loop.MetadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
		if queueErr != nil {
			return queueErr
		}
		if ok {
			dedupeKey = built.DedupeKey
		}
	}
	if dedupeKey != "" {
		activeDedupe, err := repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
		if err != nil {
			return err
		}
		if activeDedupe != nil {
			return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusConflict, message: fmt.Sprintf("Cannot retry loop %s while dedupe queue item %s is active", loop.ID, activeDedupe.ID)}
		}
	}
	return nil
}

func terminalReviewerRetryMetadataStatus(loop storage.LoopRecord) string {
	if loop.MetadataJSON == nil || strings.TrimSpace(*loop.MetadataJSON) == "" {
		return ""
	}
	metadata := parseJSONObject(loop.MetadataJSON)
	loopMeta, _ := metadata["loop"].(map[string]any)
	if loopMeta == nil {
		return ""
	}
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
	status, _ := loopMeta["status"].(string)
	if status == "terminated" || status == "stopped" {
		return status
	}
	return ""
}

func resetReviewerLoopRetryMetadata(current *string) (*string, error) {
	if current == nil || strings.TrimSpace(*current) == "" {
		return current, nil
	}
	metadata := parseJSONObject(current)
	loopMeta, ok := metadata["loop"].(map[string]any)
	if !ok || loopMeta == nil {
		return current, nil
	}
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
	loopMeta["status"] = "queued"
	metadata["loop"] = loopMeta
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	value := string(encoded)
	return &value, nil
}

func buildQueuedLoopQueueRecordCompat(record storage.LoopRecord, target domain.LoopTarget, nowISO string, metadataJSON *string, maxAttempts int64) (storage.QueueItemRecord, bool, error) {
	queueType := domain.LoopType(record.Type)
	if queueType != domain.LoopTypeReviewer && queueType != domain.LoopTypeFixer && queueType != domain.LoopTypeWorker && queueType != domain.LoopTypePlanner {
		return storage.QueueItemRecord{}, false, nil
	}

	projectIDCopy := record.ProjectID
	loopID := record.ID
	queueRecord := storage.QueueItemRecord{
		ID:          generateRequestID(),
		ProjectID:   &projectIDCopy,
		LoopID:      &loopID,
		Type:        record.Type,
		TargetType:  record.TargetType,
		TargetID:    derefString(record.TargetID),
		Repo:        record.Repo,
		PRNumber:    record.PRNumber,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: maxAttempts,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}

	switch queueType {
	case domain.LoopTypePlanner:
		repo := strings.TrimSpace(derefString(record.Repo))
		issueNumber := target.IssueNumber
		if target.TargetType != domain.LoopTargetTypeIssue || repo == "" || issueNumber <= 0 {
			return storage.QueueItemRecord{}, false, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s loop requires repo and issueNumber", record.Type)}
		}
		lockKey := storage.IssueLockKey(record.ProjectID, repo, issueNumber)
		targetID := fmt.Sprintf("issue:%s:%d", repo, issueNumber)
		manual := false
		if metadata := parseJSONObject(metadataJSON); metadata["manual"] == true {
			if boolValue, ok := metadata["manual"].(bool); ok {
				manual = boolValue
			}
		}
		payload := map[string]any{"issueNumber": issueNumber}
		if manual {
			payload["manual"] = true
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return storage.QueueItemRecord{}, false, err
		}
		payloadJSON := string(payloadBytes)
		queueRecord.TargetType = string(domain.LoopTargetTypeIssue)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = nil
		queueRecord.DedupeKey = fmt.Sprintf("planner:%s:%s:%s:%d", record.ProjectID, record.ID, repo, issueNumber)
		queueRecord.Priority = storage.QueuePriorityPlanner
		queueRecord.LockKey = &lockKey
		queueRecord.PayloadJSON = &payloadJSON
	case domain.LoopTypeReviewer:
		repo := strings.TrimSpace(derefString(record.Repo))
		if repo == "" || record.PRNumber == nil {
			return storage.QueueItemRecord{}, false, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s loop requires repo and prNumber", record.Type)}
		}
		prNumber := *record.PRNumber
		lockKey := storage.PullRequestLockKey(record.ProjectID, repo, prNumber)
		targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = &prNumber
		queueRecord.DedupeKey = fmt.Sprintf("reviewer:%s:%s:%s:%d", record.ProjectID, record.ID, repo, prNumber)
		queueRecord.Priority = storage.QueuePriorityReviewer
		queueRecord.LockKey = &lockKey
	case domain.LoopTypeFixer:
		repo := strings.TrimSpace(derefString(record.Repo))
		if repo == "" || record.PRNumber == nil {
			return storage.QueueItemRecord{}, false, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s loop requires repo and prNumber", record.Type)}
		}
		prNumber := *record.PRNumber
		lockKey := storage.PullRequestLockKey(record.ProjectID, repo, prNumber)
		targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = &prNumber
		queueRecord.DedupeKey = fmt.Sprintf("fixer:%s", record.ID)
		queueRecord.Priority = storage.QueuePriorityFixer
		queueRecord.LockKey = &lockKey
	case domain.LoopTypeWorker:
		payloadJSON := buildWorkerQueuePayloadJSONCompat(metadataJSON)
		if payloadJSON != nil {
			queueRecord.PayloadJSON = payloadJSON
		}
		queueRecord.Priority = storage.QueuePriorityWorker
		lockKey := fmt.Sprintf("worker:%s", record.ID)
		queueRecord.DedupeKey = fmt.Sprintf("worker:%s", record.ID)
		if target.TargetType == domain.LoopTargetTypeIssue {
			repo := strings.TrimSpace(derefString(record.Repo))
			issueNumber := target.IssueNumber
			if repo == "" || issueNumber <= 0 {
				return storage.QueueItemRecord{}, false, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s loop requires repo and issueNumber", record.Type)}
			}
			lockKey = storage.IssueLockKey(record.ProjectID, repo, issueNumber)
			targetID := fmt.Sprintf("issue:%s:%d", repo, issueNumber)
			queueRecord.TargetType = string(domain.LoopTargetTypeIssue)
			queueRecord.TargetID = targetID
			queueRecord.Repo = &repo
			queueRecord.PRNumber = nil
			queueRecord.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", record.ProjectID, repo, issueNumber)
		} else if target.TargetType == domain.LoopTargetTypePullRequest {
			repo := strings.TrimSpace(derefString(record.Repo))
			if repo == "" || record.PRNumber == nil {
				return storage.QueueItemRecord{}, false, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s loop requires repo and prNumber", record.Type)}
			}
			prNumber := *record.PRNumber
			lockKey = storage.PullRequestLockKey(record.ProjectID, repo, prNumber)
			targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
			queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
			queueRecord.TargetID = targetID
			queueRecord.Repo = &repo
			queueRecord.PRNumber = &prNumber
			queueRecord.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", record.ProjectID, repo, prNumber)
		}
		queueRecord.LockKey = &lockKey
	}

	return queueRecord, true, nil
}

func buildWorkerQueuePayloadJSONCompat(metadataJSON *string) *string {
	metadata := parseJSONObject(metadataJSON)
	workerMeta, ok := metadata["worker"].(map[string]any)
	if !ok || len(workerMeta) == 0 {
		return nil
	}
	encoded, err := json.Marshal(workerMeta)
	if err != nil {
		return nil
	}
	text := string(encoded)
	return &text
}

func loopTargetIDCompat(target domain.LoopTarget) *string {
	value := loopTargetKeyCompat(target)
	return &value
}

func repoFromTargetCompat(target domain.LoopTarget) *string {
	if target.TargetType == domain.LoopTargetTypeProject {
		return nil
	}
	value := target.Repo
	return &value
}

func prNumberFromTargetCompat(target domain.LoopTarget) *int64 {
	if target.TargetType != domain.LoopTargetTypePullRequest {
		return nil
	}
	value := target.PRNumber
	return &value
}

func loopTargetKeyCompat(target domain.LoopTarget) string {
	switch target.TargetType {
	case domain.LoopTargetTypeProject:
		return "project:" + normalizeProjectTargetID(target.ProjectID)
	case domain.LoopTargetTypeIssue:
		return fmt.Sprintf("issue:%s:%d", target.Repo, target.IssueNumber)
	default:
		return fmt.Sprintf("pull_request:%s:%d", target.Repo, target.PRNumber)
	}
}

func assertUniqueActiveLoopCompat(existing []storage.LoopRecord, candidateID, projectID string, loopType domain.LoopType, target domain.LoopTarget, status domain.LoopStatus) error {
	if !domain.IsConflictingActiveLoopStatus(status) {
		return nil
	}

	for _, loop := range existing {
		if loop.ID == candidateID || !domain.IsConflictingActiveLoopStatus(domain.LoopStatus(loop.Status)) {
			continue
		}

		allowConcurrentProjectWorkers := loop.ProjectID == projectID &&
			loop.Type == string(domain.LoopTypeWorker) &&
			loopType == domain.LoopTypeWorker &&
			loop.TargetType == string(domain.LoopTargetTypeProject) &&
			target.TargetType == domain.LoopTargetTypeProject
		if allowConcurrentProjectWorkers {
			continue
		}

		if loop.ProjectID == projectID && loop.Type == string(loopType) && loopTargetKeyFromRecordCompat(loop) == loopTargetKeyCompat(target) {
			return apiError{code: pkgapi.ErrorCodeLoopConflict, status: http.StatusConflict, message: fmt.Sprintf("active loop already exists for %s:%s:%s", projectID, loopType, loopTargetKeyCompat(target))}
		}
	}

	return nil
}

func loopTargetKeyFromRecordCompat(loop storage.LoopRecord) string {
	switch loop.TargetType {
	case string(domain.LoopTargetTypeProject):
		if loop.TargetID == nil {
			return "project:"
		}
		return "project:" + normalizeProjectTargetID(*loop.TargetID)
	case string(domain.LoopTargetTypeIssue):
		if loop.TargetID == nil {
			return "issue:"
		}
		return *loop.TargetID
	default:
		if loop.Repo == nil || loop.PRNumber == nil {
			return "pull_request:"
		}
		return fmt.Sprintf("pull_request:%s:%d", *loop.Repo, *loop.PRNumber)
	}
}

func loopTargetFromRecordCompat(loop storage.LoopRecord) (domain.LoopTarget, error) {
	target := domain.LoopTarget{TargetType: domain.LoopTargetType(loop.TargetType)}
	switch target.TargetType {
	case domain.LoopTargetTypeProject:
		if loop.TargetID == nil {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: fmt.Sprintf("project loop %s has no target id", loop.ID)}
		}
		target.ProjectID = normalizeProjectTargetID(*loop.TargetID)
	case domain.LoopTargetTypeIssue:
		if loop.Repo == nil || loop.TargetID == nil {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: fmt.Sprintf("issue loop %s is missing target data", loop.ID)}
		}
		target.Repo = *loop.Repo
		index := strings.LastIndex(*loop.TargetID, ":")
		if index < 0 || index+1 >= len(*loop.TargetID) {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: fmt.Sprintf("issue loop %s has invalid target id %q", loop.ID, *loop.TargetID)}
		}
		if _, err := fmt.Sscanf((*loop.TargetID)[index+1:], "%d", &target.IssueNumber); err != nil {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: fmt.Sprintf("issue loop %s has invalid issue number: %v", loop.ID, err)}
		}
	default:
		if loop.Repo == nil || loop.PRNumber == nil {
			return domain.LoopTarget{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: fmt.Sprintf("pull request loop %s is missing target data", loop.ID)}
		}
		target.Repo = *loop.Repo
		target.PRNumber = *loop.PRNumber
	}
	return target, nil
}

func normalizeProjectTargetID(targetID string) string {
	normalized := strings.TrimSpace(targetID)
	for strings.HasPrefix(normalized, "project:") {
		normalized = strings.TrimPrefix(normalized, "project:")
	}
	return normalized
}

func parseIssueNumber(targetID string) (int64, error) {
	match := issueTargetPattern.FindStringSubmatch(strings.TrimSpace(targetID))
	if len(match) != 2 {
		return 0, fmt.Errorf("issue number not found")
	}
	return strconv.ParseInt(match[1], 10, 64)
}

func mapLoopCreateError(err error) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "project not found:"):
		return apiError{code: pkgapi.ErrorCodeProjectNotFound, status: http.StatusNotFound, message: strings.Replace(message, "project not found", "Project not found", 1)}
	case strings.Contains(message, "active loop already exists"):
		return apiError{code: pkgapi.ErrorCodeLoopConflict, status: http.StatusConflict, message: message}
	case strings.Contains(message, "must target") || strings.Contains(message, "must be one of:") || strings.Contains(message, "positive integer") || strings.Contains(message, "is required"):
		return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: message}
	default:
		return apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: message}
	}
}

const maxPersistedAgentLogReadBytes = 16 * 1024 * 1024

func parseAgentOutput(logDir string, outputJSON *string) (string, string) {
	if outputJSON == nil || strings.TrimSpace(*outputJSON) == "" {
		return "", ""
	}
	var payload struct {
		Stdout        string `json:"stdout"`
		Stderr        string `json:"stderr"`
		StdoutLogPath string `json:"stdoutLogPath"`
		StderrLogPath string `json:"stderrLogPath"`
	}
	if err := json.Unmarshal([]byte(*outputJSON), &payload); err != nil {
		return "", ""
	}
	if content, ok := readAgentOutputLog(logDir, payload.StdoutLogPath); ok {
		payload.Stdout = content
	}
	if content, ok := readAgentOutputLog(logDir, payload.StderrLogPath); ok {
		payload.Stderr = content
	}
	return payload.Stdout, payload.Stderr
}

func readAgentOutputLog(logDir string, path string) (string, bool) {
	if strings.TrimSpace(path) == "" {
		return "", false
	}
	if !isPathWithinDirectory(path, logDir) {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false
	}
	if info.Size() > maxPersistedAgentLogReadBytes {
		if _, err := file.Seek(info.Size()-maxPersistedAgentLogReadBytes, io.SeekStart); err != nil {
			return "", false
		}
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxPersistedAgentLogReadBytes))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func isPathWithinDirectory(path string, directory string) bool {
	if strings.TrimSpace(directory) == "" {
		return false
	}
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(filepath.Clean(directory))
	if err != nil {
		return false
	}
	if absPath == absDir {
		return false
	}
	return strings.HasPrefix(absPath, absDir+string(os.PathSeparator))
}

func parseJSONObject(raw *string) map[string]any {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return map[string]any{}
	}
	value := map[string]any{}
	if err := json.Unmarshal([]byte(*raw), &value); err != nil {
		return map[string]any{}
	}
	return value
}

func readObject(value map[string]any, key string) map[string]any {
	child, ok := value[key]
	if !ok {
		return map[string]any{}
	}
	typed, ok := child.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return typed
}

func readObjectString(value map[string]any, key string) *string {
	return readStringAny(value[key])
}

func readStringMap(value map[string]any, key string) *string {
	return readStringAny(value[key])
}

func readStringAny(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func firstNonEmptyString(values ...*string) *string {
	for _, value := range values {
		if value != nil && strings.TrimSpace(*value) != "" {
			trimmed := strings.TrimSpace(*value)
			return &trimmed
		}
	}
	return nil
}

func stringPtrOrNil(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func isCodingAgentConfigured(cfg config.Config) bool {
	return config.AnyCodingRoleAgentConfigured(cfg)
}

func isCodingRoleAgentConfigured(cfg config.Config, role string) bool {
	return config.CodingRoleAgentConfigured(cfg, role)
}

// assertCodingRoleRetryAgent allows sticky retries after vendor removal when the
// predecessor failed/interrupted run still carries a durable agent_snapshot_json.
// New loops without a live ResolveAgent identity remain blocked at create.
func (h *Handler) assertCodingRoleRetryAgent(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord) error {
	return h.assertCodingRoleResumeAgent(ctx, repos, loop, "retry")
}

// assertCodingRoleResumeAgent gates coding-role requeue (retry, /start, HITL
// deliverHumanAnswer → mutateLoopStatus Running). Live config.agent / roles.*.agent
// vendor is preferred; otherwise a failed/interrupted predecessor run with a
// parseable agent_snapshot_json vendor is enough for sticky continuation.
func (h *Handler) assertCodingRoleResumeAgent(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, action string) error {
	switch loop.Type {
	case string(domain.LoopTypeReviewer), string(domain.LoopTypeFixer), string(domain.LoopTypeWorker), string(domain.LoopTypePlanner):
	default:
		return nil
	}
	if isCodingRoleAgentConfigured(h.effectiveConfig(), loop.Type) {
		return nil
	}
	if repos != nil && repos.Runs != nil {
		latest, err := repos.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return err
		}
		if latest != nil && (latest.Status == string(domain.RunStatusFailed) || latest.Status == string(domain.RunStatusInterrupted)) {
			if latest.AgentSnapshotJSON != nil {
				snapshot, parseErr := config.ParseAgentSnapshot(*latest.AgentSnapshotJSON)
				if parseErr == nil && strings.TrimSpace(snapshot.Vendor) != "" {
					return nil
				}
			}
		}
	}
	if strings.TrimSpace(action) == "" {
		action = "start"
	}
	return apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot %s %s loop without config.agent.vendor", action, loop.Type)}
}

func urlPathSegment(parts []string, index int) (string, error) {
	if index >= len(parts) {
		return "", apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "loopId is required"}
	}
	segment := strings.TrimSpace(parts[index])
	if segment == "" {
		return "", apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "loopId is required"}
	}
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return "", apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "loopId is required"}
	}
	return strings.TrimSpace(decoded), nil
}

type createProjectRequest struct {
	RepoPath     *string `json:"repoPath"`
	ID           *string `json:"id"`
	Name         *string `json:"name"`
	BaseBranch   *string `json:"baseBranch"`
	WorktreeRoot *string `json:"worktreeRoot"`
	Repo         *string `json:"repo"`
	Provider     *string `json:"provider"`
	SnapshotMode *string `json:"snapshotMode"`
}

func (h *Handler) buildCreateProjectResponse(r *http.Request, service projectService) (createProjectResponse, error) {
	body := createProjectRequest{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "Request body must be valid JSON"}
	}

	repoPath := strings.TrimSpace(derefString(body.RepoPath))
	if repoPath == "" {
		return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repoPath is required"}
	}

	providedID := strings.TrimSpace(derefString(body.ID))
	idSource := "derived"
	projectID := providedID
	if projectID == "" {
		projectID = deriveProjectIDFromRepoPath(repoPath)
	} else {
		idSource = "explicit"
	}

	name := strings.TrimSpace(derefString(body.Name))
	if name == "" {
		name = projectID
	}

	baseBranch := strings.TrimSpace(derefString(body.BaseBranch))
	if baseBranch == "" {
		baseBranch = h.context.Config.Defaults.BaseBranch
	}
	snapshotMode := projects.SnapshotMode(strings.TrimSpace(derefString(body.SnapshotMode)))
	if snapshotMode == "" {
		snapshotMode = projects.SnapshotMode(h.context.Config.Defaults.AddSnapshotMode)
	}
	if snapshotMode != "" && snapshotMode != projects.SnapshotModeAsync && snapshotMode != projects.SnapshotModeFull && snapshotMode != projects.SnapshotModeOff {
		return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "snapshotMode must be one of: async, full, off"}
	}

	result, err := service.AddProject(r.Context(), projects.AddInput{
		ID:           projectID,
		Name:         name,
		RepoPath:     repoPath,
		BaseBranch:   baseBranch,
		IDSource:     idSource,
		WorktreeRoot: normalizeOptionalString(body.WorktreeRoot),
		Repo:         normalizeOptionalString(body.Repo),
		Provider:     normalizeOptionalString(body.Provider),
		SnapshotMode: snapshotMode,
	})
	if err != nil {
		var collision projects.ProjectIDCollisionError
		var validation projects.ProjectValidationError
		switch {
		case errors.As(err, &collision):
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeProjectIDConflict, status: http.StatusConflict, message: err.Error()}
		case errors.As(err, &validation):
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
		case strings.HasPrefix(err.Error(), "invalid project id"):
			message := strings.Replace(err.Error(), "invalid project id", "Invalid project id", 1)
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: message}
		default:
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	}
	return createProjectResponse{
		projectResponse:        serializeProject(result.Project, h.context.Config, h.context.Config.Defaults.BaseBranch),
		DiscoveredPullRequests: result.DiscoveredPullRequests,
		DiscoveredWorktrees:    result.DiscoveredWorktrees,
		PendingSnapshots:       result.PendingSnapshots,
		CapturedSnapshots:      result.CapturedSnapshots,
		Warnings:               append([]string{}, result.Warnings...),
	}, nil
}

func serializeProject(project storage.ProjectRecord, cfg config.Config, defaultBaseBranch string) projectResponse {
	metadata := parseProjectMetadata(project.MetadataJSON)

	baseBranch := defaultBaseBranch
	if project.BaseBranch != nil && strings.TrimSpace(*project.BaseBranch) != "" {
		baseBranch = *project.BaseBranch
	}

	return projectResponse{
		ID:           project.ID,
		Name:         project.Name,
		RepoPath:     project.RepoPath,
		BaseBranch:   baseBranch,
		Archived:     project.Archived,
		Provider:     resolveProjectProviderKind(cfg, project.ID, metadata),
		Repo:         stringMetadataPtr(metadata, "repo"),
		WorktreeRoot: stringMetadataPtr(metadata, "worktreeRoot"),
		CreatedAt:    project.CreatedAt,
		UpdatedAt:    project.UpdatedAt,
	}
}

// resolveProjectProviderKind returns the display provider kind for a project:
// config binding first, then API metadata provider id, else github.
func resolveProjectProviderKind(cfg config.Config, projectID string, metadata map[string]any) string {
	for _, configured := range cfg.Projects {
		if configured.ID != projectID {
			continue
		}
		kind := config.ResolvedProjectProviderKind(cfg, configured)
		if kind != "" {
			return string(kind)
		}
		break
	}
	if providerID := strings.TrimSpace(stringMetadataValue(metadata, "provider")); providerID != "" {
		for _, provider := range cfg.Providers {
			if provider.ID == providerID && provider.Kind != "" {
				return string(provider.Kind)
			}
		}
	}
	return string(config.ProviderKindGitHub)
}

func stringMetadataValue(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return value
}

func requireActiveProjectRecord(ctx context.Context, repo *storage.ProjectsRepository, projectID string) (*storage.ProjectRecord, error) {
	project, err := repo.GetByID(ctx, projectID)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if project == nil || project.Archived {
		return nil, apiError{code: pkgapi.ErrorCodeProjectNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Project not found: %s", projectID)}
	}
	return project, nil
}

func parseProjectMetadata(metadataJSON *string) map[string]any {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return map[string]any{}
	}

	metadata := map[string]any{}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return map[string]any{}
	}

	return metadata
}

func validateLoopTargetProjectCompatibility(projectID string, projectMetadata map[string]any, target domain.LoopTarget) error {
	configuredRepo := strings.TrimSpace(derefString(stringMetadataPtr(projectMetadata, "repo")))
	if configuredRepo == "" {
		return nil
	}

	targetRepo := strings.TrimSpace(target.Repo)
	if targetRepo == "" || configuredRepo == targetRepo {
		return nil
	}

	if target.TargetType == domain.LoopTargetTypePullRequest && target.PRNumber > 0 {
		return apiError{code: pkgapi.ErrorCodePullRequestProjectMismatch, status: http.StatusConflict, message: fmt.Sprintf("Pull request %s#%d does not belong to project %s", targetRepo, target.PRNumber, projectID)}
	}

	return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("project %s is configured for repo %s, not %s", projectID, configuredRepo, targetRepo)}
}

func stringMetadataPtr(metadata map[string]any, key string) *string {
	value, ok := metadata[key]
	if !ok {
		return nil
	}

	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil
	}

	result := text
	return &result
}

func stringFromAnyDefault(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func normalizeOptionalString(value *string) *string {
	if value == nil {
		return nil
	}

	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}

	return &trimmed
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func deriveProjectIDFromRepoPath(repoPath string) string {
	segments := strings.FieldsFunc(repoPath, func(r rune) bool { return r == '/' || r == '\\' })
	lastSegment := "project"
	if len(segments) > 0 {
		lastSegment = segments[len(segments)-1]
	}
	normalized := strings.Trim(nonProjectIDPattern.ReplaceAllString(strings.ToLower(lastSegment), "-"), "-")
	if normalized == "" {
		return "project"
	}
	return normalized
}

func looperdArtifactName(target string) *string {
	supported := map[string]struct{}{
		"darwin-arm64": {},
		"linux-amd64":  {},
	}

	if _, ok := supported[target]; !ok {
		return nil
	}

	value := "looperd-" + target
	return &value
}

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/config"
	looperdruntime "github.com/powerformer/looper/internal/runtime"
	"github.com/powerformer/looper/internal/storage"
	"github.com/powerformer/looper/internal/version"
	pkgapi "github.com/powerformer/looper/pkg/api"
)

const (
	requestIDHeaderName = "x-request-id"
	apiBasePath         = "/api/v1"
	javaScriptISOString = "2006-01-02T15:04:05.000Z"
)

type RuntimeState interface {
	Services() looperdruntime.Services
	StartedAt() (time.Time, bool)
}

type Context struct {
	Config          config.Config
	Runtime         RuntimeState
	Now             func() time.Time
	RecoverySummary func() any
}

type Handler struct {
	context         Context
	now             func() time.Time
	recoverySummary func() any
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

	return &Handler{
		context:         context,
		now:             now,
		recoverySummary: recoverySummary,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := strings.TrimSpace(r.Header.Get(requestIDHeaderName))
	if requestID == "" {
		requestID = generateRequestID()
	}

	if err := authorizeRequest(r, h.context.Config); err != nil {
		var typed apiError
		if !asAPIError(err, &typed) {
			typed = internalServerError(err)
		}
		h.writeError(w, requestID, typed)
		return
	}

	path := normalizePath(r.URL.Path)

	switch path {
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
	}

	h.writeError(w, requestID, apiError{
		code:    pkgapi.ErrorCodeRouteNotFound,
		status:  http.StatusNotFound,
		message: fmt.Sprintf("Unknown route: %s", path),
	})
}

type apiError struct {
	code    pkgapi.ErrorCode
	status  int
	message string
	details any
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

func authorizeRequest(r *http.Request, cfg config.Config) error {
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

	if r.Header.Get("Authorization") != fmt.Sprintf("Bearer %s", *cfg.Server.LocalToken) {
		return apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusUnauthorized,
			message: "Authorization token is required",
		}
	}

	return nil
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
	Service       statusService       `json:"service"`
	Storage       statusStorage       `json:"storage"`
	Scheduler     statusScheduler     `json:"scheduler"`
	Loops         statusLoops         `json:"loops"`
	Safety        statusSafety        `json:"safety"`
	Notifications statusNotifications `json:"notifications"`
	Tools         statusTools         `json:"tools"`
}

type statusService struct {
	Healthy    bool                  `json:"healthy"`
	Version    string                `json:"version"`
	Build      version.BuildMetadata `json:"build"`
	DaemonMode config.DaemonMode     `json:"daemonMode"`
	StartedAt  *string               `json:"startedAt,omitempty"`
	Recovery   any                   `json:"recovery"`
	Binary     statusBinary          `json:"binary"`
}

type statusBinary struct {
	Name             string   `json:"name"`
	InstallDir       string   `json:"installDir"`
	CurrentTarget    string   `json:"currentTarget"`
	ArtifactName     *string  `json:"artifactName"`
	SupportedTargets []string `json:"supportedTargets"`
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

type statusLoopType struct {
	Running int `json:"running"`
	Paused  int `json:"paused"`
	Failed  int `json:"failed"`
}

type statusLoops struct {
	Planner  statusLoopType `json:"planner"`
	Reviewer statusLoopType `json:"reviewer"`
	Worker   statusLoopType `json:"worker"`
	Fixer    statusLoopType `json:"fixer"`
}

type statusSafety struct {
	AllowAutoCommit  bool                  `json:"allowAutoCommit"`
	AllowAutoPush    bool                  `json:"allowAutoPush"`
	AllowAutoApprove bool                  `json:"allowAutoApprove"`
	AllowRiskyFixes  bool                  `json:"allowRiskyFixes"`
	OpenPRStrategy   config.OpenPRStrategy `json:"openPrStrategy"`
}

type statusNotifications struct {
	InAppEnabled     bool `json:"inAppEnabled"`
	OsascriptEnabled bool `json:"osascriptEnabled"`
}

type statusTools struct {
	Bun       bool `json:"bun"`
	Git       bool `json:"git"`
	GH        bool `json:"gh"`
	Osascript bool `json:"osascript"`
}

func (h *Handler) buildStatusResponse(ctx context.Context) (statusResponse, error) {
	storageState, err := h.loadStorageState(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	services := h.context.Runtime.Services()
	loops, err := services.Repositories.Loops.List(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	runs, err := services.Repositories.Runs.List(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	queueItems, err := services.Repositories.Queue.List(ctx)
	if err != nil {
		return statusResponse{}, err
	}

	loopCounts := countLoops(loops)
	queueCounts := countQueueByStatus(queueItems)
	runCounts := countRunsByStatus(runs)

	currentTarget := currentLooperdTarget()
	installDir := filepath.Join(homeDirOrEmpty(), ".looper", "bin")
	artifactName := looperdArtifactName(currentTarget)

	return statusResponse{
		Service: statusService{
			Healthy:    storageState.OK,
			Version:    version.Current().Version,
			Build:      version.Current().Metadata,
			DaemonMode: h.context.Config.Daemon.Mode,
			StartedAt:  h.startedAtISO(),
			Recovery:   h.recoverySummary(),
			Binary: statusBinary{
				Name:             "looperd",
				InstallDir:       installDir,
				CurrentTarget:    currentTarget,
				ArtifactName:     artifactName,
				SupportedTargets: []string{"darwin-arm64", "darwin-x64"},
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
			QueuedItems:    queueCounts["queued"],
			RunningItems:   queueCounts["running"],
			CompletedItems: queueCounts["completed"],
			FailedItems:    queueCounts["failed"],
			TotalRuns:      len(runs),
			ActiveRuns:     runCounts["running"],
		},
		Loops: loopCounts,
		Safety: statusSafety{
			AllowAutoCommit:  h.context.Config.Defaults.AllowAutoCommit,
			AllowAutoPush:    h.context.Config.Defaults.AllowAutoPush,
			AllowAutoApprove: h.context.Config.Defaults.AllowAutoApprove,
			AllowRiskyFixes:  h.context.Config.Defaults.AllowRiskyFixes,
			OpenPRStrategy:   h.context.Config.Defaults.OpenPRStrategy,
		},
		Notifications: statusNotifications{
			InAppEnabled:     h.context.Config.Notifications.InApp,
			OsascriptEnabled: h.context.Config.Notifications.Osascript.Enabled,
		},
		Tools: statusTools{
			Bun:       hasValue(h.context.Config.Tools.BunPath),
			Git:       hasValue(h.context.Config.Tools.GitPath),
			GH:        hasValue(h.context.Config.Tools.GHPath),
			Osascript: hasValue(h.context.Config.Tools.OsascriptPath),
		},
	}, nil
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

func countLoops(loops []storage.LoopRecord) statusLoops {
	counts := statusLoops{}
	for _, loop := range loops {
		var target *statusLoopType
		switch loop.Type {
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

		switch loop.Status {
		case "running":
			target.Running++
		case "paused":
			target.Paused++
		case "failed":
			target.Failed++
		}
	}

	return counts
}

func countQueueByStatus(items []storage.QueueItemRecord) map[string]int {
	counts := map[string]int{}
	for _, item := range items {
		counts[item.Status]++
	}

	return counts
}

func countRunsByStatus(items []storage.RunRecord) map[string]int {
	counts := map[string]int{}
	for _, item := range items {
		counts[item.Status]++
	}

	return counts
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
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	}

	return fmt.Sprintf("%s-%s", runtime.GOOS, arch)
}

func normalizeRecoverySummary(summary looperdruntime.RecoverySummary) map[string]any {
	normalized := map[string]any{}
	if summary.StartedAt != "" {
		normalized["startedAt"] = summary.StartedAt
	}
	if summary.CompletedAt != "" {
		normalized["completedAt"] = summary.CompletedAt
	}
	if summary.OrphanAgentCleanup.Attempted || summary.OrphanAgentCleanup.CleanedCount != 0 || summary.OrphanAgentCleanup.Warning != "" {
		orphan := map[string]any{
			"attempted":    summary.OrphanAgentCleanup.Attempted,
			"cleanedCount": summary.OrphanAgentCleanup.CleanedCount,
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

func looperdArtifactName(target string) *string {
	supported := map[string]struct{}{
		"darwin-arm64": {},
		"darwin-x64":   {},
	}

	if _, ok := supported[target]; !ok {
		return nil
	}

	value := "looperd-" + target
	return &value
}

package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/domain"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/projects"
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

var nonProjectIDPattern = regexp.MustCompile(`[^a-z0-9]+`)

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
	case apiBasePath + "/config":
		if !assertMethod(r.Method, http.MethodGet, path, w, requestID, h.writeError) {
			return
		}

		h.writeSuccess(w, requestID, h.buildConfigResponse())
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
	}

	if strings.HasPrefix(path, apiBasePath+"/loops/") {
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

type configResponse struct {
	Server        configServerResponse      `json:"server"`
	Storage       config.StorageConfig      `json:"storage"`
	Scheduler     config.SchedulerConfig    `json:"scheduler"`
	Agent         config.AgentConfig        `json:"agent"`
	Logging       config.LoggingConfig      `json:"logging"`
	Notifications config.NotificationConfig `json:"notifications"`
	Tools         config.ToolPathsConfig    `json:"tools"`
	Daemon        configDaemonResponse      `json:"daemon"`
	Package       config.PackageConfig      `json:"package"`
	Defaults      config.DefaultsConfig     `json:"defaults"`
	Projects      []config.ProjectRefConfig `json:"projects"`
}

type configServerResponse struct {
	Host                 string          `json:"host"`
	Port                 int             `json:"port"`
	BaseURL              *string         `json:"baseUrl,omitempty"`
	AuthMode             config.AuthMode `json:"authMode"`
	LocalTokenConfigured bool            `json:"localTokenConfigured"`
}

type configDaemonResponse struct {
	Mode             config.DaemonMode `json:"mode"`
	PlistPath        *string           `json:"plistPath,omitempty"`
	LogDir           string            `json:"logDir"`
	WorkingDirectory string            `json:"workingDirectory"`
	Environment      map[string]string `json:"environment"`
}

func (h *Handler) buildConfigResponse() configResponse {
	cfg := h.context.Config

	return configResponse{
		Server: configServerResponse{
			Host:                 cfg.Server.Host,
			Port:                 cfg.Server.Port,
			BaseURL:              cfg.Server.BaseURL,
			AuthMode:             cfg.Server.AuthMode,
			LocalTokenConfigured: cfg.Server.LocalToken != nil && *cfg.Server.LocalToken != "",
		},
		Storage:       cfg.Storage,
		Scheduler:     cfg.Scheduler,
		Agent:         cfg.Agent,
		Logging:       cfg.Logging,
		Notifications: cfg.Notifications,
		Tools:         cfg.Tools,
		Daemon: configDaemonResponse{
			Mode:             cfg.Daemon.Mode,
			PlistPath:        cfg.Daemon.PlistPath,
			LogDir:           cfg.Daemon.LogDir,
			WorkingDirectory: cfg.Daemon.WorkingDirectory,
			Environment:      cfg.Daemon.Environment,
		},
		Package:  cfg.Package,
		Defaults: cfg.Defaults,
		Projects: append([]config.ProjectRefConfig{}, cfg.Projects...),
	}
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

type projectsListResponse struct {
	Items []projectResponse `json:"items"`
}

type loopsListResponse struct {
	Items []loopResponse `json:"items"`
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
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
}

type projectResponse struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	RepoPath     string  `json:"repoPath"`
	BaseBranch   string  `json:"baseBranch"`
	Archived     bool    `json:"archived"`
	Repo         *string `json:"repo"`
	WorktreeRoot *string `json:"worktreeRoot"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
}

type createProjectResponse struct {
	projectResponse
	DiscoveredPullRequests int      `json:"discoveredPullRequests"`
	DiscoveredWorktrees    int      `json:"discoveredWorktrees"`
	Warnings               []string `json:"warnings"`
}

type projectService interface {
	List(context.Context) ([]storage.ProjectRecord, error)
	AddProject(context.Context, projects.AddInput) (projects.AddResult, error)
}

func (h *Handler) buildProjectsRouteResponse(r *http.Request) (any, error) {
	services := h.context.Runtime.Services()
	if services.Projects == nil {
		return nil, apiError{
			code:    pkgapi.ErrorCodeProjectsUnavailable,
			status:  http.StatusInternalServerError,
			message: "Project management is not available in this runtime",
		}
	}

	switch r.Method {
	case http.MethodGet:
		items, err := services.Projects.List(r.Context())
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}

		responseItems := make([]projectResponse, 0, len(items))
		for _, item := range items {
			responseItems = append(responseItems, serializeProject(item, h.context.Config.Defaults.BaseBranch))
		}
		return projectsListResponse{Items: responseItems}, nil
	case http.MethodPost:
		return h.buildCreateProjectResponse(r, services.Projects)
	default:
		return nil, apiError{
			code:    pkgapi.ErrorCodeMethodNotAllowed,
			status:  http.StatusMethodNotAllowed,
			message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/projects"),
		}
	}
}

func (h *Handler) buildLoopsRouteResponse(r *http.Request) (any, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.Loops == nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Loops repository is not configured"}
	}

	switch r.Method {
	case http.MethodGet:
		items, err := services.Repositories.Loops.List(r.Context())
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}

		responseItems := make([]loopResponse, 0, len(items))
		for _, item := range items {
			responseItems = append(responseItems, serializeLoop(item))
		}

		return loopsListResponse{Items: responseItems}, nil
	case http.MethodPost:
		return h.buildCreateLoopResponse(r)
	default:
		return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/loops")}
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
		return serializeLoop(loop), nil
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
	default:
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}
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
	Metadata    json.RawMessage `json:"metadata"`
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

	targetType := strings.TrimSpace(derefString(body.TargetType))
	if targetType == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "targetType is required"}
	}

	status := strings.TrimSpace(derefString(body.Status))
	if status == "" {
		status = string(domain.LoopStatusRunning)
	}

	if (loopType == string(domain.LoopTypeReviewer) || loopType == string(domain.LoopTypeFixer)) && !isCodingAgentConfigured(h.context.Config) {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot create %s loop without config.agent.vendor", loopType)}
	}

	target, err := buildLoopTarget(targetType, body)
	if err != nil {
		return loopResponse{}, err
	}

	metadataJSON, err := normalizeMetadataJSON(body.Metadata)
	if err != nil {
		return loopResponse{}, err
	}

	now := h.now().UTC()
	record, err := storage.WithTransactionValue(r.Context(), services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		transactionRepos := storage.NewRepositories(tx)
		project, err := transactionRepos.Projects.GetByID(r.Context(), projectID)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if project == nil {
			return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeProjectNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Project not found: %s", projectID)}
		}

		existing, err := transactionRepos.Loops.List(r.Context())
		if err != nil {
			return storage.LoopRecord{}, err
		}
		candidateStatus := domain.LoopStatus(status)
		if err := assertUniqueActiveLoopCompat(existing, projectID, domain.LoopType(loopType), target, candidateStatus); err != nil {
			return storage.LoopRecord{}, err
		}

		seq, err := transactionRepos.Loops.AllocateSeq(r.Context())
		if err != nil {
			return storage.LoopRecord{}, err
		}

		nowISO := eventlog.FormatJavaScriptISOString(now)
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
		if candidateStatus == domain.LoopStatusRunning {
			record.NextRunAt = &nowISO
		}

		if err := transactionRepos.Loops.Upsert(r.Context(), record); err != nil {
			return storage.LoopRecord{}, err
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

	return serializeLoop(record), nil
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
	services := h.context.Runtime.Services()
	loop, err := services.Repositories.Loops.GetByID(ctx, loopID)
	if err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if loop == nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
	}

	if status == domain.LoopStatusRunning && (loop.Type == string(domain.LoopTypeReviewer) || loop.Type == string(domain.LoopTypeFixer)) && !isCodingAgentConfigured(h.context.Config) {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot start %s loop without config.agent.vendor", loop.Type)}
	}

	updated := *loop
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	updated.Status = string(status)
	updated.UpdatedAt = nowISO
	if status == domain.LoopStatusRunning {
		updated.NextRunAt = &nowISO
	}

	if err := services.Repositories.Loops.Upsert(ctx, updated); err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	return serializeLoop(updated), nil
}

func (h *Handler) buildLoopLogsResponse(ctx context.Context, loop storage.LoopRecord) (loopLogsResponse, error) {
	services := h.context.Runtime.Services()
	latestRun, err := services.Repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	var runPayload *loopLogsRunResponse
	var agentPayload *loopLogsAgentPayload
	if latestRun != nil {
		runPayload = &loopLogsRunResponse{
			RunID:        latestRun.ID,
			Status:       latestRun.Status,
			CurrentStep:  latestRun.CurrentStep,
			StartedAt:    latestRun.StartedAt,
			EndedAt:      latestRun.EndedAt,
			Summary:      latestRun.Summary,
			ErrorMessage: latestRun.ErrorMessage,
		}

		latestAgent, agentErr := services.Repositories.AgentExecutions.GetLatestByRunID(ctx, latestRun.ID)
		if agentErr != nil {
			return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: agentErr.Error()}
		}
		if latestAgent != nil {
			stdout, stderr := parseAgentOutput(latestAgent.OutputJSON)
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

func buildLoopTarget(targetType string, body createLoopRequest) (domain.LoopTarget, error) {
	switch targetType {
	case string(domain.LoopTargetTypeProject):
		targetID := strings.TrimSpace(derefString(body.TargetID))
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
		return "project:" + target.ProjectID
	case domain.LoopTargetTypeIssue:
		return fmt.Sprintf("issue:%s:%d", target.Repo, target.IssueNumber)
	default:
		return fmt.Sprintf("pull_request:%s:%d", target.Repo, target.PRNumber)
	}
}

func assertUniqueActiveLoopCompat(existing []storage.LoopRecord, projectID string, loopType domain.LoopType, target domain.LoopTarget, status domain.LoopStatus) error {
	if !domain.IsActiveLoopStatus(status) {
		return nil
	}

	for _, loop := range existing {
		if !domain.IsActiveLoopStatus(domain.LoopStatus(loop.Status)) {
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
		if strings.HasPrefix(*loop.TargetID, "project:") {
			return *loop.TargetID
		}
		return "project:" + *loop.TargetID
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

func parseIssueNumber(targetID string) (int64, error) {
	index := strings.LastIndex(targetID, ":")
	if index < 0 || index+1 >= len(targetID) {
		return 0, fmt.Errorf("issue number not found")
	}
	return strconv.ParseInt(targetID[index+1:], 10, 64)
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

func parseAgentOutput(outputJSON *string) (string, string) {
	if outputJSON == nil || strings.TrimSpace(*outputJSON) == "" {
		return "", ""
	}
	var payload struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	if err := json.Unmarshal([]byte(*outputJSON), &payload); err != nil {
		return "", ""
	}
	return payload.Stdout, payload.Stderr
}

func isCodingAgentConfigured(cfg config.Config) bool {
	return cfg.Agent.Vendor != nil && strings.TrimSpace(string(*cfg.Agent.Vendor)) != ""
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

	result, err := service.AddProject(r.Context(), projects.AddInput{
		ID:           projectID,
		Name:         name,
		RepoPath:     repoPath,
		BaseBranch:   baseBranch,
		IDSource:     idSource,
		WorktreeRoot: normalizeOptionalString(body.WorktreeRoot),
		Repo:         normalizeOptionalString(body.Repo),
	})
	if err != nil {
		var collision projects.ProjectIDCollisionError
		switch {
		case errors.As(err, &collision):
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeProjectIDConflict, status: http.StatusConflict, message: err.Error()}
		case strings.HasPrefix(err.Error(), "invalid project id"):
			message := strings.Replace(err.Error(), "invalid project id", "Invalid project id", 1)
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: message}
		default:
			return createProjectResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
	}

	return createProjectResponse{
		projectResponse:        serializeProject(result.Project, h.context.Config.Defaults.BaseBranch),
		DiscoveredPullRequests: result.DiscoveredPullRequests,
		DiscoveredWorktrees:    result.DiscoveredWorktrees,
		Warnings:               append([]string{}, result.Warnings...),
	}, nil
}

func serializeProject(project storage.ProjectRecord, defaultBaseBranch string) projectResponse {
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
		Repo:         stringMetadataPtr(metadata, "repo"),
		WorktreeRoot: stringMetadataPtr(metadata, "worktreeRoot"),
		CreatedAt:    project.CreatedAt,
		UpdatedAt:    project.UpdatedAt,
	}
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
		"darwin-x64":   {},
	}

	if _, ok := supported[target]; !ok {
		return nil
	}

	value := "looperd-" + target
	return &value
}

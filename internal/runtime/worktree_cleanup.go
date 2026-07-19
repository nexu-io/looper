package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreecleanup"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

type WorktreeCleanupStatus struct {
	Enabled         bool    `json:"enabled"`
	DryRun          bool    `json:"dryRun"`
	LastStartedAt   *string `json:"lastStartedAt,omitempty"`
	LastCompletedAt *string `json:"lastCompletedAt,omitempty"`
	LastStatus      string  `json:"lastStatus"`
	Scanned         int     `json:"scanned"`
	Candidates      int     `json:"candidates"`
	Cleaned         int     `json:"cleaned"`
	Skipped         int     `json:"skipped"`
	Failed          int     `json:"failed"`
	LastError       string  `json:"lastError,omitempty"`
}

func (r *Runtime) WorktreeCleanupStatus() WorktreeCleanupStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status := r.worktreeCleanupStatusLocked()
	return status
}

func (r *Runtime) worktreeCleanupStatusLocked() WorktreeCleanupStatus {
	status := r.worktreeCleanupStatus
	status.Enabled = r.config.Daemon.WorktreeCleanup.Enabled
	status.DryRun = r.config.Daemon.WorktreeCleanup.DryRun
	if status.LastStatus == "" {
		status.LastStatus = "idle"
	}
	return status
}

func (r *Runtime) startWorktreeCleanupLoop() {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	initialDelay := r.worktreeCleanupInitialDelay
	if initialDelay == 0 {
		initialDelay = time.Minute
	}
	interval, err := time.ParseDuration(r.config.Daemon.WorktreeCleanup.Interval)
	if err != nil || interval <= 0 {
		interval = time.Hour
	}

	r.mu.Lock()
	if r.worktreeCleanupStop != nil {
		r.mu.Unlock()
		cancel()
		return
	}
	r.worktreeCleanupStop = stopCh
	r.worktreeCleanupDone = doneCh
	r.worktreeCleanupCancel = cancel
	r.mu.Unlock()

	go func() {
		defer close(doneCh)
		if initialDelay > 0 {
			timer := time.NewTimer(initialDelay)
			select {
			case <-stopCh:
				timer.Stop()
				return
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		r.executeWorktreeCleanupPass(ctx)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.executeWorktreeCleanupPass(ctx)
			}
		}
	}()
}

func (r *Runtime) stopWorktreeCleanupLoop() {
	r.mu.Lock()
	stopCh := r.worktreeCleanupStop
	doneCh := r.worktreeCleanupDone
	cancel := r.worktreeCleanupCancel
	r.worktreeCleanupStop = nil
	r.worktreeCleanupDone = nil
	r.worktreeCleanupCancel = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stopCh == nil || doneCh == nil {
		return
	}
	close(stopCh)
	timer := time.NewTimer(r.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-doneCh:
	case <-timer.C:
		if r.logger != nil {
			r.logger.Warn("looperd stop timed out waiting for worktree cleanup loop", map[string]any{"timeoutMs": r.shutdownTimeout.Milliseconds()})
		}
	}
}

func (r *Runtime) executeWorktreeCleanupPass(ctx context.Context) {
	// Cleanup deletes managed worktrees and touches durable records — treat it
	// as a mutation surface under the same admission Authority as claims
	// (#580). Loop starts before MarkReady; without this gate a first pass can
	// run while starting, or continue while degraded/stopping.
	if err := r.AllowClaim(); err != nil {
		return
	}
	r.mu.Lock()
	if r.worktreeCleanupRunning {
		r.mu.Unlock()
		return
	}
	r.worktreeCleanupRunning = true
	services := r.services
	r.mu.Unlock()
	cfg := r.Config()

	defer func() {
		r.mu.Lock()
		r.worktreeCleanupRunning = false
		r.mu.Unlock()
	}()

	if services.Repositories == nil {
		return
	}
	gitGateway := gitinfra.New(gitinfra.Options{GitPath: derefString(cfg.Tools.GitPath), Repos: services.Repositories, Now: r.now})
	summary := r.runWorktreeCleanupPass(ctx, services.Repositories, gitGateway, cfg)
	r.mu.Lock()
	r.worktreeCleanupStatus = summary
	r.mu.Unlock()
}

type worktreeCleanupGit interface {
	ListWorktrees(context.Context, string) ([]gitinfra.WorktreeListEntry, error)
	WorktreeClean(context.Context, string) (bool, error)
	CleanupWorktree(context.Context, gitinfra.CleanupWorktreeInput) error
}

func (r *Runtime) runWorktreeCleanupPass(ctx context.Context, repos *storage.Repositories, gitGateway worktreeCleanupGit, cfg config.Config) WorktreeCleanupStatus {
	startedAt := formatJavaScriptISOString(r.now().UTC())
	summary := WorktreeCleanupStatus{
		Enabled:       cfg.Daemon.WorktreeCleanup.Enabled,
		DryRun:        cfg.Daemon.WorktreeCleanup.DryRun,
		LastStartedAt: stringPtr(startedAt),
		LastStatus:    "running",
	}
	// Gate the start event with a point-in-time AllowClaim, then append outside
	// admission.mu. Holding WithAllowClaim across the SQLite write stalls
	// MarkDegraded/BeginShutdown (they need that mutex to cancel producers), so
	// the cancellation that would unblock a busy ExecContext cannot run.
	// Already-closed admission still omits durable events; a concurrent close
	// during the write is accepted for telemetry rather than deadlocking drain.
	if err := r.AllowClaim(); err != nil {
		summary.LastError = err.Error()
		summary.LastCompletedAt = stringPtr(formatJavaScriptISOString(r.now().UTC()))
		summary.LastStatus = "completed"
		// No durable cleanup events after admission closed — leave only the
		// in-memory summary for status surfaces.
		return summary
	}
	_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.started", nil, map[string]any{
		"dryRun":     summary.DryRun,
		"maxPerTick": worktreeCleanupMaxPerTick(cfg),
		"startedAt":  startedAt,
	})

	plan, err := (&worktreecleanup.Service{
		Repos:  repos,
		Config: cfg.Daemon.WorktreeCleanup,
		Now:    r.now,
	}).Plan(ctx)
	if err != nil {
		summary.Failed = 1
		summary.LastStatus = "failed"
		summary.LastError = err.Error()
		summary.LastCompletedAt = stringPtr(formatJavaScriptISOString(r.now().UTC()))
		// MarkDegraded cancels the cleanup context during Plan; that surfaces as
		// a Plan error after admission is already closed. Hold WithAllowClaim so
		// terminal failed/completed events cannot append after closure.
		_ = r.WithAllowClaim(func() {
			_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.failed", nil, map[string]any{"message": err.Error()})
			_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.completed", nil, map[string]any{"status": summary.LastStatus, "failed": summary.Failed, "lastError": summary.LastError})
		})
		return summary
	}
	summary.Scanned = plan.Summary.Scanned
	summary.Candidates = plan.Summary.Candidates

	// Recheck after planning: MarkDegraded can close admission while Plan runs,
	// and a pass that already started must not continue into candidate mutations
	// or emit durable cleanup events while closed.
	if err := r.AllowClaim(); err != nil {
		summary.LastError = err.Error()
		summary.LastCompletedAt = stringPtr(formatJavaScriptISOString(r.now().UTC()))
		summary.LastStatus = "completed"
		// No worktree/record mutations and no cleanup DB events after admission
		// closed — leave only the in-memory summary for status surfaces.
		return summary
	}
	for _, decision := range plan.Decisions {
		if ctx.Err() != nil {
			summary.LastError = ctx.Err().Error()
			// cancelWorkProducers (via MarkDegraded/BeginShutdown) cancels ctx
			// after admission is already closed. Do not fall through to a
			// terminal completed event without rechecking admission.
			summary.LastCompletedAt = stringPtr(formatJavaScriptISOString(r.now().UTC()))
			if summary.Failed > 0 {
				summary.LastStatus = "failed"
			} else {
				summary.LastStatus = "completed"
			}
			_ = r.WithAllowClaim(func() {
				_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.completed", nil, map[string]any{
					"status":      summary.LastStatus,
					"scanned":     summary.Scanned,
					"candidates":  summary.Candidates,
					"cleaned":     summary.Cleaned,
					"skipped":     summary.Skipped,
					"failed":      summary.Failed,
					"lastError":   summary.LastError,
					"completedAt": derefString(summary.LastCompletedAt),
				})
			})
			return summary
		}
		// Recheck before each candidate so degradation mid-pass cancels remaining
		// worktree/record mutations instead of finishing the planned batch.
		if err := r.AllowClaim(); err != nil {
			summary.LastError = err.Error()
			// Admission closed mid-pass: stop candidate work and do not append
			// further durable cleanup events (including completed) after close.
			summary.LastCompletedAt = stringPtr(formatJavaScriptISOString(r.now().UTC()))
			if summary.Failed > 0 {
				summary.LastStatus = "failed"
			} else {
				summary.LastStatus = "completed"
			}
			return summary
		}
		if decision.Action != worktreecleanup.ActionWouldClean {
			if err := r.recordWorktreeCleanupPlanSkip(ctx, repos, decision.Worktree, decision.Reason); err != nil {
				// Admission closed between the loop recheck and the skip record —
				// stop without durable skip/completed events after closure.
				summary.LastError = err.Error()
				summary.LastCompletedAt = stringPtr(formatJavaScriptISOString(r.now().UTC()))
				summary.LastStatus = "completed"
				return summary
			}
			summary.Skipped++
			continue
		}
		result := r.cleanupWorktreeCandidate(ctx, repos, gitGateway, cfg, decision.Worktree)
		switch result.status {
		case "cleaned":
			summary.Cleaned++
		case "skipped":
			summary.Skipped++
		default:
			summary.Failed++
			summary.LastError = result.message
		}
	}

	summary.LastCompletedAt = stringPtr(formatJavaScriptISOString(r.now().UTC()))
	if summary.Failed > 0 {
		summary.LastStatus = "failed"
	} else {
		summary.LastStatus = "completed"
	}
	// Terminal completed is a durable cleanup mutation: hold admission so a
	// concurrent MarkDegraded/cancel cannot append after closure.
	_ = r.WithAllowClaim(func() {
		_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.completed", nil, map[string]any{
			"status":      summary.LastStatus,
			"scanned":     summary.Scanned,
			"candidates":  summary.Candidates,
			"cleaned":     summary.Cleaned,
			"skipped":     summary.Skipped,
			"failed":      summary.Failed,
			"lastError":   summary.LastError,
			"completedAt": derefString(summary.LastCompletedAt),
		})
	})
	return summary
}

type worktreeCleanupCandidateResult struct {
	status  string
	message string
}

func (r *Runtime) cleanupWorktreeCandidate(ctx context.Context, repos *storage.Repositories, gitGateway worktreeCleanupGit, cfg config.Config, candidate storage.WorktreeRecord) worktreeCleanupCandidateResult {
	// Per-candidate gate: admission can close after the pass/loop recheck and
	// before this candidate mutates records or deletes a checkout.
	if err := r.AllowClaim(); err != nil {
		return worktreeCleanupCandidateResult{status: "skipped", message: err.Error()}
	}
	current, err := repos.Worktrees.GetByID(ctx, candidate.ID)
	if err != nil {
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, err)
	}
	if current == nil || current.Status == "cleaned" {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "record_already_cleaned")
	}
	candidate = *current

	project, err := repos.Projects.GetByID(ctx, candidate.ProjectID)
	if err != nil {
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, err)
	}
	if project == nil {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "project_missing")
	}
	if project.Archived {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "project_archived")
	}

	worktreeRoot, err := worktreeCleanupRoot(*project)
	if err != nil {
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, err)
	}
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: candidate.WorktreePath, RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "unsafe_worktree_path: "+err.Error())
	}
	if active, err := worktreeCleanupCandidateActive(ctx, repos, candidate); err != nil {
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, err)
	} else if active {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "active_loop_or_run_references_worktree")
	}
	if active, err := worktreeCleanupCandidateActiveQueue(ctx, repos, candidate); err != nil {
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, err)
	} else if active {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "active_queue_item_references_worktree")
	}

	listed, listErr := gitGateway.ListWorktrees(ctx, project.RepoPath)
	if listErr != nil {
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, listErr)
	}
	inGitList := worktreeInList(listed, candidate.WorktreePath)
	if _, statErr := os.Stat(candidate.WorktreePath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) && !inGitList {
			return r.cleanWorktreeCandidate(ctx, repos, gitGateway, cfg, *project, candidate, worktreeRoot, "missing_checkout")
		}
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, statErr)
	}
	if !inGitList {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "worktree_not_registered")
	}

	clean, err := gitGateway.WorktreeClean(ctx, candidate.WorktreePath)
	if err != nil {
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, err)
	}
	if !clean {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "dirty_git_status")
	}
	return r.cleanWorktreeCandidate(ctx, repos, gitGateway, cfg, *project, candidate, worktreeRoot, "clean")
}

// recordWorktreeCleanupPlanSkip writes a durable skip event only while admission
// still allows claims. Hold WithAllowClaim across the append so MarkDegraded
// cannot close admission between a point-in-time AllowClaim and the event write.
// Returns the admission error when closed so callers stop without counting a
// skip that was never recorded.
func (r *Runtime) recordWorktreeCleanupPlanSkip(ctx context.Context, repos *storage.Repositories, candidate storage.WorktreeRecord, reason string) error {
	return r.WithAllowClaim(func() {
		_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.skipped", &candidate, map[string]any{"reason": reason})
	})
}

func (r *Runtime) cleanWorktreeCandidate(ctx context.Context, repos *storage.Repositories, gitGateway worktreeCleanupGit, cfg config.Config, project storage.ProjectRecord, candidate storage.WorktreeRecord, worktreeRoot, reason string) worktreeCleanupCandidateResult {
	if cfg.Daemon.WorktreeCleanup.DryRun {
		return r.recordWorktreeCleanupSkip(ctx, repos, candidate, "dry_run")
	}
	// Do NOT hold admission.mu across the full CleanupWorktree Wait: BeginShutdown/
	// MarkDegraded take that mutex for the closed transition + cancelWorkProducers;
	// holding it for a stalled `git worktree remove` deadlocks degrade/shutdown.
	//
	// R7 atomicity with admission closure: point-in-time AllowClaim + ctx.Err
	// leave a window before cmd.Start where MarkDegraded can close admission and
	// cancel, yet git worktree remove can still Start (cancellation is not a
	// reservation synchronized with process start). AdmitStart holds WithAllowClaim
	// only across Start; retry waits and Wait/Drain stay outside so cancel can run.
	if cleanErr := gitGateway.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{
		ProjectID:         candidate.ProjectID,
		RepoPath:          project.RepoPath,
		WorktreeRoot:      worktreeRoot,
		WorktreePath:      candidate.WorktreePath,
		Branch:            candidate.Branch,
		ProtectedBranches: []string{derefString(project.BaseBranch)},
		AdmitStart:        r.admitWorktreeCleanupStart(ctx),
	}); cleanErr != nil {
		// AdmitStart refusals (closed admission or canceled ctx before Start)
		// are skips, not durable cleanup failures — no remove began.
		if isWorktreeCleanupStartRefused(cleanErr) {
			return worktreeCleanupCandidateResult{status: "skipped", message: cleanErr.Error()}
		}
		return r.recordWorktreeCleanupFailure(ctx, repos, candidate, cleanErr)
	}
	var result worktreeCleanupCandidateResult
	if err := r.WithAllowClaim(func() {
		_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.cleaned", &candidate, map[string]any{"reason": reason})
		result = worktreeCleanupCandidateResult{status: "cleaned"}
	}); err != nil {
		// Filesystem remove may have completed after admission closed; do not
		// append durable cleaned after close. Surface cleaned for in-memory
		// summary so operators see the mutation that already happened.
		return worktreeCleanupCandidateResult{status: "cleaned", message: err.Error()}
	}
	return result
}

// admitWorktreeCleanupStart returns a StartGate that holds claim admission across
// cmd.Start only. MarkDegraded cannot transition between the allow check and
// process launch; the hold is released before Wait so degrade can still cancel.
func (r *Runtime) admitWorktreeCleanupStart(ctx context.Context) func(start func() error) error {
	return func(start func() error) error {
		var startErr error
		if err := r.WithAllowClaim(func() {
			if err := ctx.Err(); err != nil {
				startErr = err
				return
			}
			if start != nil {
				startErr = start()
			}
		}); err != nil {
			return err
		}
		return startErr
	}
}

// isWorktreeCleanupStartRefused reports errors from AdmitStart that mean the
// destructive remove never launched (admission closed or context canceled).
func isWorktreeCleanupStartRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return errors.Is(err, ErrAdmissionNotReady) ||
		errors.Is(err, ErrAdmissionStopping) ||
		errors.Is(err, ErrAdmissionDegraded)
}

// recordWorktreeCleanupSkip holds admission across the worktrees touch and skip
// event so degradation after eligibility checks cannot commit durable cleanup
// mutations after close. Callers that already hold WithAllowClaim must use
// writeWorktreeCleanupSkip instead.
func (r *Runtime) recordWorktreeCleanupSkip(ctx context.Context, repos *storage.Repositories, candidate storage.WorktreeRecord, reason string) worktreeCleanupCandidateResult {
	var result worktreeCleanupCandidateResult
	if err := r.WithAllowClaim(func() {
		result = r.writeWorktreeCleanupSkip(ctx, repos, candidate, reason)
	}); err != nil {
		return worktreeCleanupCandidateResult{status: "skipped", message: err.Error()}
	}
	return result
}

func (r *Runtime) writeWorktreeCleanupSkip(ctx context.Context, repos *storage.Repositories, candidate storage.WorktreeRecord, reason string) worktreeCleanupCandidateResult {
	if err := r.touchWorktreeCleanupAttempt(ctx, repos, candidate); err != nil {
		message := err.Error()
		_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.failed", &candidate, map[string]any{"message": message})
		return worktreeCleanupCandidateResult{status: "failed", message: message}
	}
	_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.skipped", &candidate, map[string]any{"reason": reason})
	return worktreeCleanupCandidateResult{status: "skipped", message: reason}
}

// recordWorktreeCleanupFailure holds admission across the worktrees touch and
// failure event (same write-boundary contract as recordWorktreeCleanupSkip).
// Callers already inside WithAllowClaim must use writeWorktreeCleanupFailure.
func (r *Runtime) recordWorktreeCleanupFailure(ctx context.Context, repos *storage.Repositories, candidate storage.WorktreeRecord, cause error) worktreeCleanupCandidateResult {
	var result worktreeCleanupCandidateResult
	if err := r.WithAllowClaim(func() {
		result = r.writeWorktreeCleanupFailure(ctx, repos, candidate, cause)
	}); err != nil {
		return worktreeCleanupCandidateResult{status: "skipped", message: err.Error()}
	}
	return result
}

func (r *Runtime) writeWorktreeCleanupFailure(ctx context.Context, repos *storage.Repositories, candidate storage.WorktreeRecord, cause error) worktreeCleanupCandidateResult {
	message := cause.Error()
	if touchErr := r.touchWorktreeCleanupAttempt(ctx, repos, candidate); touchErr != nil {
		message = message + "; " + touchErr.Error()
	}
	_ = r.appendWorktreeCleanupEvent(ctx, repos, "worktree.cleanup.failed", &candidate, map[string]any{"message": message})
	return worktreeCleanupCandidateResult{status: "failed", message: message}
}

func (r *Runtime) touchWorktreeCleanupAttempt(ctx context.Context, repos *storage.Repositories, candidate storage.WorktreeRecord) error {
	if repos == nil || repos.Worktrees == nil || candidate.ID == "" {
		return nil
	}
	return repos.Worktrees.TouchCleanupAttempt(ctx, candidate.ID, formatJavaScriptISOString(r.now().UTC()))
}

func (r *Runtime) appendWorktreeCleanupEvent(ctx context.Context, repos *storage.Repositories, eventType string, candidate *storage.WorktreeRecord, payload map[string]any) error {
	if repos == nil || repos.Events == nil {
		return nil
	}
	projectID := (*string)(nil)
	entityID := (*string)(nil)
	if candidate != nil {
		projectID = stringPtr(candidate.ProjectID)
		entityID = stringPtr(candidate.ID)
		payload["worktreeId"] = candidate.ID
		payload["worktreePath"] = candidate.WorktreePath
		payload["branch"] = candidate.Branch
	}
	return appendSystemEvent(ctx, repos, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   eventType,
		ProjectID:   projectID,
		EntityType:  stringPtr("worktree"),
		EntityID:    entityID,
		PayloadJSON: mustMarshalJSON(payload),
		CreatedAt:   formatJavaScriptISOString(r.now().UTC()),
	})
}

func worktreeCleanupMaxPerTick(cfg config.Config) int {
	if cfg.Daemon.WorktreeCleanup.MaxPerTick <= 0 {
		return 10
	}
	return cfg.Daemon.WorktreeCleanup.MaxPerTick
}

func worktreeCleanupRoot(project storage.ProjectRecord) (string, error) {
	if project.MetadataJSON != nil && strings.TrimSpace(*project.MetadataJSON) != "" {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(*project.MetadataJSON), &metadata); err == nil {
			if value, ok := metadata["worktreeRoot"].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value), nil
			}
		}
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
}

func worktreeInList(items []gitinfra.WorktreeListEntry, path string) bool {
	for _, item := range items {
		if normalizeRuntimePath(item.Path) == normalizeRuntimePath(path) {
			return true
		}
	}
	return false
}

func normalizeRuntimePath(path string) string {
	return strings.TrimRight(strings.TrimSpace(path), string(os.PathSeparator))
}

func worktreeCleanupCandidateActive(ctx context.Context, repos *storage.Repositories, candidate storage.WorktreeRecord) (bool, error) {
	loops, err := repos.Loops.List(ctx)
	if err != nil {
		return false, err
	}
	for _, loop := range loops {
		if loop.ProjectID != candidate.ProjectID || !worktreeCleanupActiveLoopStatus(loop.Status) {
			continue
		}
		if jsonContainsWorktree(loop.MetadataJSON, candidate) {
			return true, nil
		}
		run, err := repos.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return false, err
		}
		if run != nil && jsonContainsWorktree(run.CheckpointJSON, candidate) {
			return true, nil
		}
	}
	return false, nil
}

func worktreeCleanupCandidateActiveQueue(ctx context.Context, repos *storage.Repositories, candidate storage.WorktreeRecord) (bool, error) {
	items, err := repos.Queue.List(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Status != "queued" && item.Status != "running" {
			continue
		}
		if jsonContainsWorktree(item.PayloadJSON, candidate) {
			return true, nil
		}
		if item.LoopID == nil {
			continue
		}
		loop, err := repos.Loops.GetByID(ctx, *item.LoopID)
		if err != nil {
			return false, err
		}
		if loop != nil && loop.ProjectID == candidate.ProjectID && jsonContainsWorktree(loop.MetadataJSON, candidate) {
			return true, nil
		}
	}
	return false, nil
}

func worktreeCleanupActiveLoopStatus(status string) bool {
	switch status {
	// human_takeover keeps the worktree pinned: a human is (or is about to be)
	// driving the loop's agent session inside it — reclaiming it would pull the
	// working tree out from under them.
	case "idle", "queued", "running", "paused", "waiting", "human_takeover":
		return true
	default:
		return false
	}
}

func jsonContainsWorktree(raw *string, candidate storage.WorktreeRecord) bool {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return false
	}
	return strings.Contains(*raw, candidate.WorktreePath) || strings.Contains(*raw, candidate.Branch) || strings.Contains(*raw, candidate.ID)
}

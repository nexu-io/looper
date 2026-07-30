package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// notifyHumanAttentionBestEffort builds a short-lived gateway from the current
// runtime config and emits human-attention notifications without affecting the
// caller's control flow. Used from recovery rescan (async), post-claim delivery,
// and tests.
func (r *Runtime) notifyHumanAttentionBestEffort(ctx context.Context, repos *storage.Repositories, loopID string) {
	if r == nil || repos == nil || strings.TrimSpace(loopID) == "" {
		return
	}
	cfg := r.Config()
	gateway := notify.NewGateway(notify.Options{
		Config:        cfg.Notifications,
		OsascriptPath: strings.TrimSpace(derefString(cfg.Tools.OsascriptPath)),
		Repositories:  repos,
		Now:           r.now,
	})
	notifyDurableHumanAttention(ctx, gateway, repos, loopID)
}

// ensureHumanAttentionNotifyCtx returns the shared cancelable parent for
// human-attention delivery. Refuses to arm after Stop or while admission is
// stopping so a post-claim race cannot outlive BeginShutdown's cancel snapshot.
func (r *Runtime) ensureHumanAttentionNotifyCtx() (context.Context, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, false
	}
	if r.admission != nil && r.admission.State() == AdmissionStopping {
		return nil, false
	}
	if r.humanAttentionNotifyCancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		r.humanAttentionNotifyCtx = ctx
		r.humanAttentionNotifyCancel = cancel
	}
	return r.humanAttentionNotifyCtx, true
}

// goHumanAttentionNotify runs fn under the shared cancelable HA context and
// tracks it on humanAttentionNotifyWG so Stop can drain before SQLite close.
func (r *Runtime) goHumanAttentionNotify(fn func(context.Context)) {
	if r == nil || fn == nil {
		return
	}
	ctx, ok := r.ensureHumanAttentionNotifyCtx()
	if !ok {
		return
	}
	r.humanAttentionNotifyWG.Add(1)
	go func() {
		defer r.humanAttentionNotifyWG.Done()
		if ctx.Err() != nil {
			return
		}
		fn(ctx)
	}()
}

// notifyHumanAttentionPostClaim is the scheduler post-finalize callback.
// It ignores the dispatch WithoutCancel context: notification delivery must
// cancel when BeginShutdown fires, and Stop must drain before SQLite close.
// Delivery is async so a slow local or remote notifier cannot pin a scheduler
// task slot across the shutdown timeout.
func (r *Runtime) notifyHumanAttentionPostClaim(_ context.Context, loopID string) {
	if r == nil {
		return
	}
	loopID = strings.TrimSpace(loopID)
	if loopID == "" {
		return
	}
	services := r.Services()
	if services.Repositories == nil {
		return
	}
	repos := services.Repositories
	r.goHumanAttentionNotify(func(ctx context.Context) {
		r.notifyHumanAttentionBestEffort(ctx, repos, loopID)
	})
}

// scheduleHumanAttentionRecoveryNotify rescans durable human-attention parks
// after CompleteStartup has committed (MarkReady succeeded). Callers must not
// schedule this while later startup steps can still fail and close SQLite.
// Delivery runs asynchronously so notification transports cannot delay
// admission readiness.
//
// Covers: (1) parks that crashed after durable await/manual hold but before the
// scheduler finalize callback; (2) recovery quarantine parks that used to notify
// on the critical path. Permanent entry-scoped dedupe decides whether an alert
// is sent — not the crash-sensitive post-finalize callback window.
//
// Shares the cancel/WG lifecycle with post-claim delivery; Stop joins both
// before SQLite close so queries cannot race coordinator.Close.
//
// Mirrors startDeferredReviewerRecovery: require ready admission, publish the
// generation under lock, then recheck so a BeginShutdown that missed a nil
// cancel cannot leave a live notify goroutine after stop has already drained.
func (r *Runtime) scheduleHumanAttentionRecoveryNotify(repos *storage.Repositories) {
	if r == nil || repos == nil {
		return
	}
	done := make(chan struct{})
	r.mu.Lock()
	// Refuse once shutdown has begun or admission is no longer ready.
	// Checking only Stopping/stopped is insufficient: BeginShutdown may race
	// after MarkReady and before cancel is published; stop may already have
	// waited on a nil cancel/empty WaitGroup.
	if r.stopped || r.admission == nil || r.admission.State() != AdmissionReady {
		r.mu.Unlock()
		return
	}
	// CompleteStartup is once; if a prior schedule exists (tests), wait it out
	// without canceling the shared delivery context (post-claim may be live).
	prevDone := r.humanAttentionNotifyDone
	r.humanAttentionNotifyDone = done
	r.mu.Unlock()
	if prevDone != nil {
		<-prevDone
	}

	ctx, ok := r.ensureHumanAttentionNotifyCtx()
	if !ok {
		r.clearHumanAttentionNotifyDone(done)
		close(done)
		return
	}

	// Publish-then-recheck: if BeginShutdown raced between the ready check and
	// arming humanAttentionNotifyCancel, it missed cancel. If admission is no
	// longer ready, do not launch work (stop may already have drained).
	if r.admission == nil || r.admission.State() != AdmissionReady {
		r.clearHumanAttentionNotifyDone(done)
		close(done)
		return
	}

	r.humanAttentionNotifyWG.Add(1)
	go func() {
		defer r.humanAttentionNotifyWG.Done()
		defer close(done)
		if ctx.Err() != nil {
			return
		}
		// Born-canceled / late-shutdown recheck before any SQLite or osascript.
		if r.isStopped() || r.admission == nil || r.admission.State() != AdmissionReady {
			return
		}
		r.notifyDurableHumanAttentionParksBestEffort(ctx, repos)
	}()
}

func (r *Runtime) clearHumanAttentionNotifyDone(done chan struct{}) {
	if r == nil || done == nil {
		return
	}
	r.mu.Lock()
	if r.humanAttentionNotifyDone == done {
		r.humanAttentionNotifyDone = nil
	}
	r.mu.Unlock()
}

// stopHumanAttentionRecoveryNotify cancels all human-attention delivery
// (recovery rescan + post-claim) and waits for inflight work to exit
// (bounded by shutdownTimeout) before SQLite may be closed.
//
// A wait timeout is a drain failure: shell.Run may still be in TERM grace /
// SIGKILL (up to ~5s + slack) while Stop would otherwise close SQLite. Record
// shutdownDrainErr so Stop retains storage ownership (#577).
func (r *Runtime) stopHumanAttentionRecoveryNotify() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.humanAttentionNotifyCancel
	r.humanAttentionNotifyCancel = nil
	r.humanAttentionNotifyCtx = nil
	r.humanAttentionNotifyDone = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		r.humanAttentionNotifyWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(r.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		if r.logger != nil {
			r.logger.Warn("looperd stop timed out waiting for human-attention notify", map[string]any{
				"timeoutMs": r.shutdownTimeout.Milliseconds(),
			})
		}
		// Confirmed-drain failed: retain SQLite so a still-live osascript/notify
		// cannot persist through a closed coordinator.
		r.mu.Lock()
		r.shutdownDrainErr = errors.Join(r.shutdownDrainErr, fmt.Errorf("%w: human-attention notify", errShutdownDrainTimeout))
		r.mu.Unlock()
	}
}

// WaitForHumanAttentionRecoveryNotify blocks until the post-recovery human-
// attention rescan exits, or until ctx is canceled. Returns immediately when
// the rescan was never scheduled. Test fixtures use this after CompleteStartup
// so later Stop/TempDir cleanup does not race a still-running query.
func (r *Runtime) WaitForHumanAttentionRecoveryNotify(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	done := r.humanAttentionNotifyDone
	r.mu.RUnlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// notifyDurableHumanAttentionParksBestEffort lists durable awaiting_human loops
// and latest hard manual_intervention queue holds, then observes each once.
// Safe to call from a background goroutine; skips when the runtime is stopping
// or the provided context is canceled.
func (r *Runtime) notifyDurableHumanAttentionParksBestEffort(ctx context.Context, repos *storage.Repositories) {
	if r == nil || repos == nil {
		return
	}
	if ctx.Err() != nil || r.isStopped() {
		return
	}
	for _, loopID := range collectHumanAttentionLoopIDs(ctx, repos) {
		if ctx.Err() != nil || r.isStopped() {
			return
		}
		r.notifyHumanAttentionBestEffort(ctx, repos, loopID)
	}
}

func (r *Runtime) isStopped() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stopped
}

// collectHumanAttentionLoopIDs returns loop IDs that may need human-attention
// observation after recovery: durable awaiting_human loop status, and latest
// queue rows parked as manual_intervention. notifyDurableHumanAttention applies
// the hard-condition filter and permanent entry dedupe.
func collectHumanAttentionLoopIDs(ctx context.Context, repos *storage.Repositories) []string {
	if repos == nil {
		return nil
	}
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if repos.Loops != nil {
		loops, err := repos.Loops.ListByStatuses(ctx, []string{string(domain.LoopStatusAwaitingHuman)})
		if err == nil {
			for _, loop := range loops {
				add(loop.ID)
			}
		}
	}
	if repos.Queue != nil {
		// Latest row per loop only: older manual_intervention history is not a current park.
		items, err := repos.Queue.ListLatestByLoopStatuses(ctx, []string{"manual_intervention"})
		if err == nil {
			for _, item := range items {
				if item.LoopID != nil {
					add(*item.LoopID)
				}
			}
		}
	}
	return ids
}

// notifyDurableHumanAttention observes durable loop/queue state after a claim
// finishes (or after recovery parks work) and emits at most one action_required
// notification for a newly entered human-attention condition.
//
// Authority remains the durable loop/queue state. Notification is best-effort:
// failures are audited by the gateway and must not change loop/queue/run behavior.
func notifyDurableHumanAttention(ctx context.Context, gateway *notify.Gateway, repos *storage.Repositories, loopID string) {
	if gateway == nil || repos == nil || repos.Loops == nil || strings.TrimSpace(loopID) == "" {
		return
	}
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}

	if notify.IsHumanAttentionLoopStatus(loop.Status) {
		entryKey := humanAttentionEntryKeyForAwaitingHuman(ctx, repos, *loop)
		if entryKey == "" {
			return
		}
		gateway.NotifyHumanAttention(ctx, notify.HumanAttentionInput{
			ProjectID:  loop.ProjectID,
			LoopID:     loop.ID,
			LoopSeq:    loop.Seq,
			RunID:      latestRunID(ctx, repos, loop.ID),
			LoopType:   loop.Type,
			Reason:     notify.HumanAttentionAwaitingHuman,
			EntryKey:   entryKey,
			Subtitle:   humanAttentionSubtitle(*loop),
			EntityType: "loop",
			EntityID:   loop.ID,
		})
		return
	}

	if repos.Queue == nil {
		return
	}
	queue, err := repos.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || queue == nil {
		return
	}
	if queue.Status != "manual_intervention" {
		return
	}
	// CancelByLoop (used by Terminate/Pause) only cancels queued/running rows, so a
	// terminal loop can retain a stale manual_intervention queue status. Alert only
	// when the loop itself is still active — terminated/completed parks are not
	// current operator holds.
	if !domain.IsActiveLoopStatus(domain.LoopStatus(loop.Status)) {
		return
	}
	lastErrorKind := ""
	if queue.LastErrorKind != nil {
		lastErrorKind = *queue.LastErrorKind
	}
	resumePolicy := ""
	if repos.Runs != nil {
		if run, runErr := repos.Runs.GetLatestByLoopID(ctx, loop.ID); runErr == nil && run != nil {
			resumePolicy = resumePolicyFromCheckpointJSON(run.CheckpointJSON)
		}
	}
	if !notify.IsManualInterventionCondition(lastErrorKind, resumePolicy) {
		return
	}
	entryKey := humanAttentionEntryKeyForManualIntervention(*queue)
	if entryKey == "" {
		return
	}
	gateway.NotifyHumanAttention(ctx, notify.HumanAttentionInput{
		ProjectID:  loop.ProjectID,
		LoopID:     loop.ID,
		LoopSeq:    loop.Seq,
		RunID:      latestRunID(ctx, repos, loop.ID),
		LoopType:   loop.Type,
		Reason:     notify.HumanAttentionManualIntervention,
		EntryKey:   entryKey,
		Subtitle:   humanAttentionSubtitle(*loop),
		EntityType: "queue_item",
		EntityID:   queue.ID,
	})
}

func humanAttentionEntryKeyForAwaitingHuman(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord) string {
	// Prefer run id so leave+re-enter (new run) is a new event. Fall back to
	// askedAt / loop.UpdatedAt so a park without a run still dedupes across restarts.
	if runID := latestRunID(ctx, repos, loop.ID); runID != "" {
		return "run:" + runID
	}
	if ask, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok {
		if askedAt := strings.TrimSpace(ask.AskedAt); askedAt != "" {
			return "asked:" + askedAt
		}
	}
	if updated := strings.TrimSpace(loop.UpdatedAt); updated != "" {
		return "loop:" + loop.ID + ":" + updated
	}
	return "loop:" + loop.ID
}

func humanAttentionEntryKeyForManualIntervention(queue storage.QueueItemRecord) string {
	// FinishedAt changes on each durable park of the same queue row after requeue.
	if queue.FinishedAt != nil && strings.TrimSpace(*queue.FinishedAt) != "" {
		return "queue:" + queue.ID + ":" + strings.TrimSpace(*queue.FinishedAt)
	}
	if updated := strings.TrimSpace(queue.UpdatedAt); updated != "" {
		return "queue:" + queue.ID + ":" + updated
	}
	return "queue:" + queue.ID
}

func humanAttentionSubtitle(loop storage.LoopRecord) string {
	parts := make([]string, 0, 3)
	if repo := strings.TrimSpace(derefString(loop.Repo)); repo != "" {
		parts = append(parts, repo)
	}
	if loop.PRNumber != nil && *loop.PRNumber > 0 {
		parts = append(parts, "#"+strconv.FormatInt(*loop.PRNumber, 10))
	}
	if loop.Type != "" {
		parts = append(parts, loop.Type)
	}
	return strings.Join(parts, " ")
}

func latestRunID(ctx context.Context, repos *storage.Repositories, loopID string) string {
	if repos == nil || repos.Runs == nil || strings.TrimSpace(loopID) == "" {
		return ""
	}
	run, err := repos.Runs.GetLatestByLoopID(ctx, loopID)
	if err != nil || run == nil {
		return ""
	}
	return strings.TrimSpace(run.ID)
}

func resumePolicyFromCheckpointJSON(checkpointJSON *string) string {
	if checkpointJSON == nil || strings.TrimSpace(*checkpointJSON) == "" {
		return ""
	}
	var parsed struct {
		ResumePolicy string `json:"resumePolicy"`
	}
	if err := json.Unmarshal([]byte(*checkpointJSON), &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.ResumePolicy)
}

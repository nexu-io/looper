package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/loops"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/storage"
)

// shepherdReconcileLockTTL matches the worker's shepherd lock TTL; the reconciler
// refreshes it every tick (tick << TTL) so a same-account fixer keeps skipping.
const shepherdReconcileLockTTL = 20 * time.Minute

// shepherdReconcileInterval is the dedicated shepherd poll cadence. looper has no
// per-PR-event webhook seam, so a worker loop driving its PR to merge polls on
// this cadence (well under the lock TTL) to pick up a re-review / CI change /
// conflict / a human's merge promptly — independent of the slower 5-min
// wake-reconcile.
const shepherdReconcileInterval = 60 * time.Second

// Plane state writes are best-effort and Plane can be temporarily slow or unavailable.
// Remember failed attempts in the durable shepherd marker and retry at this cadence,
// rather than hammering Plane on every 60-second PR reconciliation tick.
const shepherdPlaneStateRetryInterval = 10 * time.Minute

// shepherdMaxPasses caps how many agent passes one PR may consume before the
// shepherd stops waking and leaves it for a human — a backstop against a flapping
// PR burning agent quota. Counts only agent-waking passes.
const shepherdMaxPasses = 25

// reconcileWorkerShepherd drives a worker loop that is shepherding its own impl
// PR toward merge. Polling (looper has no per-PR-event webhook seam): for each
// loop in status "shepherding" it refreshes the coordination lock, reads live PR
// state via the full GitHub gateway, and:
//   - state MERGED (a HUMAN colleague merged) → terminal completed+outcome=merged;
//     CLOSED (not merged) → completed+outcome=abandoned. Releases the lock.
//   - else, when the folded signal changed AND something is actionable
//     (CHANGES_REQUESTED / failing CI / conflict), enqueue ONE worker pass (the
//     resolver forces stepShepherd via the durable marker). No wake when nothing
//     is actionable — the bot NEVER merges; it just keeps the PR healthy and
//     waits for a human to merge.
//
// Best-effort; a no-op without storage or the GitHub gateway.
func (r *Runtime) reconcileWorkerShepherd(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	now := r.now
	logger := r.logger
	gateway := r.githubGateway
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil || repositories.Queue == nil || gateway == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	// Completed shepherd loops are included only to retry a failed terminal Plane
	// state write. They never touch GitHub or wake an agent again.
	shepherding, err := repositories.Loops.ListByStatuses(ctx, []string{"shepherding", "completed"})
	if err != nil {
		return
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	for _, loop := range shepherding {
		if strings.EqualFold(strings.TrimSpace(loop.Status), "completed") {
			r.reconcileCompletedShepherdPlaneState(ctx, repositories, loop, nowISO)
			continue
		}
		r.reconcileOneShepherdLoop(ctx, repositories, gateway, now, loop, nowISO, logger)
	}
}

// ReconcilePullRequest reconciles the worker loop shepherding a specific PR right
// away — the event-driven path (a webhook PR event fires this instead of waiting
// for the 60s poll). A no-op when no loop is shepherding that PR.
func (r *Runtime) ReconcilePullRequest(ctx context.Context, projectID, repo string, prNumber int64) error {
	r.mu.RLock()
	repositories := r.services.Repositories
	now := r.now
	logger := r.logger
	gateway := r.githubGateway
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil || repositories.Queue == nil || gateway == nil || prNumber <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	shepherding, err := repositories.Loops.ListByStatuses(ctx, []string{"shepherding"})
	if err != nil {
		return nil
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	for _, loop := range shepherding {
		if loop.PRNumber == nil || *loop.PRNumber != prNumber {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(derefString(loop.Repo)), strings.TrimSpace(repo)) {
			continue
		}
		r.reconcileOneShepherdLoop(ctx, repositories, gateway, now, loop, nowISO, logger)
	}
	return nil
}

// reconcileOneShepherdLoop is the per-loop shepherd reconcile shared by the 60s
// poll and the event-driven ReconcilePullRequest.
func (r *Runtime) reconcileOneShepherdLoop(ctx context.Context, repositories *storage.Repositories, gateway *githubinfra.Gateway, now func() time.Time, loop storage.LoopRecord, nowISO string, logger bootstrap.Logger) {
	if !strings.EqualFold(strings.TrimSpace(loop.Type), "worker") {
		return
	}
	marker, ok := loops.ReadShepherd(loop.MetadataJSON)
	if !ok || !marker.Active {
		return
	}
	repo := strings.TrimSpace(derefString(loop.Repo))
	if repo == "" || loop.PRNumber == nil || *loop.PRNumber <= 0 {
		return
	}
	prNumber := *loop.PRNumber
	// Shepherding itself proves the implementation PR exists, so keep Plane at
	// In Review independently of the live GitHub read below. GitHub auth/network
	// failures must not suppress Plane's own lifecycle reconciliation.
	if r.syncShepherdPlaneState(ctx, repositories, &marker, loop, "In Review", nowISO) {
		if metadata, werr := loops.WriteShepherd(loop.MetadataJSON, marker); werr == nil {
			r.persistLoopMetadata(ctx, repositories, loop.ID, metadata, nowISO)
			loop.MetadataJSON = &metadata
		}
	}
	cwd := ""
	if proj, perr := repositories.Projects.GetByID(ctx, loop.ProjectID); perr == nil && proj != nil {
		cwd = proj.RepoPath
	}
	r.refreshShepherdLock(ctx, repositories, loop.ID, repo, prNumber, now)

	// ViewPullRequestForReviewer (not ViewPullRequest) so the detail carries
	// reviews + statusCheckRollup — the plain view uses metadata-only fields
	// and would leave Checks/Reviews empty, blinding CI and review-round detection.
	pr, err := gateway.ViewPullRequestForReviewer(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return
	}
	if outcome := shepherdTerminalOutcome(pr); outcome != "" {
		// Persist the terminal (merged/closed) snapshot BEFORE flipping the loop to
		// completed, so the anchor card's §A path resolves 🎉 已合并 / 🚫 已关闭 from real
		// data — a deploy with reviewer/fixer off otherwise never writes a snapshot.
		r.captureShepherdSnapshot(ctx, repositories, gateway, loop, repo, prNumber, pr, nowISO)
		r.terminateShepherd(ctx, repositories, loop, repo, prNumber, outcome, nowISO, logger)
		return
	}
	signal := foldShepherdSignal(pr)
	if signal == marker.LastSignal {
		// A shepherd pass stamps the durable phase to "fixing" while the agent is
		// running. The pass can finish without changing the PR signal (for example,
		// a final read-only sweep after approval + green CI). In that case the old
		// early return left the Feishu card permanently stuck on 修复中 even though
		// the live PR was already awaiting QA/merge. Always reconcile phase drift
		// from the live PR before resting, even when the wake signal is unchanged.
		phaseChanged := syncShepherdPhase(&marker, pr)
		if phaseChanged {
			r.reportShepherdPhase(ctx, &marker, loop, prNumber, prValidated(pr))
			if newMeta, werr := loops.WriteShepherd(loop.MetadataJSON, marker); werr == nil {
				if newMeta, werr = loopcondition.Set(&newMeta, shepherdRestingCondition(pr, signal, nowISO)); werr == nil {
					r.persistLoopMetadata(ctx, repositories, loop.ID, newMeta, nowISO)
					r.refreshShepherdCard(ctx, loop.ID)
				}
			}
			return
		}
		r.persistShepherdBlockedCondition(ctx, repositories, loop, shepherdRestingCondition(pr, signal, nowISO), nowISO)
		return
	}
	// The folded PR signal changed → persist a fresh snapshot (best-effort, only on a
	// real change, so low-frequency) so the PR-state card mapping (§A: labels /
	// reviewDecision / checks / merge state) has data even though reviewer/fixer are
	// off. No extra gh call — reuses the detail already fetched this tick.
	r.captureShepherdSnapshot(ctx, repositories, gateway, loop, repo, prNumber, pr, nowISO)
	marker.LastSignal = signal
	syncShepherdPhase(&marker, pr)
	// Milestone thread note + @-mention on entering a report-worthy phase, BEFORE we
	// persist — so LastReportedPhase is saved and the next pass doesn't double-post.
	r.reportShepherdPhase(ctx, &marker, loop, prNumber, prValidated(pr))
	wakePass := shepherdActionable(pr) || commentedOnCurrentHead(pr)
	if newMeta, werr := loops.WriteShepherd(loop.MetadataJSON, marker); werr == nil {
		if wakePass {
			newMeta, werr = loopcondition.Clear(&newMeta)
		} else {
			newMeta, werr = loopcondition.Set(&newMeta, shepherdRestingCondition(pr, signal, nowISO))
		}
		if werr != nil {
			return
		}
		r.persistLoopMetadata(ctx, repositories, loop.ID, newMeta, nowISO)
		// The folded signal changed (past the guard above), so the phase may have
		// advanced (👀 评审中 → 🔧 修复中 → ✅ 待合并). Refresh the anchor card now: a
		// passively-shepherded PR runs no scheduler pass, so nothing else would move
		// the card off the worker's "🔨 实现中".
		r.refreshShepherdCard(ctx, loop.ID)
	}
	// Enqueue a pass for the actionable cases (conflict / failed CI / changes requested
	// at head) OR when a peer left a COMMENTED review at the current head — the latter
	// is not "actionable" for the card phase (a COMMENTED review never clears, so it
	// must NOT wedge shepherdPhaseFor at 修复中 / block 待合并), but it does carry
	// feedback the pass should read + address or answer. We only reach here past the
	// LastSignal guard, i.e. on a genuinely new signal (new review round included), so
	// this fires once per round, not every tick.
	if wakePass {
		if marker.PassCount >= shepherdMaxPasses {
			if logger != nil {
				logger.Info("shepherd: pass cap reached — leaving PR for a human", map[string]any{"loopId": loop.ID, "repo": repo, "prNumber": prNumber, "passCount": marker.PassCount})
			}
			return
		}
		r.enqueueShepherdPass(ctx, repositories, loop, repo, prNumber, nowISO, logger)
	}
}

// syncShepherdPlaneState makes the In Review transition self-healing. The initial
// worker-completed notification still attempts the update immediately; this durable
// retry closes the gap when that one-shot write happened during a Plane outage.
// It returns true when an attempt changed marker metadata and therefore needs
// persistence by the caller.
func (r *Runtime) syncShepherdPlaneState(ctx context.Context, repositories *storage.Repositories, marker *loops.Shepherd, loop storage.LoopRecord, desired, nowISO string) bool {
	if marker == nil {
		return false
	}
	now, err := time.Parse(time.RFC3339Nano, nowISO)
	if err != nil || !shepherdPlaneStateSyncDue(*marker, desired, now) {
		return false
	}
	marker.PlaneStateAttemptAt = nowISO

	r.mu.RLock()
	cfg := r.config
	stateLogger := r.logger
	r.mu.RUnlock()
	if setPlaneWorkItemState(ctx, &cfg, repositories, stateLogger, loop.ProjectID, loop.ID, desired) {
		marker.PlaneState = desired
		marker.PlaneStateAttemptAt = ""
	}
	return true
}

func (r *Runtime) reconcileCompletedShepherdPlaneState(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO string) {
	marker, ok := loops.ReadShepherd(loop.MetadataJSON)
	if !ok || marker.Active || !strings.EqualFold(strings.TrimSpace(marker.Outcome), "merged") {
		return
	}
	if !r.syncShepherdPlaneState(ctx, repositories, &marker, loop, "Done", nowISO) {
		return
	}
	if metadata, err := loops.WriteShepherd(loop.MetadataJSON, marker); err == nil {
		r.persistLoopMetadata(ctx, repositories, loop.ID, metadata, nowISO)
	}
}

func shepherdPlaneStateSyncDue(marker loops.Shepherd, desired string, now time.Time) bool {
	if strings.EqualFold(strings.TrimSpace(marker.PlaneState), strings.TrimSpace(desired)) {
		return false
	}
	attemptedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(marker.PlaneStateAttemptAt))
	return err != nil || now.Sub(attemptedAt) >= shepherdPlaneStateRetryInterval
}

func shepherdRestingCondition(pr githubinfra.PullRequestDetail, signal, nowISO string) loopcondition.Record {
	kind := loopcondition.ReviewUpdated
	if shepherdCIPhase(pr) == "pending" {
		kind = loopcondition.CISettled
	}
	return loopcondition.Record{Kind: kind, Since: nowISO, Fingerprint: signal}
}

func (r *Runtime) persistShepherdBlockedCondition(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, record loopcondition.Record, nowISO string) {
	if current, ok := loopcondition.Read(loop.MetadataJSON); ok && current.Kind == record.Kind && current.Fingerprint == record.Fingerprint {
		return
	}
	metadata, err := loopcondition.Set(loop.MetadataJSON, record)
	if err != nil {
		return
	}
	r.persistLoopMetadata(ctx, repositories, loop.ID, metadata, nowISO)
}

// captureShepherdSnapshot persists a PullRequestSnapshot from the PR detail the
// shepherd already fetched this reconcile, so the Feishu anchor card's PR-state
// path (§A in notify) has real review-cycle data. Without this, a deploy running
// with reviewer/fixer OFF never writes pull_request_snapshots and every delivered
// PR card falls back to a coarse default. Best-effort: any failure is swallowed so
// it can never disturb the shepherd's reconcile. No extra gh call — the diff is
// intentionally omitted (SnapshotFromDetail).
func (r *Runtime) captureShepherdSnapshot(ctx context.Context, repos *storage.Repositories, gateway *githubinfra.Gateway, loop storage.LoopRecord, repo string, prNumber int64, pr githubinfra.PullRequestDetail, capturedAt string) {
	if repos == nil || repos.PullRequestSnapshots == nil || gateway == nil || prNumber <= 0 {
		return
	}
	record, err := gateway.SnapshotFromDetail(pr, loop.ProjectID, repo, prNumber, capturedAt)
	if err != nil {
		return
	}
	_ = repos.PullRequestSnapshots.Upsert(ctx, record)
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// shepherdTerminalOutcome returns "merged"/"abandoned"/"" for a PR.
func shepherdTerminalOutcome(pr githubinfra.PullRequestDetail) string {
	state := strings.ToUpper(strings.TrimSpace(pr.State))
	if state == "MERGED" || strings.TrimSpace(pr.MergedAt) != "" {
		return "merged"
	}
	if state == "CLOSED" {
		return "abandoned"
	}
	return ""
}

// shepherdCIPhase folds statusCheckRollup into a coarse settled/pending phase so
// CI wakes the loop exactly once (when it settles), not once per check.
func shepherdCIPhase(pr githubinfra.PullRequestDetail) string {
	pending := false
	failed := false
	for _, c := range pr.Checks {
		concl := strings.ToUpper(strings.TrimSpace(stringFromAny(c["conclusion"])))
		status := strings.ToUpper(strings.TrimSpace(stringFromAny(c["status"])))
		if concl == "" && status != "COMPLETED" {
			pending = true
			continue
		}
		switch concl {
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			failed = true
		}
	}
	switch {
	case pending:
		return "pending"
	case failed:
		return "failed"
	default:
		return "passed"
	}
}

// latestReviewMarker is the newest top-level review timestamp (across all
// reviewers). It changes when a reviewer submits a NEW review round — even when
// the aggregate reviewDecision stays CHANGES_REQUESTED and the head is unchanged
// — but NOT on the agent's own thread replies (those are review comments, not
// top-level reviews) or pushes, so it catches re-reviews without self-retrigger.
func latestReviewMarker(pr githubinfra.PullRequestDetail) string {
	last := ""
	for _, review := range pr.Reviews {
		at := stringFromAny(review["submittedAt"])
		if at == "" {
			at = stringFromAny(review["updatedAt"])
		}
		if at > last {
			last = at
		}
	}
	return last
}

// foldShepherdSignal is the wake key: stable semantic fields only. It deliberately
// omits pr.UpdatedAt (the agent's own comments bump it → self-retrigger forever)
// but includes latestReviewMarker so a new review round wakes a pass even at the
// same head with the same aggregate decision.
func foldShepherdSignal(pr githubinfra.PullRequestDetail) string {
	// needs-validation/validated are in the tuple so a QA-gate label change (maintainer
	// adds needs-validation, QA adds validated) wakes the reconcile to re-evaluate the
	// phase and re-report — otherwise a validation that lands at the same head/decision
	// would be missed.
	return fmt.Sprintf("%s|%s|%s|%v|%s|%s|%v|%v", strings.ToUpper(pr.State), strings.ToUpper(pr.ReviewDecision), shepherdCIPhase(pr), pr.HasConflicts, pr.HeadSHA, latestReviewMarker(pr), prNeedsValidation(pr), prValidated(pr))
}

// changesRequestedOnCurrentHead reports whether a reviewer has a CHANGES_REQUESTED
// review whose commit IS the current head. This — not the aggregate
// reviewDecision — is the actionable review signal: a CHANGES_REQUESTED sticks in
// reviewDecision until the reviewer approves, so after we push a fix the reviewer
// hasn't re-reviewed yet, reviewDecision stays CHANGES_REQUESTED but there is
// nothing new to do (we already responded and are WAITING for the re-review).
// Keying on "changes requested AT the current head" stops the wasteful spin of
// re-fixing a stale review round.
func changesRequestedOnCurrentHead(pr githubinfra.PullRequestDetail) bool {
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		return false
	}
	for _, review := range pr.Reviews {
		if !strings.EqualFold(strings.TrimSpace(stringFromAny(review["state"])), "CHANGES_REQUESTED") {
			continue
		}
		commit, _ := review["commit"].(map[string]any)
		if commit != nil && strings.TrimSpace(stringFromAny(commit["oid"])) == head {
			return true
		}
	}
	return false
}

// commentedOnCurrentHead reports whether a reviewer left a COMMENTED review whose
// commit IS the current head. A COMMENTED review is not a formal CHANGES_REQUESTED,
// but it routinely carries real feedback (a non-blocking concern the author agreed
// to, an inline suggestion) that must not be silently ignored — the shepherd should
// read the review threads and address or answer them. Keyed on the current head like
// changesRequestedOnCurrentHead so a stale COMMENTED round on an old head does not
// re-trigger; combined with foldShepherdSignal's LastSignal dedup (a given review
// round is folded in once) and the fact that a pushed fix moves the head off this
// review, this cannot spin.
func commentedOnCurrentHead(pr githubinfra.PullRequestDetail) bool {
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		return false
	}
	for _, review := range pr.Reviews {
		if !strings.EqualFold(strings.TrimSpace(stringFromAny(review["state"])), "COMMENTED") {
			continue
		}
		commit, _ := review["commit"].(map[string]any)
		if commit != nil && strings.TrimSpace(stringFromAny(commit["oid"])) == head {
			return true
		}
	}
	return false
}

// approvedOnCurrentHead reports whether a reviewer APPROVED the current head.
// Like changesRequestedOnCurrentHead, this beats the aggregate reviewDecision: a
// stale CHANGES_REQUESTED from a reviewer who only saw an OLD head keeps the
// aggregate at CHANGES_REQUESTED even after fresh approvals of the current head,
// which would otherwise wedge the card at "reviewing" forever.
func approvedOnCurrentHead(pr githubinfra.PullRequestDetail) bool {
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		return false
	}
	for _, review := range pr.Reviews {
		if !strings.EqualFold(strings.TrimSpace(stringFromAny(review["state"])), "APPROVED") {
			continue
		}
		commit, _ := review["commit"].(map[string]any)
		if commit != nil && strings.TrimSpace(stringFromAny(commit["oid"])) == head {
			return true
		}
	}
	return false
}

// prHasLabel reports whether the PR carries the given label (case-insensitive).
func prHasLabel(pr githubinfra.PullRequestDetail, label string) bool {
	for _, l := range pr.Labels {
		if strings.EqualFold(strings.TrimSpace(l), label) {
			return true
		}
	}
	return false
}

// prNeedsValidation / prValidated read the QA validation-gate labels a maintainer
// bot manages on the PR: `needs-validation` (runtime change needs QA sign-off) and
// `validated` (QA passed). The shepherd holds at awaiting_validation while
// needs-validation is present and validated is not.
func prNeedsValidation(pr githubinfra.PullRequestDetail) bool {
	return prHasLabel(pr, "needs-validation")
}
func prValidated(pr githubinfra.PullRequestDetail) bool { return prHasLabel(pr, "validated") }

// shepherdActionable reports whether the agent has something to do THIS wake:
// changes requested AT THE CURRENT HEAD, failing CI, or a conflict. Approved (or
// a fix already pushed and awaiting the reviewer's re-review) is NOT actionable —
// the bot waits; it never merges.
func shepherdActionable(pr githubinfra.PullRequestDetail) bool {
	if pr.HasConflicts {
		return true
	}
	if shepherdCIPhase(pr) == "failed" {
		return true
	}
	return changesRequestedOnCurrentHead(pr)
}

// shepherdPhaseFor maps PR state to the card phase. awaiting_merge keys on the
// CURRENT head's review state (approved here + no outstanding change request
// here), not the aggregate reviewDecision — a stale CHANGES_REQUESTED on an old
// head must not block "待合并" once the current head is approved and green. The
// bot never merges; a human does.
func shepherdPhaseFor(pr githubinfra.PullRequestDetail) string {
	if shepherdActionable(pr) {
		return "fixing"
	}
	if approvedOnCurrentHead(pr) && !changesRequestedOnCurrentHead(pr) && shepherdCIPhase(pr) == "passed" && !pr.HasConflicts {
		// Approved + green + clean. Hold at awaiting_validation (→ @QA) while the PR
		// still needs QA sign-off; only once validated (or it never needed it) is it
		// awaiting_merge (→ @owner). The bot never merges; a human does.
		if prNeedsValidation(pr) && !prValidated(pr) {
			return "awaiting_validation"
		}
		return "awaiting_merge"
	}
	return "reviewing"
}

// syncShepherdPhase updates the durable card phase from the current PR snapshot.
// It intentionally runs independently of the folded wake signal: an agent pass
// temporarily stamps "fixing", so phase can drift even while GitHub state remains
// unchanged. Returns true only when the persisted phase needs a refresh.
func syncShepherdPhase(marker *loops.Shepherd, pr githubinfra.PullRequestDetail) bool {
	if marker == nil {
		return false
	}
	desired := shepherdPhaseFor(pr)
	changed := marker.Phase != desired
	marker.Phase = desired
	marker.HeadSHA = pr.HeadSHA
	return changed
}

// refreshShepherdLock keeps the pr:<repo>:<n> lock alive under the stable
// shepherd:<loopID> owner so a same-account fixer's discovery keeps skipping.
func (r *Runtime) refreshShepherdLock(ctx context.Context, repos *storage.Repositories, loopID, repo string, prNumber int64, now func() time.Time) {
	if repos.Locks == nil {
		return
	}
	key := fmt.Sprintf("pr:%s:%d", repo, prNumber)
	owner := "shepherd:" + loopID
	reason := "shepherd"
	nowISO := formatJavaScriptISOString(now().UTC())
	expiresAt := formatJavaScriptISOString(now().UTC().Add(shepherdReconcileLockTTL))
	if refreshed, err := repos.Locks.Refresh(ctx, storage.LockRecord{Key: key, Owner: owner, Reason: &reason, ExpiresAt: expiresAt, UpdatedAt: nowISO}); err == nil && refreshed {
		return
	}
	_, _ = repos.Locks.Acquire(ctx, storage.LockRecord{Key: key, Owner: owner, Reason: &reason, ExpiresAt: expiresAt, CreatedAt: nowISO, UpdatedAt: nowISO})
}

// terminateShepherd finishes a shepherding loop once its PR merged or closed:
// clears the marker, releases the coordination lock, and sets loop completed.
func (r *Runtime) terminateShepherd(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, repo string, prNumber int64, outcome, nowISO string, logger interface {
	Info(string, map[string]any)
}) {
	marker, _ := loops.ReadShepherd(loop.MetadataJSON)
	marker.Active = false
	marker.Outcome = outcome
	marker.Phase = "merged"
	newMeta, err := loops.WriteShepherd(loop.MetadataJSON, marker)
	if err != nil {
		return
	}
	fresh, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	updated := *fresh
	updated.Status = string(domain.LoopStatusCompleted)
	updated.MetadataJSON = &newMeta
	updated.NextRunAt = nil
	updated.LastRunAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return
	}
	// A human merged (or closed) the PR — advance the anchor card to its terminal
	// state (✅ 已合并) instead of leaving it frozen mid-shepherd, and post a closing
	// milestone note so the thread reads end-to-end.
	r.refreshShepherdCard(ctx, loop.ID)
	if strings.EqualFold(strings.TrimSpace(outcome), "merged") {
		r.postShepherdThreadNote(ctx, loop.ID, fmt.Sprintf("🎉 PR #%d 已合并。", prNumber), nil)
		// Reflect the merge in Plane's own state column: In Review → Done. A failed
		// best-effort write is recorded in the marker and retried by the reconciler.
		r.reconcileCompletedShepherdPlaneState(ctx, repos, updated, nowISO)
	}
	if repos.Locks != nil {
		_ = repos.Locks.Release(ctx, fmt.Sprintf("pr:%s:%d", repo, prNumber))
	}
	if logger != nil {
		logger.Info("shepherd: PR reached terminal state", map[string]any{"loopId": loop.ID, "repo": repo, "prNumber": prNumber, "outcome": outcome})
	}
}

// persistLoopMetadata writes updated $.shepherd metadata without changing status.
func (r *Runtime) persistLoopMetadata(ctx context.Context, repos *storage.Repositories, loopID, metadataJSON, nowISO string) {
	fresh, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || fresh == nil {
		return
	}
	updated := *fresh
	updated.MetadataJSON = &metadataJSON
	updated.UpdatedAt = nowISO
	_ = repos.Loops.Upsert(ctx, updated)
}

// refreshShepherdCard re-renders a shepherded loop's Feishu anchor card so it
// reflects the loop's CURRENT shepherd phase (👀 评审中 / 🔧 修复中 / ✅ 待合并, or ✅
// 已合并 on terminate). The scheduler only refreshes the anchor on a run start/finish;
// a passively-shepherded PR (waiting on a human review or merge) runs no pass, so
// without this the card would freeze at the worker's "🔨 实现中". App-mode only
// (RefreshThreadHeader no-ops otherwise). The gateway is built on demand — this is
// called only when a phase actually changes, which is rare.
func (r *Runtime) refreshShepherdCard(ctx context.Context, loopID string) {
	if strings.TrimSpace(loopID) == "" {
		return
	}
	if gw, ok := r.shepherdNotifyGateway(); ok {
		gw.RefreshThreadHeader(ctx, loopID, nil, 0)
	}
}

// postShepherdThreadNote posts a milestone note into the loop's Feishu thread,
// @-mentioning the given open_ids (resolved per-project from config, never hardcoded).
// App-mode only; best-effort.
func (r *Runtime) postShepherdThreadNote(ctx context.Context, loopID, text string, mentionOpenIDs []string) {
	if strings.TrimSpace(loopID) == "" || strings.TrimSpace(text) == "" {
		return
	}
	if gw, ok := r.shepherdNotifyGateway(); ok {
		_ = gw.PostThreadNote(ctx, loopID, text, mentionOpenIDs)
	}
}

// shepherdNotifyGateway builds a notify gateway for the shepherd's card refreshes and
// milestone thread notes. Returns (nil,false) unless notifications are in app mode.
// Built on demand — the shepherd only notifies on a phase change, which is rare.
func (r *Runtime) shepherdNotifyGateway() (*notify.Gateway, bool) {
	r.mu.RLock()
	cfg := r.config
	repos := r.services.Repositories
	now := r.now
	r.mu.RUnlock()
	if repos == nil || !strings.EqualFold(strings.TrimSpace(cfg.Notifications.Webhook.Mode), "app") {
		return nil, false
	}
	if now == nil {
		now = time.Now
	}
	return notify.NewGateway(notify.Options{
		Config:        cfg.Notifications,
		OsascriptPath: derefString(cfg.Tools.OsascriptPath),
		LogFilePath:   filepath.Join(cfg.Daemon.LogDir, "looperd.log"),
		Repositories:  repos,
		Now:           now,
		ResolveOwnerOpenID: func(projectID string) string {
			return config.ProjectOwner(cfg, projectID)
		},
	}), true
}

// reportShepherdPhase posts a milestone thread note when the loop ENTERS a
// report-worthy phase, exactly once per phase (deduped via marker.LastReportedPhase):
//   - awaiting_validation → @QA (needs QA sign-off)
//   - awaiting_merge      → @owner (ready to merge; the bot never merges)
//
// reviewing / fixing don't post — the anchor card + the worker's own thread feed
// already carry those. The mention open_ids come from per-project config.
func (r *Runtime) reportShepherdPhase(ctx context.Context, marker *loops.Shepherd, loop storage.LoopRecord, prNumber int64, validated bool) {
	phase := marker.Phase
	if phase == "" || phase == marker.LastReportedPhase {
		return
	}
	r.mu.RLock()
	cfg := r.config
	r.mu.RUnlock()
	var text string
	var mentions []string
	switch phase {
	case "awaiting_validation":
		text = fmt.Sprintf("🧪 PR #%d 已批准 + CI 全绿 + 无冲突,但带 `needs-validation` —— 请验收(通过后在 PR 上打 `validated` 标签)。", prNumber)
		if qa := strings.TrimSpace(config.ProjectQA(cfg, loop.ProjectID)); qa != "" {
			mentions = []string{qa}
		}
	case "awaiting_merge":
		suffix := ""
		if validated {
			suffix = " + 已通过验收"
		}
		text = fmt.Sprintf("✅ PR #%d 已就绪(批准 + CI 全绿 + 无冲突%s)—— 可以合并了。bot 不会自己合,等你点。", prNumber, suffix)
		if owner := strings.TrimSpace(config.ProjectOwner(cfg, loop.ProjectID)); owner != "" {
			mentions = []string{owner}
		}
	default:
		return
	}
	r.postShepherdThreadNote(ctx, loop.ID, text, mentions)
	marker.LastReportedPhase = phase
}

// enqueueShepherdPass wakes ONE shepherd pass: a worker queue item bound to the
// loop (deduped by loop id so an in-flight pass is not double-dispatched). The
// resolver forces stepShepherd via the durable marker. The LockKey is recorded
// but not acquired at claim (the shepherd pass manages the pr lock itself).
func (r *Runtime) enqueueShepherdPass(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, repo string, prNumber int64, nowISO string, logger interface {
	Info(string, map[string]any)
}) {
	lockKey := fmt.Sprintf("pr:%s:%d", repo, prNumber)
	projectID := loop.ProjectID
	record := storage.QueueItemRecord{
		ID:          eventlog.NewEventID("queue"),
		ProjectID:   &projectID,
		LoopID:      &loop.ID,
		Type:        "worker",
		TargetType:  string(domain.LoopTargetTypePullRequest),
		TargetID:    lockKey,
		Repo:        &repo,
		PRNumber:    &prNumber,
		DedupeKey:   fmt.Sprintf("worker:%s", loop.ID),
		Priority:    storage.QueuePriorityWorker,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: 3,
		LockKey:     &lockKey,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	_, created, err := repos.Queue.CreateOrGetActiveByDedupe(ctx, record)
	if err != nil {
		if logger != nil {
			logger.Info("shepherd: enqueue pass failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
		}
		return
	}
	if created {
		r.TriggerSchedulerClaim()
		if logger != nil {
			logger.Info("shepherd: woke a pass", map[string]any{"loopId": loop.ID, "repo": repo, "prNumber": prNumber})
		}
	}
}

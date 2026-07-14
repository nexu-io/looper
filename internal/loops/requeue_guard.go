package loops

import (
	"fmt"
	"strings"
	"sync"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

// loopRequeueGuards serializes per-loop queue rearm across the API discard/retry
// path and runtime free-text / HITL / recovery / discovery requeues. Without a
// process-wide mutex, concurrent requeue can land after API preflight and
// before git reset, wiping the worktree for a continuation that then loses the
// retry transaction to an active-queue conflict.
var loopRequeueGuards sync.Map // loopID -> *sync.Mutex

// loopTargetGuards serializes same worktree-target mutations across API
// discard/retry/start/create and runtime recovery/discovery/HITL requeues.
// Per-loop locks alone cannot block a *different* loop on the shared PR
// worktree from requeuing between discard preflight and git reset.
var loopTargetGuards sync.Map // targetGuardKey -> *sync.Mutex

// LockLoopRequeue acquires the process-wide per-loop requeue mutex shared by:
//   - API retry/start/reuse
//   - runtime HITL free-text / answer requeues
//   - runtime deferred/startup recovery requeues
//   - reviewer/fixer discovery enqueue for an existing loop
//
// Callers must unlock via the returned function (typically defer). Nested
// acquisitions on the same loop from the same goroutine deadlock — do not
// call requeue helpers while already holding this lock for that loopID.
// Call order with LockLoopTarget: take the per-loop lock first, then the
// target lock.
func LockLoopRequeue(loopID string) func() {
	value, _ := loopRequeueGuards.LoadOrStore(loopID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// LoopTargetGuardKey builds the process-wide target mutex key shared by API
// discard/retry/start/create and runtime recovery/discovery/HITL requeues.
//
// Empty string means no lock (concurrent project-scoped workers are exempt).
// Pull-request targets omit loop type so fixer/reviewer/worker share one key —
// they share the managed PR worktree (looper-fix-<project>-pr-N).
func LoopTargetGuardKey(projectID, loopType, targetType, targetKey string) string {
	projectID = strings.TrimSpace(projectID)
	loopType = strings.TrimSpace(loopType)
	targetType = strings.TrimSpace(targetType)
	targetKey = strings.TrimSpace(targetKey)
	if targetKey == "" {
		return ""
	}
	if loopType == string(domain.LoopTypeWorker) && targetType == string(domain.LoopTargetTypeProject) {
		return ""
	}
	if targetType == string(domain.LoopTargetTypePullRequest) {
		return fmt.Sprintf("%s|%s", projectID, targetKey)
	}
	return fmt.Sprintf("%s|%s|%s", projectID, loopType, targetKey)
}

// LoopTargetGuardKeyFromRecord derives LoopTargetGuardKey from a stored loop.
// Key formatting matches API loopTargetKeyFromRecordCompat / loopTargetKeyCompat
// so API and runtime share the same mutex entries.
func LoopTargetGuardKeyFromRecord(loop storage.LoopRecord) string {
	return LoopTargetGuardKey(loop.ProjectID, loop.Type, loop.TargetType, TargetKeyFromLoopRecord(loop))
}

// PullRequestTargetGuardKey builds the shared PR worktree target mutex key from
// project + repo + PR number (loop type omitted). Use when discovery creates or
// requeues a reviewer/fixer for a PR before a loop record is available.
func PullRequestTargetGuardKey(projectID, repo string, prNumber int64) string {
	projectID = strings.TrimSpace(projectID)
	repo = strings.TrimSpace(repo)
	if projectID == "" || repo == "" || prNumber <= 0 {
		return ""
	}
	return LoopTargetGuardKey(projectID, string(domain.LoopTypeReviewer), string(domain.LoopTargetTypePullRequest), fmt.Sprintf("pull_request:%s:%d", repo, prNumber))
}

// TargetKeyFromLoopRecord returns the canonical target key for a stored loop
// (project:/issue:/pull_request:...), matching API loopTargetKeyFromRecordCompat.
func TargetKeyFromLoopRecord(loop storage.LoopRecord) string {
	switch loop.TargetType {
	case string(domain.LoopTargetTypeProject):
		if loop.TargetID == nil {
			return "project:"
		}
		normalized := strings.TrimSpace(*loop.TargetID)
		for strings.HasPrefix(normalized, "project:") {
			normalized = strings.TrimPrefix(normalized, "project:")
		}
		return "project:" + normalized
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

// LockLoopTarget acquires the process-wide same-target mutex. An empty key is a
// no-op. Callers that also take LockLoopRequeue must acquire the per-loop lock
// first to avoid deadlocks with API discard+retry.
func LockLoopTarget(key string) func() {
	if strings.TrimSpace(key) == "" {
		return func() {}
	}
	value, _ := loopTargetGuards.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

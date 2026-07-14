package runtime

import "sync"

// loopRequeueGuards serializes per-loop queue rearm across the API discard/retry
// path and runtime free-text / HITL requeues. Without a process-wide mutex,
// Feishu/GitHub inbox delivery can call enqueueHumanMessageToLoop after API
// preflight and before git reset, wiping the worktree for a message-driven
// continuation that then loses the retry transaction to an active-queue conflict.
var loopRequeueGuards sync.Map // loopID -> *sync.Mutex

// LockLoopRequeue acquires the process-wide per-loop requeue mutex shared by:
//   - API retry/start/reuse (via Handler.lockLoopRetry)
//   - runtime HITL free-text enqueue (enqueueHumanMessageToLoop)
//   - runtime HITL answer requeue (deliverHITLAnswerToLoop)
//
// Callers must unlock via the returned function (typically defer). Nested
// acquisitions on the same loop from the same goroutine deadlock — do not
// call requeue helpers while already holding this lock for that loopID.
func LockLoopRequeue(loopID string) func() {
	value, _ := loopRequeueGuards.LoadOrStore(loopID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

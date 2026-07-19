package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

// ErrOperationAdmissionClosed is returned when an operation lease is refused
// because Supervisor admission is closed (daemon shutdown / degraded).
var ErrOperationAdmissionClosed = errors.New("queue operation admission is closed")

// ErrOperationLeaseCancelled is returned by BindClaim when stop/shutdown (or
// another Supervisor cancel) closed the lease before the durable claim can be
// owned. Context cancel alone is insufficient — callers must treat this
// explicit error as "do not start the queue processor".
var ErrOperationLeaseCancelled = errors.New("queue operation lease cancelled before bind")

// ErrOperationFinalizeFailed is returned when durable complete/cancel/requeue
// of a claimed queue item fails. Ownership must be retained and admission
// degraded rather than treating release as success (ADR-0015 R6 / #579).
var ErrOperationFinalizeFailed = errors.New("queue operation durable finalize failed")

// OperationMeta identifies one queue claim operation admitted before durable
// ClaimNext*. Loop/item identity is filled at BindClaim after a successful claim.
type OperationMeta struct {
	// ClaimedBy is the durable claimed_by value (e.g. "scheduler").
	ClaimedBy string
}

// OperationPermit is the explicit token proving BindClaim succeeded. The queue
// processor / agent spawn path must not start without a non-zero permit.
type OperationPermit struct {
	leaseID     uint64
	queueItemID string
}

// Valid reports whether this permit authorizes starting the queue processor.
func (p OperationPermit) Valid() bool {
	return p.leaseID != 0 && p.queueItemID != ""
}

// QueueItemID returns the durable queue item bound to this permit.
func (p OperationPermit) QueueItemID() string {
	return p.queueItemID
}

// OperationLease is the Supervisor ownership token for one durable queue claim
// from before ClaimNext* until durable complete / cancel / requeue (ADR-0015 R6).
type OperationLease interface {
	// Context is cancelled when stop/shutdown races the in-flight claim or run.
	// Cancellation alone must not be treated as a bind permit failure — use
	// BindClaim's explicit error.
	Context() context.Context
	// BindClaim binds a successful durable claim to this lease. Returns an
	// explicit OperationPermit only when the lease is still owned and live.
	// On ErrOperationLeaseCancelled the processor must never start; the claim
	// remains owned until durable finalize then Release.
	BindClaim(item storage.QueueItemRecord) (OperationPermit, error)
	// Release drops the lease. Call immediately on claim miss/error. After a
	// successful claim, call only once complete/cancel/requeue is durably
	// committed. Never call after a finalize persistence failure.
	Release()
	// Owns reports whether this lease currently owns queueItemID (bound, not released).
	Owns(queueItemID string) bool
}

// operationLease implements OperationLease under ActiveExecutionRegistry.
//
// Lock order when both are needed: registry.mu (r.mu) before lease.mu (l.mu).
// Release, BindClaim, OwnsQueueClaim, and stop/shutdown scans all follow this
// order so a finalize-path Release cannot deadlock with BeginLoopStop /
// BeginShutdown inspecting bound operations.
type operationLease struct {
	registry *ActiveExecutionRegistry
	id       uint64
	meta     OperationMeta
	ctx      context.Context
	cancel   context.CancelCauseFunc

	mu          sync.Mutex
	released    bool
	bound       bool
	queueItemID string
	loopID      string

	// pendingDone is closed when the lease leaves the pending (pre-bind) set —
	// either BindClaim succeeded or Release ran before bind. BeginShutdown waits
	// so stop cannot return while a claim is mid-bind without ownership decision.
	pendingDone     chan struct{}
	pendingDoneOnce sync.Once
}

func (l *operationLease) closePendingDone() {
	if l == nil {
		return
	}
	l.pendingDoneOnce.Do(func() {
		if l.pendingDone != nil {
			close(l.pendingDone)
		}
	})
}

func (l *operationLease) Context() context.Context {
	if l == nil || l.ctx == nil {
		return context.Background()
	}
	return l.ctx
}

func (l *operationLease) Owns(queueItemID string) bool {
	if l == nil || queueItemID == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.released && l.bound && l.queueItemID == queueItemID
}

func (l *operationLease) BindClaim(item storage.QueueItemRecord) (OperationPermit, error) {
	if l == nil {
		return OperationPermit{}, ErrOperationAdmissionClosed
	}
	r := l.registry
	if r == nil {
		return OperationPermit{}, ErrOperationAdmissionClosed
	}
	if item.ID == "" {
		return OperationPermit{}, errors.New("queue operation bind: queue item id is required")
	}

	r.mu.Lock()
	if l.released || r.pendingOps[l.id] != l {
		r.mu.Unlock()
		return OperationPermit{}, ErrOperationLeaseCancelled
	}
	// Explicit refuse: admission closed, loop stopping, or lease cancel cause.
	// Do not rely on context.Err alone — BindClaim must return a typed error
	// so callers never start a processor after stop/bind race.
	closing := r.admissionClosed
	loopID := ""
	if item.LoopID != nil {
		loopID = *item.LoopID
	}
	stopping := loopID != "" && r.stoppingLoops[loopID] > 0
	cancelled := l.ctx != nil && l.ctx.Err() != nil
	if closing || stopping || cancelled {
		delete(r.pendingOps, l.id)
		// Keep the lease in boundOps so the durable claim still has a live owner
		// until requeue/finalize + Release. Mark bound under the same lock.
		l.mu.Lock()
		l.bound = true
		l.queueItemID = item.ID
		l.loopID = loopID
		l.mu.Unlock()
		r.boundOps[l.id] = l
		r.boundByQueueItem[item.ID] = l.id
		r.mu.Unlock()
		l.closePendingDone()
		return OperationPermit{}, ErrOperationLeaseCancelled
	}

	l.mu.Lock()
	l.bound = true
	l.queueItemID = item.ID
	l.loopID = loopID
	l.mu.Unlock()
	r.boundOps[l.id] = l
	r.boundByQueueItem[item.ID] = l.id
	delete(r.pendingOps, l.id)
	r.mu.Unlock()
	l.closePendingDone()
	return OperationPermit{leaseID: l.id, queueItemID: item.ID}, nil
}

func (l *operationLease) Release() {
	if l == nil {
		return
	}
	r := l.registry
	if r == nil {
		l.mu.Lock()
		if l.released {
			l.mu.Unlock()
			return
		}
		l.released = true
		l.mu.Unlock()
		if l.cancel != nil {
			l.cancel(nil)
		}
		l.closePendingDone()
		return
	}

	// Lock order: registry.mu before lease.mu (must match BindClaim / stop scans).
	// Taking lease.mu first would ABBA-deadlock with cancelBoundOperationsLocked
	// and BeginLoopStop bound-op inspection, which hold r.mu then l.mu.
	r.mu.Lock()
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		r.mu.Unlock()
		return
	}
	l.released = true
	queueItemID := l.queueItemID
	l.mu.Unlock()
	delete(r.pendingOps, l.id)
	delete(r.boundOps, l.id)
	if queueItemID != "" {
		if ownerID, ok := r.boundByQueueItem[queueItemID]; ok && ownerID == l.id {
			delete(r.boundByQueueItem, queueItemID)
		}
	}
	r.mu.Unlock()

	if l.cancel != nil {
		l.cancel(nil)
	}
	l.closePendingDone()
}

// AdmitOperation acquires a Supervisor operation lease before durable ClaimNext*
// (ADR-0015 R6 / #579). Successful claim must BindClaim; miss/error releases
// immediately. Bound claims release only after durable finalize.
func (r *ActiveExecutionRegistry) AdmitOperation(ctx context.Context, meta OperationMeta) (OperationLease, error) {
	if r == nil {
		return nil, ErrOperationAdmissionClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	allow := r.allowSpawn
	closed := r.admissionClosed
	r.mu.Unlock()

	// Project the same admission Authority as spawns/claims (starting/stopping/degraded).
	if allow != nil {
		if err := allow(); err != nil {
			return nil, errors.Join(ErrOperationAdmissionClosed, err)
		}
	}
	if closed {
		return nil, ErrOperationAdmissionClosed
	}

	r.mu.Lock()
	if r.admissionClosed {
		r.mu.Unlock()
		return nil, ErrOperationAdmissionClosed
	}
	if r.pendingOps == nil {
		r.pendingOps = make(map[uint64]*operationLease)
	}
	if r.boundOps == nil {
		r.boundOps = make(map[uint64]*operationLease)
	}
	if r.boundByQueueItem == nil {
		r.boundByQueueItem = make(map[string]uint64)
	}
	r.nextOpLeaseID++
	id := r.nextOpLeaseID
	leaseCtx, cancel := context.WithCancelCause(ctx)
	lease := &operationLease{
		registry:    r,
		id:          id,
		meta:        meta,
		ctx:         leaseCtx,
		cancel:      cancel,
		pendingDone: make(chan struct{}),
	}
	r.pendingOps[id] = lease
	r.mu.Unlock()
	return lease, nil
}

// OwnsQueueClaim reports whether a durable queue item id is currently owned by
// a live operation lease (bound, not released).
func (r *ActiveExecutionRegistry) OwnsQueueClaim(queueItemID string) bool {
	if r == nil || queueItemID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.boundByQueueItem[queueItemID]
	if !ok {
		return false
	}
	lease := r.boundOps[id]
	if lease == nil {
		return false
	}
	lease.mu.Lock()
	owned := !lease.released && lease.bound && lease.queueItemID == queueItemID
	lease.mu.Unlock()
	return owned
}

// BoundOperationCount returns the number of bound (post-claim) operation leases.
func (r *ActiveExecutionRegistry) BoundOperationCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.boundOps)
}

// PendingOperationCount returns the number of pre-claim/pre-bind operation leases.
func (r *ActiveExecutionRegistry) PendingOperationCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pendingOps)
}

// cancelPendingOperationsLocked cancels pending (unbound) operation leases.
// Caller must hold r.mu. Returns wait channels for pendingDone.
func (r *ActiveExecutionRegistry) cancelPendingOperationsLocked(cause error) []<-chan struct{} {
	if r == nil {
		return nil
	}
	wait := make([]<-chan struct{}, 0, len(r.pendingOps))
	for _, lease := range r.pendingOps {
		if lease == nil {
			continue
		}
		if cause != nil {
			lease.cancel(cause)
		}
		if lease.pendingDone != nil {
			wait = append(wait, lease.pendingDone)
		}
	}
	return wait
}

// cancelBoundOperationsLocked cancels bound operation lease contexts (stop signal)
// but does not release them — finalize-before-release still applies.
func (r *ActiveExecutionRegistry) cancelBoundOperationsLocked(cause error, loopID string) {
	if r == nil || cause == nil {
		return
	}
	for _, lease := range r.boundOps {
		if lease == nil {
			continue
		}
		if loopID != "" {
			lease.mu.Lock()
			match := lease.loopID == loopID
			lease.mu.Unlock()
			if !match {
				continue
			}
		}
		lease.cancel(cause)
	}
}

// waitPendingOperations waits for pending operation bind/release windows.
func (r *ActiveExecutionRegistry) waitPendingOperations(wait []<-chan struct{}, budget time.Duration) error {
	var waitErr error
	for _, done := range wait {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(budget):
			waitErr = errors.Join(waitErr, errShutdownDrainTimeout)
		}
	}
	return waitErr
}

// waitBoundOperations waits for bound operation leases to Release after durable
// finalize (shutdown finalizer drain). Does not force-release on timeout.
func (r *ActiveExecutionRegistry) waitBoundOperations(budget time.Duration) error {
	if r == nil {
		return nil
	}
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		remaining := len(r.boundOps)
		r.mu.Unlock()
		if remaining == 0 {
			return nil
		}
		select {
		case <-deadline.C:
			return errShutdownDrainTimeout
		case <-time.After(5 * time.Millisecond):
		}
	}
}

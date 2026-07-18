package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/processcontainment"
)

// ErrAgentLiveHandleMissing is returned by stop paths when an in-scope agent
// execution has no live Supervisor handle. After #576 full agent coverage,
// live stop/kill must not reconstruct ownership from SQLite PID.
var ErrAgentLiveHandleMissing = errors.New("agent live containment handle is missing")

type activeExecution interface {
	Kill(string) error
}

type ownedExecution struct {
	loopID      string
	runID       string
	executionID string
	// softKill notifies the agent execution status path (async killCh).
	softKill agent.SoftKillFunc
	// handle is the process containment Authority for confirmed drain.
	handle *processcontainment.Handle
}

// ActiveExecutionRegistry is the in-process Supervisor registry for live agent
// executions (ADR-0015 R3 / #576). It owns:
//
//   - spawn admission leases before cmd.Start
//   - containment handle binding after spawn
//   - stop/shutdown race linearization (kill+confirmed drain before Start success)
//   - Kill via bound handle for looper stop / haltLoop
//
// This is intentionally narrower than the #572 draft ExecutionSupervisor
// (queue reservations, persistence Authority, etc.). Those land in later slices.
type ActiveExecutionRegistry struct {
	mu sync.Mutex

	executions    map[string]*ownedExecution
	pending       map[uint64]*spawnLease
	stoppingLoops map[string]int
	nextLeaseID   uint64

	admissionClosed bool
	shutdownReason  string

	// allowSpawn, when set, projects daemon Admission.AllowClaim so spawns
	// refuse while starting/stopping/degraded. Nil means registry-local only
	// (tests that do not wire Runtime admission).
	allowSpawn func() error

	// killTimeout bounds handle.Kill during stop. Zero uses defaultKillTimeout.
	killTimeout time.Duration
}

const defaultKillTimeout = 20 * time.Second

func NewActiveExecutionRegistry() *ActiveExecutionRegistry {
	return &ActiveExecutionRegistry{
		executions:    make(map[string]*ownedExecution),
		pending:       make(map[uint64]*spawnLease),
		stoppingLoops: make(map[string]int),
	}
}

// SetAllowSpawn wires the daemon Admission projection for spawn decisions.
func (r *ActiveExecutionRegistry) SetAllowSpawn(fn func() error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.allowSpawn = fn
	r.mu.Unlock()
}

// spawnLease implements agent.SpawnLease.
type spawnLease struct {
	registry *ActiveExecutionRegistry
	id       uint64
	meta     agent.SpawnMeta
	ctx      context.Context
	cancel   context.CancelCauseFunc

	mu       sync.Mutex
	released bool
	handle   *processcontainment.Handle
	softKill agent.SoftKillFunc
}

func (l *spawnLease) Context() context.Context {
	if l == nil || l.ctx == nil {
		return context.Background()
	}
	return l.ctx
}

func (l *spawnLease) BindHandle(handle *processcontainment.Handle, softKill agent.SoftKillFunc) error {
	if l == nil {
		return agent.ErrSpawnAdmissionClosed
	}
	if handle == nil {
		return errors.New("agent spawn: containment handle is required")
	}
	r := l.registry
	if r == nil {
		return agent.ErrSpawnAdmissionClosed
	}

	r.mu.Lock()
	if l.released || r.pending[l.id] != l {
		r.mu.Unlock()
		return l.killUnowned(handle, agent.ErrSpawnAdmissionClosed)
	}
	closing := r.admissionClosed
	stopping := r.stoppingLoops[l.meta.LoopID] > 0 && l.meta.LoopID != ""
	reason := r.shutdownReason
	if reason == "" {
		if stopping {
			reason = agent.ErrSpawnLoopStopping.Error()
		} else {
			reason = agent.ErrSpawnAdmissionClosed.Error()
		}
	}
	if closing || stopping {
		// Linearize stop-vs-bind: drop pending, kill+drain before Start returns success.
		delete(r.pending, l.id)
		r.mu.Unlock()
		l.cancel(errors.New(reason))
		return l.killUnowned(handle, agent.ErrSpawnStoppedDuringBind)
	}

	key := activeExecutionKey(l.meta.LoopID, l.meta.RunID, l.meta.ExecutionID)
	entry := &ownedExecution{
		loopID:      l.meta.LoopID,
		runID:       l.meta.RunID,
		executionID: l.meta.ExecutionID,
		handle:      handle,
		softKill:    softKill,
	}
	l.mu.Lock()
	l.handle = handle
	l.softKill = softKill
	l.mu.Unlock()
	r.executions[key] = entry
	delete(r.pending, l.id)
	r.mu.Unlock()
	return nil
}

// RebindHandle replaces the live containment handle after native-resume
// fallback starts a second process. The prior handle must already have been
// waited/reaped by the executor run loop.
func (l *spawnLease) RebindHandle(handle *processcontainment.Handle, softKill agent.SoftKillFunc) error {
	if l == nil {
		return agent.ErrSpawnAdmissionClosed
	}
	if handle == nil {
		return errors.New("agent spawn: containment handle is required")
	}
	r := l.registry
	if r == nil {
		return agent.ErrSpawnAdmissionClosed
	}
	r.mu.Lock()
	if l.released {
		r.mu.Unlock()
		return l.killUnowned(handle, agent.ErrSpawnAdmissionClosed)
	}
	closing := r.admissionClosed
	stopping := r.stoppingLoops[l.meta.LoopID] > 0 && l.meta.LoopID != ""
	if closing || stopping {
		r.mu.Unlock()
		l.cancel(agent.ErrSpawnStoppedDuringBind)
		return l.killUnowned(handle, agent.ErrSpawnStoppedDuringBind)
	}
	key := activeExecutionKey(l.meta.LoopID, l.meta.RunID, l.meta.ExecutionID)
	entry, ok := r.executions[key]
	if !ok {
		entry = &ownedExecution{
			loopID:      l.meta.LoopID,
			runID:       l.meta.RunID,
			executionID: l.meta.ExecutionID,
		}
		r.executions[key] = entry
	}
	entry.handle = handle
	if softKill != nil {
		entry.softKill = softKill
	}
	l.mu.Lock()
	l.handle = handle
	if softKill != nil {
		l.softKill = softKill
	}
	l.mu.Unlock()
	r.mu.Unlock()
	return nil
}

func (l *spawnLease) killUnowned(handle *processcontainment.Handle, base error) error {
	if l == nil || l.registry == nil {
		ctx, cancel := context.WithTimeout(context.Background(), defaultKillTimeout)
		defer cancel()
		if err := handle.Kill(ctx); err != nil {
			return errors.Join(base, err)
		}
		return base
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.registry.killBudget())
	defer cancel()
	killErr := handle.Kill(ctx)
	if killErr != nil {
		return errors.Join(base, killErr)
	}
	return base
}

func (l *spawnLease) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return
	}
	l.released = true
	l.mu.Unlock()
	l.cancel(nil)
	r := l.registry
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.pending, l.id)
	key := activeExecutionKey(l.meta.LoopID, l.meta.RunID, l.meta.ExecutionID)
	if entry, ok := r.executions[key]; ok {
		// Only drop if this lease still owns the entry (handle identity).
		l.mu.Lock()
		handle := l.handle
		l.mu.Unlock()
		if entry.handle == handle || (entry.handle == nil && handle == nil) {
			delete(r.executions, key)
		}
	}
	r.mu.Unlock()
}

// AdmitSpawn acquires a Supervisor spawn lease before cmd.Start (ADR-0015 / #576).
func (r *ActiveExecutionRegistry) AdmitSpawn(ctx context.Context, meta agent.SpawnMeta) (agent.SpawnLease, error) {
	if r == nil {
		return nil, agent.ErrSpawnAdmissionClosed
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
	stopping := meta.LoopID != "" && r.stoppingLoops[meta.LoopID] > 0
	r.mu.Unlock()

	if allow != nil {
		if err := allow(); err != nil {
			return nil, errors.Join(agent.ErrSpawnAdmissionClosed, err)
		}
	}
	if closed {
		return nil, agent.ErrSpawnAdmissionClosed
	}
	if stopping {
		return nil, agent.ErrSpawnLoopStopping
	}

	r.mu.Lock()
	// Re-check under lock after allowSpawn (which may have raced with shutdown).
	if r.admissionClosed {
		r.mu.Unlock()
		return nil, agent.ErrSpawnAdmissionClosed
	}
	if meta.LoopID != "" && r.stoppingLoops[meta.LoopID] > 0 {
		r.mu.Unlock()
		return nil, agent.ErrSpawnLoopStopping
	}
	r.nextLeaseID++
	id := r.nextLeaseID
	leaseCtx, cancel := context.WithCancelCause(ctx)
	lease := &spawnLease{
		registry: r,
		id:       id,
		meta:     meta,
		ctx:      leaseCtx,
		cancel:   cancel,
	}
	r.pending[id] = lease
	r.mu.Unlock()
	return lease, nil
}

// BeginLoopStop closes spawn admission for one loop and cancels pending leases
// for that loop. Registered live executions are Kill'd by the caller (haltLoop).
// The returned release reopens loop admission after the durable stop transition.
func (r *ActiveExecutionRegistry) BeginLoopStop(loopID, reason string) func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	r.stoppingLoops[loopID]++
	pending := make([]*spawnLease, 0)
	for _, lease := range r.pending {
		if lease.meta.LoopID == loopID {
			pending = append(pending, lease)
		}
	}
	r.mu.Unlock()
	cause := errors.New(reason)
	if reason == "" {
		cause = agent.ErrSpawnLoopStopping
	}
	for _, lease := range pending {
		lease.cancel(cause)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.stoppingLoops[loopID] <= 1 {
				delete(r.stoppingLoops, loopID)
			} else {
				r.stoppingLoops[loopID]--
			}
			r.mu.Unlock()
		})
	}
}

// BeginShutdown closes spawn admission, cancels pending leases, and confirmed-
// drains every bound containment handle.
func (r *ActiveExecutionRegistry) BeginShutdown(reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.admissionClosed = true
	if reason != "" {
		r.shutdownReason = reason
	}
	pending := make([]*spawnLease, 0, len(r.pending))
	for _, lease := range r.pending {
		pending = append(pending, lease)
	}
	entries := make([]*ownedExecution, 0, len(r.executions))
	for _, entry := range r.executions {
		entries = append(entries, entry)
	}
	r.mu.Unlock()

	cause := errors.New(reason)
	if reason == "" {
		cause = agent.ErrSpawnAdmissionClosed
	}
	for _, lease := range pending {
		lease.cancel(cause)
	}
	for _, entry := range entries {
		_ = r.killOwned(entry, reason)
	}
}

// Register is retained for tests and transitional paths that hold an
// activeExecution without a containment handle. Production agent spawns must
// use AdmitSpawn + BindHandle at the common executor boundary (#576).
// A contract test fails if only the worker role registers post-spawn.
func (r *ActiveExecutionRegistry) Register(loopID, runID, executionID string, execution activeExecution) func() {
	if r == nil || execution == nil {
		return func() {}
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	if r.admissionClosed || (loopID != "" && r.stoppingLoops[loopID] > 0) {
		reason := r.shutdownReason
		if reason == "" {
			reason = "execution admission is closed"
		}
		if loopID != "" && r.stoppingLoops[loopID] > 0 {
			reason = "loop is stopping"
		}
		r.mu.Unlock()
		_ = execution.Kill(reason)
		return func() {}
	}
	soft := agent.SoftKillFunc(execution.Kill)
	r.executions[key] = &ownedExecution{
		loopID:      loopID,
		runID:       runID,
		executionID: executionID,
		softKill:    soft,
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if entry, ok := r.executions[key]; ok && entry.softKill != nil {
			// Compare via pointer identity is unavailable for funcs; drop by key
			// only when still the soft-kill-only registration (no handle).
			if entry.handle == nil {
				delete(r.executions, key)
			}
		}
		r.mu.Unlock()
	}
}

// Kill stops a live owned agent by containment handle (confirmed drain) when
// bound, otherwise via softKill. Returns (false, nil) when no live ownership
// entry exists — callers must not fall back to SQLite PID after #576.
func (r *ActiveExecutionRegistry) Kill(loopID, runID, executionID, reason string) (bool, error) {
	if r == nil {
		return false, nil
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	entry := r.executions[key]
	r.mu.Unlock()
	if entry == nil {
		return false, nil
	}
	return true, r.killOwned(entry, reason)
}

// HasLiveHandle reports whether the registry holds a live entry for the key.
// Used by stop paths and contract tests.
func (r *ActiveExecutionRegistry) HasLiveHandle(loopID, runID, executionID string) bool {
	if r == nil {
		return false
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.executions[key]
	return ok
}

// LiveCount returns the number of bound/registered live agent executions.
func (r *ActiveExecutionRegistry) LiveCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.executions)
}

// PendingCount returns the number of pre-Start spawn leases.
func (r *ActiveExecutionRegistry) PendingCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

func (r *ActiveExecutionRegistry) killOwned(entry *ownedExecution, reason string) error {
	if entry == nil {
		return nil
	}
	var softErr error
	if entry.softKill != nil {
		softErr = entry.softKill(reason)
	}
	if entry.handle != nil {
		ctx, cancel := context.WithTimeout(context.Background(), r.killBudget())
		defer cancel()
		if err := entry.handle.Kill(ctx); err != nil {
			return errors.Join(softErr, err)
		}
		return softErr
	}
	return softErr
}

func (r *ActiveExecutionRegistry) killBudget() time.Duration {
	if r == nil || r.killTimeout <= 0 {
		return defaultKillTimeout
	}
	return r.killTimeout
}

func activeExecutionKey(loopID, runID, executionID string) string {
	return loopID + "\x00" + runID + "\x00" + executionID
}

// Compile-time check: registry is the agent.SpawnOwner for daemon producers.
var _ agent.SpawnOwner = (*ActiveExecutionRegistry)(nil)

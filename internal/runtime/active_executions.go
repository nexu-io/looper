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

	executions map[string]*ownedExecution
	pending    map[uint64]*spawnLease
	// active holds leases after BindHandle succeeds until Release. Stop must
	// cancel these so native-resume fallback cannot re-spawn after drain.
	active        map[uint64]*spawnLease
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
		active:        make(map[uint64]*spawnLease),
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

	// spawnDone is closed when the lease leaves pending (BindHandle or Release).
	// BeginLoopStop/BeginShutdown wait on it so stop cannot return while a
	// process has been cmd.Start'd but is not yet bound/drained in the registry.
	spawnDone     chan struct{}
	spawnDoneOnce sync.Once

	// rebinding is true between BeginRebind and RebindHandle/AbortRebind.
	// BeginLoopStop/BeginShutdown wait on rebindDone so stop cannot return
	// while native-resume fallback has a live process not yet in the registry.
	rebinding  bool
	rebindDone chan struct{}
}

// closeSpawnDone marks the pending-spawn window finished. Safe to call multiple times.
func (l *spawnLease) closeSpawnDone() {
	if l == nil {
		return
	}
	l.spawnDoneOnce.Do(func() {
		if l.spawnDone != nil {
			close(l.spawnDone)
		}
	})
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
		// Close spawnDone only after confirmed drain so BeginLoopStop cannot
		// unblock while the just-started process is still live.
		delete(r.pending, l.id)
		r.mu.Unlock()
		l.cancel(errors.New(reason))
		err := l.killUnowned(handle, agent.ErrSpawnStoppedDuringBind)
		l.closeSpawnDone()
		return err
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
	// Keep the lease cancellable after bind: pending→active so loop stop can
	// cancel x.lease.Context() and block native-resume fallback re-spawn.
	r.active[l.id] = l
	delete(r.pending, l.id)
	l.closeSpawnDone()
	r.mu.Unlock()
	return nil
}

// BeginRebind admits a native-resume fallback re-spawn under the registry lock.
// Call before cmd.Start; pair with RebindHandle (after bind) or AbortRebind
// (start/bind failure). BeginLoopStop waits for in-flight rebind windows so
// stop cannot return while a second process is live outside the registry.
func (l *spawnLease) BeginRebind() error {
	if l == nil {
		return agent.ErrSpawnAdmissionClosed
	}
	r := l.registry
	if r == nil {
		return agent.ErrSpawnAdmissionClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.released {
		return agent.ErrSpawnAdmissionClosed
	}
	if l.rebinding {
		return errors.New("agent spawn: rebind already in progress")
	}
	closing := r.admissionClosed
	stopping := l.meta.LoopID != "" && r.stoppingLoops[l.meta.LoopID] > 0
	if closing {
		return agent.ErrSpawnAdmissionClosed
	}
	if stopping {
		return agent.ErrSpawnLoopStopping
	}
	if err := l.ctx.Err(); err != nil {
		return agent.ErrSpawnLoopStopping
	}
	l.rebinding = true
	l.rebindDone = make(chan struct{})
	return nil
}

// AbortRebind ends a BeginRebind window without publishing a new handle
// (cmd.Start / processcontainment.Bind failure). Safe after RebindHandle.
func (l *spawnLease) AbortRebind() {
	if l == nil || l.registry == nil {
		return
	}
	r := l.registry
	r.mu.Lock()
	l.endRebindLocked()
	r.mu.Unlock()
}

func (l *spawnLease) endRebindLocked() {
	if !l.rebinding {
		return
	}
	l.rebinding = false
	if l.rebindDone != nil {
		close(l.rebindDone)
		l.rebindDone = nil
	}
}

// RebindHandle replaces the live containment handle after native-resume
// fallback starts a second process. The prior handle must already have been
// waited/reaped by the executor run loop. Ends a BeginRebind window.
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
		l.endRebindLocked()
		r.mu.Unlock()
		return l.killUnowned(handle, agent.ErrSpawnAdmissionClosed)
	}
	closing := r.admissionClosed
	stopping := r.stoppingLoops[l.meta.LoopID] > 0 && l.meta.LoopID != ""
	if closing || stopping {
		l.endRebindLocked()
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
	l.endRebindLocked()
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
		l.closeSpawnDone()
		return
	}
	r.mu.Lock()
	l.endRebindLocked()
	delete(r.pending, l.id)
	delete(r.active, l.id)
	l.closeSpawnDone()
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
		registry:  r,
		id:        id,
		meta:      meta,
		ctx:       leaseCtx,
		cancel:    cancel,
		spawnDone: make(chan struct{}),
	}
	r.pending[id] = lease
	r.mu.Unlock()
	return lease, nil
}

// BeginLoopStop closes spawn admission for one loop, cancels both pending and
// bound (active) leases for that loop, and confirmed-drains every bound
// containment handle for the loop. Bound-lease cancel is required so
// native-resume fallback cannot re-spawn after the old handle is drained.
// Handle drain here covers the BindHandle→persistStatus window where the
// registry owns a live process but haltLoop may not yet see a durable
// AgentExecutionRecord to Kill by ID.
//
// Drain failures from processcontainment.Handle.Kill are returned so stop/close
// cannot report success when a just-bound agent is only unconfirmed or still
// live. The release func is still returned on drain failure: the gate was
// opened and callers manage sticky vs temporary windows as before.
//
// After a durable stop (pause/terminate), callers must keep the gate closed:
// do not invoke the returned release. In-flight runners that claimed work
// before stop may still reach AgentExecutor.Start after halt returns; reopening
// would let AdmitSpawn succeed and start a process after looper stop. Clear the
// gate only via ClearLoopStop when the loop is intentionally re-activated
// (API unpause/retry/handback). Do not clear from scheduler claim dispatch: a
// pre-stop claim can race past parked checks and would reopen admission.
//
// For terminal close abort paths (before durable terminate), callers should
// invoke the returned release so a still-running loop can AdmitSpawn again.
//
// Pending spawn windows (AdmitSpawn through BindHandle/Release) and native
// rebind windows are waited with the same handshake before stop returns, so a
// just-started process cannot outlive the stop response without confirmed drain.
//
// The returned release is also used in tests and temporary windows.
func (r *ActiveExecutionRegistry) BeginLoopStop(loopID, reason string) (release func(), err error) {
	if r == nil {
		return func() {}, nil
	}
	r.mu.Lock()
	r.stoppingLoops[loopID]++
	toCancel := make([]*spawnLease, 0)
	// spawnWait covers Start→BindHandle for still-pending leases.
	// rebindWait covers native-resume fallback Start→RebindHandle.
	spawnWait := make([]<-chan struct{}, 0)
	rebindWait := make([]<-chan struct{}, 0)
	for _, lease := range r.pending {
		if lease.meta.LoopID == loopID {
			toCancel = append(toCancel, lease)
			if lease.spawnDone != nil {
				spawnWait = append(spawnWait, lease.spawnDone)
			}
			if lease.rebinding && lease.rebindDone != nil {
				rebindWait = append(rebindWait, lease.rebindDone)
			}
		}
	}
	for _, lease := range r.active {
		if lease.meta.LoopID == loopID {
			toCancel = append(toCancel, lease)
			if lease.rebinding && lease.rebindDone != nil {
				rebindWait = append(rebindWait, lease.rebindDone)
			}
		}
	}
	// Only drain entries with a containment handle. SoftKill-only Register
	// stubs (tests / transitional) stay for haltLoop Kill-by-id, which still
	// consults durable execution status and must not half-kill stale rows.
	toKill := make([]*ownedExecution, 0)
	for _, entry := range r.executions {
		if entry != nil && entry.loopID == loopID && entry.handle != nil {
			toKill = append(toKill, entry)
		}
	}
	r.mu.Unlock()
	cause := errors.New(reason)
	if reason == "" {
		cause = agent.ErrSpawnLoopStopping
	}
	for _, lease := range toCancel {
		lease.cancel(cause)
	}
	// Confirmed-drain bound handles for this loop so stop does not return while
	// a post-BindHandle process is only asynchronously killed via lease cancel
	// after Start continues (BindHandle→persistStatus window). Propagate kill
	// failures: this may be the only path that confirms the process is dead
	// when no durable AgentExecutionRecord exists yet.
	var drainErr error
	for _, entry := range toKill {
		if killErr := r.killOwned(entry, reason); killErr != nil {
			drainErr = errors.Join(drainErr, killErr)
		}
	}
	// Wait for pending Start→BindHandle and rebind windows that began before we
	// set stopping. BindHandle/RebindHandle refuse+kill once they see the gate;
	// we must not return until those windows end and re-drain any published handle.
	budget := r.killBudget()
	waitChans := make([]<-chan struct{}, 0, len(spawnWait)+len(rebindWait))
	waitChans = append(waitChans, spawnWait...)
	waitChans = append(waitChans, rebindWait...)
	for _, done := range waitChans {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(budget):
		}
	}
	if len(waitChans) > 0 {
		r.mu.Lock()
		second := make([]*ownedExecution, 0)
		for _, entry := range r.executions {
			if entry != nil && entry.loopID == loopID && entry.handle != nil {
				second = append(second, entry)
			}
		}
		r.mu.Unlock()
		for _, entry := range second {
			if killErr := r.killOwned(entry, reason); killErr != nil {
				drainErr = errors.Join(drainErr, killErr)
			}
		}
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
	}, drainErr
}

// ClearLoopStop reopens spawn admission for a loop after intentional re-activation
// (API unpause, retry, or handback). Not for scheduler claim dispatch.
func (r *ActiveExecutionRegistry) ClearLoopStop(loopID string) {
	if r == nil || loopID == "" {
		return
	}
	r.mu.Lock()
	delete(r.stoppingLoops, loopID)
	r.mu.Unlock()
}

// RestoreLoopStop re-closes spawn admission after a failed intentional
// reactivation that already called ClearLoopStop. Does not cancel leases or
// drain handles — only restores the sticky AdmitSpawn gate.
func (r *ActiveExecutionRegistry) RestoreLoopStop(loopID string) {
	if r == nil || loopID == "" {
		return
	}
	r.mu.Lock()
	if r.stoppingLoops[loopID] == 0 {
		r.stoppingLoops[loopID] = 1
	}
	r.mu.Unlock()
}

// LoopStopActive reports whether spawn admission is closed for loopID.
func (r *ActiveExecutionRegistry) LoopStopActive(loopID string) bool {
	if r == nil || loopID == "" {
		return false
	}
	r.mu.Lock()
	active := r.stoppingLoops[loopID] > 0
	r.mu.Unlock()
	return active
}

// BeginShutdown closes spawn admission, cancels pending and bound (active)
// leases, and confirmed-drains every bound containment handle.
func (r *ActiveExecutionRegistry) BeginShutdown(reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.admissionClosed = true
	if reason != "" {
		r.shutdownReason = reason
	}
	toCancel := make([]*spawnLease, 0, len(r.pending)+len(r.active))
	spawnWait := make([]<-chan struct{}, 0)
	rebindWait := make([]<-chan struct{}, 0)
	for _, lease := range r.pending {
		toCancel = append(toCancel, lease)
		if lease.spawnDone != nil {
			spawnWait = append(spawnWait, lease.spawnDone)
		}
		if lease.rebinding && lease.rebindDone != nil {
			rebindWait = append(rebindWait, lease.rebindDone)
		}
	}
	for _, lease := range r.active {
		toCancel = append(toCancel, lease)
		if lease.rebinding && lease.rebindDone != nil {
			rebindWait = append(rebindWait, lease.rebindDone)
		}
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
	for _, lease := range toCancel {
		lease.cancel(cause)
	}
	for _, entry := range entries {
		_ = r.killOwned(entry, reason)
	}
	budget := r.killBudget()
	waitChans := make([]<-chan struct{}, 0, len(spawnWait)+len(rebindWait))
	waitChans = append(waitChans, spawnWait...)
	waitChans = append(waitChans, rebindWait...)
	for _, done := range waitChans {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(budget):
		}
	}
	if len(waitChans) > 0 {
		r.mu.Lock()
		second := make([]*ownedExecution, 0, len(r.executions))
		for _, entry := range r.executions {
			if entry != nil && entry.handle != nil {
				second = append(second, entry)
			}
		}
		r.mu.Unlock()
		for _, entry := range second {
			_ = r.killOwned(entry, reason)
		}
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

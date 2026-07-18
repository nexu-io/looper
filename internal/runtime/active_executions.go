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
	// stopEpoch is bumped by ClearLoopStop so outstanding BeginLoopStop release
	// closures captured before the clear become no-ops. Without this, a
	// temporary release (e.g. stopCandidateExecution) that outlived ClearLoopStop
	// could still decrement a later RestoreLoopStop sticky ref and reopen
	// AdmitSpawn for pre-stop runners.
	stopEpoch map[string]uint64
	// stopDrained records execution keys confirmed-drained by BeginLoopStop
	// (or Kill) while a loop stop gate is active. haltLoop looks up Kill by
	// durable id after BeginLoopStop; concurrent releaseLease can delete the
	// live entry during handle.Kill, so Kill must still report killed=true
	// when this stop already drained that key (avoids ErrAgentLiveHandleMissing
	// with a PID that the stop just killed).
	stopDrained map[string]struct{}
	nextLeaseID uint64

	admissionClosed bool
	shutdownReason  string

	// allowSpawn, when set, projects daemon Admission.AllowClaim so spawns
	// refuse while starting/stopping/degraded. Nil means registry-local only
	// (tests that do not wire Runtime admission).
	allowSpawn func() error

	// onHardPersistFailure maps hard agent_executions write failures into the
	// single sticky admission degraded state (ADR-0015 R5 / #578).
	onHardPersistFailure func(error)

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
		stopEpoch:     make(map[string]uint64),
		stopDrained:   make(map[string]struct{}),
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

// SetOnHardPersistFailure wires sticky admission degrade for hard execution
// observation write failures (initial/heartbeat/output/terminal). Soft cancel
// and conflict after terminal won must not invoke this callback.
func (r *ActiveExecutionRegistry) SetOnHardPersistFailure(fn func(error)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.onHardPersistFailure = fn
	r.mu.Unlock()
}

// ReportHardPersistFailure surfaces a hard agent_executions write failure into
// daemon admission. Safe to call from agent executor mid-life paths.
func (r *ActiveExecutionRegistry) ReportHardPersistFailure(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	fn := r.onHardPersistFailure
	r.mu.Unlock()
	if fn != nil {
		fn(err)
	}
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

	// unownedDrainErr is set when killUnowned fails for a refused BindHandle/
	// RebindHandle. That handle is never inserted into r.executions, so
	// cancelAndDrainLoop must join this error after the wait channel closes
	// (the second registry drain pass cannot see it).
	unownedDrainErr error
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
		// Keep rebindDone open across killUnowned: the refused fallback handle
		// is never inserted into r.executions, so BeginLoopStop/BeginShutdown
		// only wait on rebindDone (then re-drain registry handles). Closing the
		// wait channel before confirmed drain lets stop return while TERM/grace/KILL
		// is still in flight. Match BindHandle: signal done only after kill.
		r.mu.Unlock()
		err := l.killUnowned(handle, agent.ErrSpawnAdmissionClosed)
		r.mu.Lock()
		l.endRebindLocked()
		r.mu.Unlock()
		return err
	}
	closing := r.admissionClosed
	stopping := r.stoppingLoops[l.meta.LoopID] > 0 && l.meta.LoopID != ""
	if closing || stopping {
		r.mu.Unlock()
		l.cancel(agent.ErrSpawnStoppedDuringBind)
		err := l.killUnowned(handle, agent.ErrSpawnStoppedDuringBind)
		r.mu.Lock()
		l.endRebindLocked()
		r.mu.Unlock()
		return err
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
			l.publishUnownedDrainErr(err)
			return errors.Join(base, err)
		}
		return base
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.registry.killBudget())
	defer cancel()
	killErr := handle.Kill(ctx)
	if killErr != nil {
		// Publish before closeSpawnDone/endRebindLocked so cancelAndDrainLoop
		// can join this after the wait channel unblocks. Without this, stop
		// treats a closed wait channel as success while the only owner of the
		// just-started handle (killUnowned) already failed to confirm death.
		l.publishUnownedDrainErr(killErr)
		return errors.Join(base, killErr)
	}
	return base
}

// publishUnownedDrainErr records a failed refuse-path Handle.Kill for stop to
// join. Safe under concurrent BindHandle/RebindHandle and cancelAndDrainLoop.
func (l *spawnLease) publishUnownedDrainErr(err error) {
	if l == nil || err == nil {
		return
	}
	l.mu.Lock()
	l.unownedDrainErr = errors.Join(l.unownedDrainErr, err)
	l.mu.Unlock()
}

// takeUnownedDrainErr returns and clears any published refuse-path kill failure.
func (l *spawnLease) takeUnownedDrainErr() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	err := l.unownedDrainErr
	l.unownedDrainErr = nil
	l.mu.Unlock()
	return err
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

// errLoopStopWaitTimeout is returned when BeginLoopStop cannot confirm that a
// pending Start→BindHandle or native rebind window closed within killBudget.
// Without this, stop would report success while a just-started process may
// still be live outside the registry.
var errLoopStopWaitTimeout = errors.New("loop stop: pending spawn or rebind wait timed out")

// loopStopTargets holds cancel/drain work collected under the registry lock.
type loopStopTargets struct {
	toCancel   []*spawnLease
	spawnWait  []<-chan struct{}
	rebindWait []<-chan struct{}
	toKill     []*ownedExecution
}

// collectLoopStopTargetsLocked snapshots leases and bound handles for loopID.
// Caller must hold r.mu.
func (r *ActiveExecutionRegistry) collectLoopStopTargetsLocked(loopID string) loopStopTargets {
	var t loopStopTargets
	// spawnWait covers Start→BindHandle for still-pending leases.
	// rebindWait covers native-resume fallback Start→RebindHandle.
	for _, lease := range r.pending {
		if lease.meta.LoopID == loopID {
			t.toCancel = append(t.toCancel, lease)
			if lease.spawnDone != nil {
				t.spawnWait = append(t.spawnWait, lease.spawnDone)
			}
			if lease.rebinding && lease.rebindDone != nil {
				t.rebindWait = append(t.rebindWait, lease.rebindDone)
			}
		}
	}
	for _, lease := range r.active {
		if lease.meta.LoopID == loopID {
			t.toCancel = append(t.toCancel, lease)
			if lease.rebinding && lease.rebindDone != nil {
				t.rebindWait = append(t.rebindWait, lease.rebindDone)
			}
		}
	}
	// Only drain entries with a containment handle. SoftKill-only Register
	// stubs (tests / transitional) stay for haltLoop Kill-by-id, which still
	// consults durable execution status and must not half-kill stale rows.
	for _, entry := range r.executions {
		if entry != nil && entry.loopID == loopID && entry.handle != nil {
			t.toKill = append(t.toKill, entry)
		}
	}
	return t
}

// cancelAndDrainLoop cancels leases, confirmed-drains bound handles, and waits
// for pending spawn/rebind windows that began before the stop gate was set.
// BindHandle/RebindHandle refuse+kill once they see the gate; we must not
// return until those windows end (or time out) and re-drain any published handle.
func (r *ActiveExecutionRegistry) cancelAndDrainLoop(loopID, reason string, t loopStopTargets) error {
	cause := errors.New(reason)
	if reason == "" {
		cause = agent.ErrSpawnLoopStopping
	}
	for _, lease := range t.toCancel {
		lease.cancel(cause)
	}
	// Confirmed-drain bound handles for this loop so stop does not return while
	// a post-BindHandle process is only asynchronously killed via lease cancel
	// after Start continues (BindHandle→persistStatus window). Propagate kill
	// failures: this may be the only path that confirms the process is dead
	// when no durable AgentExecutionRecord exists yet.
	var drainErr error
	for _, entry := range t.toKill {
		killErr := r.killOwned(entry, reason)
		if killErr != nil {
			drainErr = errors.Join(drainErr, killErr)
		}
		r.rememberStopDrain(entry, killErr)
	}
	budget := r.killBudget()
	waitChans := make([]<-chan struct{}, 0, len(t.spawnWait)+len(t.rebindWait))
	waitChans = append(waitChans, t.spawnWait...)
	waitChans = append(waitChans, t.rebindWait...)
	for _, done := range waitChans {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(budget):
			// Surface timeout as drain failure: the confirmation channel that
			// proves no just-started process outlives stop never completed.
			drainErr = errors.Join(drainErr, errLoopStopWaitTimeout)
		}
	}
	// Join refuse-path killUnowned failures published on waited leases. Those
	// handles never enter r.executions, so the second drain pass cannot see
	// them; treating a closed wait channel as success would hide live agents.
	for _, lease := range t.toCancel {
		if err := lease.takeUnownedDrainErr(); err != nil {
			drainErr = errors.Join(drainErr, err)
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
			killErr := r.killOwned(entry, reason)
			if killErr != nil {
				drainErr = errors.Join(drainErr, killErr)
			}
			r.rememberStopDrain(entry, killErr)
		}
	}
	return drainErr
}

// BeginLoopStop closes spawn admission for one loop, cancels both pending and
// bound (active) leases for that loop, and confirmed-drains every bound
// containment handle for the loop. Bound-lease cancel is required so
// native-resume fallback cannot re-spawn after the old handle is drained.
// Handle drain here covers the BindHandle→persistStatus window where the
// registry owns a live process but haltLoop may not yet see a durable
// AgentExecutionRecord to Kill by ID.
//
// Successfully drained keys are recorded in stopDrained so a subsequent
// haltLoop Kill-by-id still returns killed=true when releaseLease removes the
// registry entry while handle.Kill is waiting (process exit → run finish).
//
// Drain failures from processcontainment.Handle.Kill and pending spawn/rebind
// wait timeouts are returned so stop/close cannot report success when a
// just-started agent is only unconfirmed or still live. The release func is
// still returned on drain failure: the gate was opened and callers manage
// sticky vs temporary windows as before.
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
	// Capture epoch so a later ClearLoopStop can invalidate this release without
	// needing to track each closure. Epoch is never reset; only bumped on clear.
	epoch := r.stopEpoch[loopID]
	targets := r.collectLoopStopTargetsLocked(loopID)
	r.mu.Unlock()
	drainErr := r.cancelAndDrainLoop(loopID, reason, targets)
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			// ClearLoopStop bumped the epoch: this release no longer owns a ref.
			if r.stopEpoch[loopID] != epoch {
				r.mu.Unlock()
				return
			}
			if r.stoppingLoops[loopID] <= 1 {
				delete(r.stoppingLoops, loopID)
				r.clearStopDrainedForLoopLocked(loopID)
			} else {
				r.stoppingLoops[loopID]--
			}
			r.mu.Unlock()
		})
	}, drainErr
}

// ClearLoopStop reopens spawn admission for a loop after intentional re-activation
// (API unpause, retry, or handback). Not for scheduler claim dispatch.
//
// Returns whether a stop gate was active under the same lock that clears it.
// Callers that restore on abort must use this return value instead of a separate
// LoopStopActive check: a concurrent BeginLoopStop between those two calls would
// leave gateWasActive=false while this delete still removes the new gate, and a
// failed start/retry/reuse TX would skip RestoreLoopStop.
//
// Outstanding BeginLoopStop release closures captured before this clear are
// invalidated via stopEpoch: deleting the refcount alone is not enough, because
// a temporary release (stopCandidateExecution) can still run after a failed
// reactivation's RestoreLoopStop and would otherwise drop the restored sticky
// gate when it sees count <= 1.
func (r *ActiveExecutionRegistry) ClearLoopStop(loopID string) (wasActive bool) {
	if r == nil || loopID == "" {
		return false
	}
	r.mu.Lock()
	wasActive = r.stoppingLoops[loopID] > 0
	delete(r.stoppingLoops, loopID)
	// Bump even when wasActive is false so a concurrent BeginLoopStop that lost
	// the race to this delete cannot leave a live release that still matches.
	r.stopEpoch[loopID]++
	r.clearStopDrainedForLoopLocked(loopID)
	r.mu.Unlock()
	return wasActive
}

// RestoreLoopStop re-closes spawn admission after a failed intentional
// reactivation that already called ClearLoopStop. Cancels pending/active
// leases and confirmed-drains bound handles admitted during the clear window
// so a failed retry/start/worker-reuse cannot leave a live agent for a loop
// that was never reactivated.
//
// Always increments the stop-gate refcount (sticky restore reference) even when
// a temporary BeginLoopStop is already active for the same loop. Leaving the
// count unchanged would let that temporary release reopen AdmitSpawn after the
// failed reactivation; restore must outlive unrelated releases.
//
// Returns cancelAndDrainLoop's error when kill or confirmed-drain fails so
// callers can join it with the original validation/TX failure; the gate is
// still closed even when drain fails.
func (r *ActiveExecutionRegistry) RestoreLoopStop(loopID string) error {
	if r == nil || loopID == "" {
		return nil
	}
	r.mu.Lock()
	// Sticky restore always owns a reference. Temporary BeginLoopStop (for
	// example stopCandidateExecution's kill window) may already hold a count;
	// incrementing ensures its deferred release cannot clear the restored gate.
	r.stoppingLoops[loopID]++
	targets := r.collectLoopStopTargetsLocked(loopID)
	r.mu.Unlock()
	// Gate is closed first; cancel+drain prevents orphan processes from the
	// clear window. Surface drain failure so callers do not only report the
	// original TX/validation error while a live agent may still be unconfirmed.
	return r.cancelAndDrainLoop(loopID, "restore loop stop after failed reactivation", targets)
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
//
// When BeginLoopStop already confirmed-drained this key and releaseLease removed
// the entry during handle.Kill, returns (true, nil) so haltLoop does not treat
// the missing entry as ErrAgentLiveHandleMissing for a process stop just killed.
func (r *ActiveExecutionRegistry) Kill(loopID, runID, executionID, reason string) (bool, error) {
	if r == nil {
		return false, nil
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	entry := r.executions[key]
	if entry == nil {
		_, drained := r.stopDrained[key]
		r.mu.Unlock()
		if drained {
			return true, nil
		}
		return false, nil
	}
	r.mu.Unlock()
	err := r.killOwned(entry, reason)
	r.rememberStopDrain(entry, err)
	return true, err
}

// rememberStopDrain records that stop already confirmed-drained this ownership
// key so a later Kill-by-id still returns killed=true after releaseLease. Safe
// when releaseLease concurrently deletes r.executions[key].
func (r *ActiveExecutionRegistry) rememberStopDrain(entry *ownedExecution, killErr error) {
	if r == nil || entry == nil {
		return
	}
	// Record only when kill succeeded, or the containment handle is already dead
	// (soft-kill error after confirmed handle drain must still count).
	if killErr != nil && (entry.handle == nil || !entry.handle.ConfirmedDead()) {
		return
	}
	key := activeExecutionKey(entry.loopID, entry.runID, entry.executionID)
	r.mu.Lock()
	if r.stopDrained == nil {
		r.stopDrained = make(map[string]struct{})
	}
	r.stopDrained[key] = struct{}{}
	r.mu.Unlock()
}

// clearStopDrainedForLoopLocked drops stopDrained keys for loopID. Caller holds r.mu.
func (r *ActiveExecutionRegistry) clearStopDrainedForLoopLocked(loopID string) {
	if r == nil || r.stopDrained == nil || loopID == "" {
		return
	}
	prefix := loopID + "\x00"
	for k := range r.stopDrained {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(r.stopDrained, k)
		}
	}
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

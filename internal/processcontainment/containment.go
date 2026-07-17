package processcontainment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	defaultGracePeriod  = 5 * time.Second
	defaultDrainTimeout = 15 * time.Second
	groupPollInterval   = 10 * time.Millisecond
)

// ErrNotConfirmedDead is returned when stop delivery completed without a
// confirmed non-runnable process group (timeout or unresolved descendants).
var ErrNotConfirmedDead = errors.New("process containment: not confirmed dead")

// SignalFunc delivers a signal to a pid (negative pid = process group).
// Injected in tests; production uses syscall.Kill.
type SignalFunc func(pid int, sig syscall.Signal) error

// Options configures grace/escalation and injects test seams.
type Options struct {
	// GracePeriod is how long to wait after SIGTERM before SIGKILL escalation.
	// Zero selects the package default (5s). Negative disables escalation wait
	// (immediate SIGKILL after TERM attempt).
	GracePeriod time.Duration
	// DrainTimeout bounds Kill/Drain until confirmed-dead or failure.
	// Zero selects the package default (15s).
	DrainTimeout time.Duration
	// Signal overrides process/group signaling (tests).
	Signal SignalFunc
	// Now overrides the clock (tests).
	Now func() time.Time
}

// DrainSnapshot is a point-in-time view of containment progress.
// Signal delivery fields alone never imply success.
type DrainSnapshot struct {
	LeaderPID     int
	PGID          int
	LeaderReaped  bool
	TermDelivered bool
	KillEscalated bool
	ConfirmedDead bool
	ExitCode      int
	WaitErr       error
}

// Handle owns process-group configuration, signal delivery, exactly-once wait,
// descendant drain, and confirmed-dead reporting for one spawned leader.
type Handle struct {
	cmd  *exec.Cmd
	pid  int
	pgid int

	gracePeriod  time.Duration
	drainTimeout time.Duration
	signalFn     SignalFunc
	now          func() time.Time

	armOnce  sync.Once
	waitOnce sync.Once
	waitCh   chan struct{}
	waitErr  error
	state    *os.ProcessState

	mu            sync.Mutex
	termDelivered bool
	killEscalated bool
	confirmedDead bool
	waitConsumed  bool

	// groupLive overrides groupHasNonZombieMember when non-nil (unit tests).
	// Production always leaves this nil so the platform /proc probe is used.
	groupLive func(pgid int) (hasLive bool, ok bool)
}

// Configure sets process-group isolation on cmd before Start.
// The child becomes the leader of a new process group (Setpgid).
// Shared by agent and shell producers when they migrate onto this handle.
//
// Inherited SysProcAttr fields are normalized: a non-zero Pgid would join an
// existing group instead of creating one, and Setsid combined with Setpgid can
// make cmd.Start fail. Clear both so Bind's pgid==pid invariant holds after Start.
func Configure(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	cmd.SysProcAttr.Setsid = false
}

// Bind attaches a Handle to an already-started command that was Configure'd
// (or otherwise started in its own process group). Does not treat the process
// as drained. Exactly-once wait is armed lazily on the first Wait, Kill, or
// Drain so callers can start StdoutPipe/StderrPipe readers before Wait closes
// those pipes.
//
// The bound process must be its process-group leader (pgid == pid). Binding a
// command that shares the caller's ambient group would make SignalGroup target
// -pgid against Looper and sibling processes.
func Bind(cmd *exec.Cmd, opts Options) (*Handle, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("process containment: started command with process is required")
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return nil, fmt.Errorf("process containment: invalid leader pid %d", pid)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return nil, fmt.Errorf("process containment: getpgid(%d): %w", pid, err)
	}
	if pgid != pid {
		return nil, fmt.Errorf("process containment: pid %d is not process group leader (pgid=%d); Configure before Start", pid, pgid)
	}
	return newHandle(cmd, pid, pgid, opts), nil
}

// Start is Configure + cmd.Start + Bind. Wait is not armed before Start
// returns, so short-lived producers that use StdoutPipe/StderrPipe can begin
// draining before the reaper closes the pipes.
func Start(cmd *exec.Cmd, opts Options) (*Handle, error) {
	if cmd == nil {
		return nil, fmt.Errorf("process containment: command is required")
	}
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("process containment: start: %w", err)
	}
	return Bind(cmd, opts)
}

func newHandle(cmd *exec.Cmd, pid, pgid int, opts Options) *Handle {
	grace := opts.GracePeriod
	if grace == 0 {
		grace = defaultGracePeriod
	}
	drainTimeout := opts.DrainTimeout
	if drainTimeout == 0 {
		drainTimeout = defaultDrainTimeout
	}
	signalFn := opts.Signal
	if signalFn == nil {
		signalFn = syscall.Kill
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Handle{
		cmd:          cmd,
		pid:          pid,
		pgid:         pgid,
		gracePeriod:  grace,
		drainTimeout: drainTimeout,
		signalFn:     signalFn,
		now:          now,
		waitCh:       make(chan struct{}),
	}
}

// armWait starts exactly-once leader reaping. Deferred until Wait/Kill/Drain
// so Bind/Start can return before any pipe readers are running.
// No-op when cmd is unset (unit-test handles that close waitCh directly).
func (h *Handle) armWait() {
	if h.cmd == nil {
		return
	}
	h.armOnce.Do(func() {
		go func() {
			h.waitOnce.Do(func() {
				err := h.cmd.Wait()
				state := h.cmd.ProcessState
				// Publish under mu so Snapshot/ProcessState cannot race the write.
				h.mu.Lock()
				h.waitErr = err
				h.state = state
				h.mu.Unlock()
				close(h.waitCh)
			})
		}()
	})
}

// PID returns the leader process id.
func (h *Handle) PID() int { return h.pid }

// PGID returns the owned process group id.
func (h *Handle) PGID() int { return h.pgid }

// Snapshot returns the current drain progress. ConfirmedDead is the only
// success signal for stop release.
func (h *Handle) Snapshot() DrainSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	exitCode := -1
	if h.state != nil {
		exitCode = h.state.ExitCode()
	}
	return DrainSnapshot{
		LeaderPID:     h.pid,
		PGID:          h.pgid,
		LeaderReaped:  h.waitConsumed || h.isWaitDone(),
		TermDelivered: h.termDelivered,
		KillEscalated: h.killEscalated,
		ConfirmedDead: h.confirmedDead,
		ExitCode:      exitCode,
		WaitErr:       h.waitErr,
	}
}

// ConfirmedDead reports whether the owned process group is confirmed
// non-runnable and the leader has been reaped.
func (h *Handle) ConfirmedDead() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.confirmedDead
}

// SignalGroup delivers sig to the owned process group.
// Success of this call is never treated as confirmed-dead.
// After confirmed-dead, returns nil without signaling: the leader was reaped
// and the numeric pgid may later be reused, so late cleanup/retry must not
// target -pgid (same reusable-ID guard as Kill/Drain).
//
// Non-zero signals also re-check groupRunnable before delivery. Callers may
// Wait (reap the leader) then use this low-level path for cleanup before
// Drain/Kill sets confirmedDead; once the original group is empty the PGID
// can already be released/reused, and groupRunnable's wait-done/reuse probes
// are the same authority Kill/Drain use to avoid signaling an unrelated group.
func (h *Handle) SignalGroup(sig syscall.Signal) error {
	h.mu.Lock()
	if h.confirmedDead {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	if h.pgid <= 0 {
		return fmt.Errorf("process containment: invalid pgid %d", h.pgid)
	}
	// Probe (sig==0) must still reach signalFn so groupRunnable can observe
	// liveness/reuse. Cleanup signals must not.
	if sig != 0 && !h.groupRunnable() {
		return nil
	}
	err := h.signalFn(-h.pgid, sig)
	if err == nil {
		h.mu.Lock()
		if sig == syscall.SIGTERM {
			h.termDelivered = true
		}
		if sig == syscall.SIGKILL {
			h.killEscalated = true
		}
		h.mu.Unlock()
		return nil
	}
	if isNoSuchProcess(err) {
		// Group may already be empty; still not confirmed until Wait+drain.
		return nil
	}
	return err
}

// Wait waits for the leader to exit and reaps it exactly once.
// Leader exit alone does not confirm descendants are dead; call Drain.
//
// When waitCh is already closed and ctx is also canceled, prefer the completed
// wait. Returning ctx.Err() after reap would make Drain treat a finished
// stop as failNotConfirmed even though the leader is already reaped.
func (h *Handle) Wait(ctx context.Context) error {
	h.armWait()
	// Fast path: already reaped.
	select {
	case <-h.waitCh:
		return h.consumeWait()
	default:
	}
	select {
	case <-h.waitCh:
		return h.consumeWait()
	case <-ctx.Done():
		// Both may be ready; re-check wait before honoring cancellation.
		select {
		case <-h.waitCh:
			return h.consumeWait()
		default:
			return ctx.Err()
		}
	}
}

// consumeWait records that the leader has been reaped and returns waitErr.
// Call only after receiving from waitCh (or observing it closed).
func (h *Handle) consumeWait() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.waitConsumed = true
	return h.waitErr
}

// ProcessState returns the reaped leader ProcessState, or nil if not yet waited.
func (h *Handle) ProcessState() *os.ProcessState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

// Kill delivers SIGTERM to the group, escalates to SIGKILL after the grace
// period, waits for leader reap, and drains descendants until the group is
// confirmed non-runnable. Returns nil only on confirmed-dead; otherwise an
// explicit error (including context/timeout wrapped with ErrNotConfirmedDead).
//
// If the leader was already reaped (for example Wait then Kill as cleanup),
// Kill skips the initial group signal and only drains/confirms: the leader
// PID/PGID may already have been released, so an unconditional SIGTERM to
// -pgid could hit a reused process group when no descendants remain.
func (h *Handle) Kill(ctx context.Context) error {
	h.mu.Lock()
	if h.confirmedDead {
		h.mu.Unlock()
		// Already confirmed; never re-signal a reusable numeric pgid/pid.
		return nil
	}
	h.mu.Unlock()

	h.armWait()

	ctx, cancel := h.withDrainTimeout(ctx)
	defer cancel()

	// Already reaped: drain only. Do not SIGTERM -pgid before observing waitCh.
	select {
	case <-h.waitCh:
		_ = h.consumeWait()
		return h.drainGroup(ctx)
	default:
	}

	if err := h.SignalGroup(syscall.SIGTERM); err != nil {
		return fmt.Errorf("process containment: SIGTERM: %w", err)
	}

	// Wait on waitCh in this goroutine (no detached waiter). A background
	// Wait(context.Background()) would keep a goroutine and the handle/cmd
	// alive after Kill returns on timeout, and each retry would add another.
	graceTimer := h.graceTimer()
	escalated := false
	for {
		// Prefer completed leader wait over cancellation when both are ready.
		select {
		case <-h.waitCh:
			_ = h.consumeWait()
			if err := h.drainGroup(ctx); err != nil {
				return err
			}
			return nil
		default:
		}

		select {
		case <-h.waitCh:
			_ = h.consumeWait()
			// Leader reaped; still must drain descendants.
			if err := h.drainGroup(ctx); err != nil {
				return err
			}
			return nil
		case <-ctx.Done():
			// Re-check waitCh in case reap raced with cancellation.
			select {
			case <-h.waitCh:
				_ = h.consumeWait()
				if err := h.drainGroup(ctx); err != nil {
					return err
				}
				return nil
			default:
				_ = h.SignalGroup(syscall.SIGKILL)
				return h.failNotConfirmed(fmt.Errorf("kill interrupted: %w", ctx.Err()))
			}
		case <-graceTimer:
			graceTimer = nil
			if !escalated {
				escalated = true
				_ = h.SignalGroup(syscall.SIGKILL)
			}
			// Keep looping until leader exit or ctx timeout.
		}
	}
}

// Drain waits for the leader (if needed) and ensures no runnable members remain
// in the owned process group. Intended after normal leader exit leaves
// background descendants, or as the confirmation half of stop delivery.
// Returns nil only when ConfirmedDead. Already-confirmed handles no-op so
// retry paths never re-probe a reusable numeric pgid.
//
// Drain must not block on Wait before signaling the group. With writer-based
// stdout/stderr capture (cmd.Stdout/Stderr set to an io.Writer), cmd.Wait does
// not return until Go's copy goroutines see EOF. A background descendant that
// inherited those pipes keeps Wait stuck; signaling via drainGroup first closes
// the pipes so the reaper can finish. Confirmed-dead still requires waitCh
// completion inside drainGroup.
func (h *Handle) Drain(ctx context.Context) error {
	h.mu.Lock()
	if h.confirmedDead {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	ctx, cancel := h.withDrainTimeout(ctx)
	defer cancel()

	h.armWait()
	return h.drainGroup(ctx)
}

func (h *Handle) drainGroup(ctx context.Context) error {
	// After leader reap, remaining group members (background children that
	// inherited the pgid) must be forced down. TERM-resistant members need KILL.
	if h.groupRunnable() {
		_ = h.SignalGroup(syscall.SIGTERM)
	}

	deadline := h.now().Add(h.remainingDrainBudget(ctx))
	graceDeadline := h.now().Add(h.effectiveGrace())
	escalated := false

	for {
		if !h.groupRunnable() {
			// Leader must be reaped for confirmed-dead.
			select {
			case <-h.waitCh:
				_ = h.consumeWait()
				h.mu.Lock()
				h.confirmedDead = true
				h.mu.Unlock()
				return nil
			default:
				// Group empty but leader wait not finished — still waiting to reap.
			}
		}

		if err := ctx.Err(); err != nil {
			_ = h.SignalGroup(syscall.SIGKILL)
			return h.failNotConfirmed(err)
		}
		if h.now().After(deadline) {
			_ = h.SignalGroup(syscall.SIGKILL)
			return h.failNotConfirmed(fmt.Errorf("%w: drain timeout", ErrNotConfirmedDead))
		}

		if !escalated && (h.gracePeriod < 0 || !h.now().Before(graceDeadline)) {
			escalated = true
			_ = h.SignalGroup(syscall.SIGKILL)
		}

		// Prefer waiting on leader reap when still pending.
		timer := time.NewTimer(groupPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = h.SignalGroup(syscall.SIGKILL)
			return h.failNotConfirmed(ctx.Err())
		case <-h.waitCh:
			timer.Stop()
			// reaped; loop to re-check group
		case <-timer.C:
		}
	}
}

func (h *Handle) groupRunnable() bool {
	if h.pgid <= 0 {
		return false
	}
	err := h.signalFn(-h.pgid, 0)
	if err != nil {
		// ESRCH / process done => no addressable group members.
		return !isNoSuchProcess(err)
	}
	// After the leader is reaped, kill(-pgid, 0) may succeed because a new
	// process group reused the numeric PGID. On Unix the leader PID is not
	// recycled while the old group still has members, so a live process at
	// pid==pgid after reap means the empty group's id was reassigned — not
	// our ownership. Do not treat that as runnable (avoids TERM/KILL of an
	// unrelated group from delayed drainGroup cleanup).
	if h.isWaitDone() {
		if err := h.signalFn(h.pgid, 0); err == nil {
			return false
		}
		// +pgid gone (or unprobeable): -pgid live ⇒ orphaned descendants.
	}
	// kill(-pgid, 0) succeeds for zombie-only groups on Linux. Those are not
	// runnable; confirmed-dead must not wait on init reaping them.
	if live, ok := h.groupHasLiveMember(h.pgid); ok {
		return live
	}
	// No platform non-zombie probe (or scan failed): trust signal 0.
	return true
}

// groupHasLiveMember reports non-zombie membership for pgid.
// Tests may override via Handle.groupLive so synthetic signalFn fixtures do
// not collide with a real /proc scan of an unused numeric PGID on Linux.
func (h *Handle) groupHasLiveMember(pgid int) (hasLive bool, ok bool) {
	if h.groupLive != nil {
		return h.groupLive(pgid)
	}
	return groupHasNonZombieMember(pgid)
}

func (h *Handle) withDrainTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if h.drainTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if h.now().Add(h.drainTimeout).After(deadline) {
			return context.WithCancel(ctx)
		}
	}
	return context.WithTimeout(ctx, h.drainTimeout)
}

func (h *Handle) remainingDrainBudget(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		rem := deadline.Sub(h.now())
		if rem < 0 {
			return 0
		}
		return rem
	}
	if h.drainTimeout > 0 {
		return h.drainTimeout
	}
	return defaultDrainTimeout
}

func (h *Handle) effectiveGrace() time.Duration {
	if h.gracePeriod < 0 {
		return 0
	}
	if h.gracePeriod == 0 {
		return defaultGracePeriod
	}
	return h.gracePeriod
}

func (h *Handle) graceTimer() <-chan time.Time {
	grace := h.effectiveGrace()
	if grace <= 0 {
		ch := make(chan time.Time, 1)
		ch <- h.now()
		return ch
	}
	return time.After(grace)
}

func (h *Handle) isWaitDone() bool {
	select {
	case <-h.waitCh:
		return true
	default:
		return false
	}
}

// failNotConfirmed wraps a stop failure with ErrNotConfirmedDead.
// confirmedDead is monotonic: once another overlapping Kill/Drain sets it,
// a later failure path must not flip it back to false (reusable PGID safety).
func (h *Handle) failNotConfirmed(cause error) error {
	if cause == nil {
		return ErrNotConfirmedDead
	}
	if errors.Is(cause, ErrNotConfirmedDead) {
		return cause
	}
	return fmt.Errorf("%w: %v", ErrNotConfirmedDead, cause)
}

func isNoSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone)
}

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

	waitOnce sync.Once
	waitCh   chan struct{}
	waitErr  error
	state    *os.ProcessState

	mu            sync.Mutex
	termDelivered bool
	killEscalated bool
	confirmedDead bool
	waitConsumed  bool
}

// Configure sets process-group isolation on cmd before Start.
// The child becomes the leader of a new process group (Setpgid).
// Shared by agent and shell producers when they migrate onto this handle.
func Configure(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Bind attaches a Handle to an already-started command that was Configure'd
// (or otherwise started in its own process group). Arms exactly-once wait
// without treating the process as drained.
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
		// Fall back to leader pid as pgid (Setpgid leaders use pid==pgid).
		pgid = pid
	}
	h := newHandle(cmd, pid, pgid, opts)
	h.armWait()
	return h, nil
}

// Start is Configure + cmd.Start + Bind.
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

func (h *Handle) armWait() {
	go func() {
		h.waitOnce.Do(func() {
			h.waitErr = h.cmd.Wait()
			h.state = h.cmd.ProcessState
			close(h.waitCh)
		})
	}()
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
func (h *Handle) SignalGroup(sig syscall.Signal) error {
	if h.pgid <= 0 {
		return fmt.Errorf("process containment: invalid pgid %d", h.pgid)
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
func (h *Handle) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.waitCh:
	}
	h.mu.Lock()
	h.waitConsumed = true
	err := h.waitErr
	h.mu.Unlock()
	return err
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
func (h *Handle) Kill(ctx context.Context) error {
	ctx, cancel := h.withDrainTimeout(ctx)
	defer cancel()

	if err := h.SignalGroup(syscall.SIGTERM); err != nil {
		return fmt.Errorf("process containment: SIGTERM: %w", err)
	}

	// Concurrently wait for leader exit while grace period runs.
	leaderDone := make(chan struct{})
	go func() {
		_ = h.Wait(context.Background())
		close(leaderDone)
	}()

	graceTimer := h.graceTimer()
	escalated := false
	for {
		select {
		case <-ctx.Done():
			_ = h.SignalGroup(syscall.SIGKILL)
			return h.failNotConfirmed(fmt.Errorf("kill interrupted: %w", ctx.Err()))
		case <-leaderDone:
			leaderDone = nil
			// Leader reaped; still must drain descendants.
			if err := h.drainGroup(ctx); err != nil {
				return err
			}
			return nil
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
// Returns nil only when ConfirmedDead.
func (h *Handle) Drain(ctx context.Context) error {
	ctx, cancel := h.withDrainTimeout(ctx)
	defer cancel()

	if err := h.Wait(ctx); err != nil {
		// Leader non-zero exit or signal death still reaps; only ctx failure aborts.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return h.failNotConfirmed(err)
		}
	}
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
				h.mu.Lock()
				h.waitConsumed = true
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
	if err == nil {
		return true
	}
	return !isNoSuchProcess(err)
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

func (h *Handle) failNotConfirmed(cause error) error {
	h.mu.Lock()
	h.confirmedDead = false
	h.mu.Unlock()
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

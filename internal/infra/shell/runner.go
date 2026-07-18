package shell

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/processcontainment"
)

const (
	defaultMaxOutputBytes = 256 * 1024
	defaultGracefulStop   = 5 * time.Second
	// startAttempts covers transient Linux ETXTBSY after a binary/script is
	// installed (common when tests write a fake tool then exec it immediately).
	startAttempts      = 8
	startRetryBaseWait = 5 * time.Millisecond
	// drainSlack is added to GracefulShutdown when building containment
	// DrainTimeout so TERM grace plus descendant cleanup fit the budget.
	drainSlack = 15 * time.Second
)

type Result struct {
	ExitCode        int
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	Duration        time.Duration
	DurationMS      int64
}

type Options struct {
	Command          string
	Args             []string
	CWD              string
	Env              map[string]string
	Stdin            string
	Timeout          time.Duration
	GracefulShutdown time.Duration
	MaxCapturedBytes int
}

type CommandExecutionError struct {
	Message string
	Result  Result
}

func (e *CommandExecutionError) Error() string { return e.Message }

// Run starts a command under process containment (ADR-0015 / #577).
//
// The spawn boundary is Configure + Start + Bind. Cancel and timeout use
// Handle.Kill (confirmed drain), not raw PID signal-only success. Normal exit
// still Drain so background process-group descendants cannot outlive Run.
func Run(ctx context.Context, options Options) (Result, error) {
	if strings.TrimSpace(options.Command) == "" {
		return Result{}, fmt.Errorf("command is required")
	}

	startedAt := time.Now()
	maxCapturedBytes := options.MaxCapturedBytes
	if maxCapturedBytes <= 0 {
		maxCapturedBytes = defaultMaxOutputBytes
	}
	gracefulShutdown := options.GracefulShutdown
	if gracefulShutdown <= 0 {
		gracefulShutdown = defaultGracefulStop
	}

	stdoutBuffer := newBoundedBuffer(maxCapturedBytes)
	stderrBuffer := newBoundedBuffer(maxCapturedBytes)

	handle, err := startContainedCommand(ctx, options, gracefulShutdown, stdoutBuffer, stderrBuffer)
	if err != nil {
		return Result{}, fmt.Errorf("start command: %w", err)
	}

	waitCtx := ctx
	var timeoutCancel context.CancelFunc
	if options.Timeout > 0 {
		waitCtx, timeoutCancel = context.WithTimeout(ctx, options.Timeout)
		defer timeoutCancel()
	}

	waitErr := handle.Wait(waitCtx)
	timedOut := false
	var canceledErr error
	var killErr error

	if waitErr != nil && isContextError(waitErr) {
		// Prefer the parent context error when it was the cause.
		if ctx.Err() != nil {
			canceledErr = ctx.Err()
		} else if options.Timeout > 0 && errors.Is(waitErr, context.DeadlineExceeded) {
			timedOut = true
		} else {
			canceledErr = waitErr
		}
		// Confirmed drain is the only stop success path (#574 / #577).
		// Propagate Kill failures (e.g. ErrNotConfirmedDead) so callers learn
		// that process-group containment failed rather than only seeing cancel/timeout.
		killCtx, killCancel := context.WithTimeout(context.Background(), gracefulShutdown+drainSlack)
		killErr = handle.Kill(killCtx)
		killCancel()
	} else {
		// Leader exited (zero or non-zero). Drain group members that outlived it.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), gracefulShutdown+drainSlack)
		if drainErr := handle.Drain(drainCtx); drainErr != nil {
			// Surface drain failure after a finished leader so callers do not
			// treat Run as successful while descendants remain runnable.
			if waitErr == nil {
				waitErr = drainErr
			} else {
				waitErr = errors.Join(waitErr, drainErr)
			}
		}
		drainCancel()
	}

	duration := time.Since(startedAt)
	result := Result{
		ExitCode:        exitCode(handle),
		Stdout:          stdoutBuffer.String(),
		Stderr:          stderrBuffer.String(),
		StdoutTruncated: stdoutBuffer.Truncated(),
		StderrTruncated: stderrBuffer.Truncated(),
		Duration:        duration,
		DurationMS:      duration.Milliseconds(),
	}

	if timedOut {
		timeoutErr := error(&CommandExecutionError{Message: "Command timed out", Result: result})
		if killErr != nil {
			return result, errors.Join(timeoutErr, killErr)
		}
		return result, timeoutErr
	}
	if canceledErr != nil {
		if killErr != nil {
			return result, errors.Join(canceledErr, killErr)
		}
		return result, canceledErr
	}
	if result.ExitCode != 0 {
		return result, &CommandExecutionError{Message: commandFailureMessage(result), Result: result}
	}
	if waitErr != nil && !isExitError(waitErr) {
		return result, waitErr
	}
	return result, nil
}

func startContainedCommand(ctx context.Context, options Options, gracefulShutdown time.Duration, stdout, stderr *boundedBuffer) (*processcontainment.Handle, error) {
	var lastErr error
	for attempt := 0; attempt < startAttempts; attempt++ {
		if attempt > 0 {
			wait := startRetryBaseWait * time.Duration(attempt)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		cmd := exec.Command(options.Command, options.Args...)
		cmd.Dir = options.CWD
		if len(options.Env) > 0 {
			cmd.Env = envSlice(options.Env)
		}
		if options.Stdin != "" {
			cmd.Stdin = strings.NewReader(options.Stdin)
		}
		// Fresh buffers each attempt: Start never ran on failure, but reset
		// so a partial Write cannot leak across retries if that ever changes.
		stdout.reset()
		stderr.reset()
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		processcontainment.Configure(cmd)

		if err := cmd.Start(); err != nil {
			lastErr = err
			if isTextFileBusy(err) {
				continue
			}
			return nil, err
		}
		handle, err := processcontainment.Bind(cmd, processcontainment.Options{
			GracePeriod:  gracefulShutdown,
			DrainTimeout: gracefulShutdown + drainSlack,
		})
		if err != nil {
			killStartedWithoutHandle(cmd)
			return nil, err
		}
		return handle, nil
	}
	return nil, lastErr
}

// killStartedWithoutHandle is only used when Bind fails after Start so the
// orphaned process group is not left live. Production stop paths use Handle.Kill.
func killStartedWithoutHandle(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func isTextFileBusy(err error) bool {
	return errors.Is(err, syscall.ETXTBSY)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

func commandFailureMessage(result Result) string {
	message := fmt.Sprintf("Command exited with code %d", result.ExitCode)
	stderr := strings.TrimSpace(result.Stderr)
	stdout := strings.TrimSpace(result.Stdout)
	if stderr != "" {
		message += ": " + stderr
	}
	if stdout != "" {
		if stderr != "" {
			message += "\nstdout: " + stdout
		} else {
			message += ": " + stdout
		}
	}
	return message
}

func envSlice(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+env[key])
	}
	return values
}

func exitCode(handle *processcontainment.Handle) int {
	if handle == nil {
		return -1
	}
	state := handle.ProcessState()
	if state == nil {
		return -1
	}
	return state.ExitCode()
}

type boundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	if limit <= 0 {
		limit = defaultMaxOutputBytes
	}
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLen := len(p)
	if len(b.data) >= b.limit {
		if originalLen > 0 {
			b.truncated = true
		}
		return originalLen, nil
	}
	remaining := b.limit - len(b.data)
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return originalLen, nil
}

func (b *boundedBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = b.data[:0]
	b.truncated = false
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

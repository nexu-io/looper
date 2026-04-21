package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultMaxOutputBytes = 256 * 1024
	defaultGracefulStop   = 5 * time.Second
)

type Result struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	Duration   time.Duration
	DurationMS int64
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

	cmd := exec.Command(options.Command, options.Args...)
	cmd.Dir = options.CWD
	if len(options.Env) > 0 {
		cmd.Env = envSlice(options.Env)
	}
	if options.Stdin != "" {
		cmd.Stdin = strings.NewReader(options.Stdin)
	}

	stdoutBuffer := newBoundedBuffer(maxCapturedBytes)
	stderrBuffer := newBoundedBuffer(maxCapturedBytes)
	cmd.Stdout = stdoutBuffer
	cmd.Stderr = stderrBuffer

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start command: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var (
		waitErr          error
		timedOut         bool
		canceledErr      error
		terminationStart <-chan time.Time
		killAt           <-chan time.Time
		terminateOnce    sync.Once
	)

	terminate := func() {
		terminateOnce.Do(func() {
			if cmd.Process == nil {
				return
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !isProcessDone(err) {
				_ = cmd.Process.Kill()
				return
			}
			if gracefulShutdown <= 0 {
				_ = cmd.Process.Kill()
				return
			}
			killAt = time.After(gracefulShutdown)
		})
	}

	if options.Timeout > 0 {
		terminationStart = time.After(options.Timeout)
	}

	waiting := true
	for waiting {
		select {
		case waitErr = <-waitCh:
			waiting = false
		case <-terminationStart:
			timedOut = true
			terminationStart = nil
			terminate()
		case <-killAt:
			killAt = nil
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		case <-ctx.Done():
			if canceledErr == nil {
				canceledErr = ctx.Err()
				terminate()
			}
		}
	}

	duration := time.Since(startedAt)
	result := Result{
		ExitCode:   exitCode(cmd),
		Stdout:     stdoutBuffer.String(),
		Stderr:     stderrBuffer.String(),
		Duration:   duration,
		DurationMS: duration.Milliseconds(),
	}

	if timedOut {
		return result, &CommandExecutionError{Message: "Command timed out", Result: result}
	}
	if canceledErr != nil {
		return result, canceledErr
	}
	if result.ExitCode != 0 {
		return result, &CommandExecutionError{Message: fmt.Sprintf("Command exited with code %d", result.ExitCode), Result: result}
	}
	if waitErr != nil {
		return result, waitErr
	}
	return result, nil
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

func exitCode(cmd *exec.Cmd) int {
	if cmd == nil || cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

func isProcessDone(err error) bool {
	return err == nil || err == os.ErrProcessDone
}

type boundedBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
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
		return originalLen, nil
	}
	remaining := b.limit - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return originalLen, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

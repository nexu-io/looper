package modelcatalog

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/processcontainment"
)

const (
	// maxProbeOutputBytes caps retained stdout and stderr independently.
	// Writers keep draining after the cap (discard excess) so pipe-full cannot
	// stall the child; overflow is treated as probe failure.
	maxProbeOutputBytes = 2 << 20 // 2 MiB per stream

	probeGracePeriod  = 500 * time.Millisecond
	probeCleanupSlack = 2 * time.Second
)

// Runner executes a short-lived CLI probe. Tests inject fakes; production uses
// defaultRunner (exec with timeout + process group kill).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout []byte, err error)
}

type defaultRunner struct{}

func (defaultRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cmd := exec.Command(name, args...)
	stdout := newBoundedBuffer(maxProbeOutputBytes)
	stderr := newBoundedBuffer(maxProbeOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	handle, err := processcontainment.Start(cmd, processcontainment.Options{
		GracePeriod:  probeGracePeriod,
		DrainTimeout: probeGracePeriod + probeCleanupSlack,
	})
	if err != nil {
		return nil, fmt.Errorf("start probe: %w", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- handle.Wait(ctx) }()

	waitErr, killErr, drainErr := awaitProbeCommand(handle, ctx, waitDone)

	if stdout.Truncated() || stderr.Truncated() {
		return nil, fmt.Errorf("probe output exceeded %d bytes", maxProbeOutputBytes)
	}

	out := stdout.Bytes()
	if waitErr != nil && isContextError(waitErr) {
		msg := "probe timed out"
		if errors.Is(waitErr, context.Canceled) {
			msg = "probe canceled"
		}
		if killErr != nil {
			return out, errors.Join(fmt.Errorf("%s", msg), killErr)
		}
		return out, fmt.Errorf("%s", msg)
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		if drainErr != nil {
			return out, errors.Join(fmt.Errorf("%s", msg), drainErr)
		}
		return out, fmt.Errorf("%s", msg)
	}
	if drainErr != nil {
		return out, drainErr
	}
	if killErr != nil {
		return out, killErr
	}
	return out, nil
}

// awaitProbeCommand finishes a contained probe without hanging on cmd.Wait's
// pipe-copy phase. On cancel/timeout: Kill the process group with an independent
// cleanup context. After normal leader exit: Drain descendants. When the leader
// is reaped but Wait is stuck on a pipe-holding descendant, Drain unblocks it.
func awaitProbeCommand(
	handle *processcontainment.Handle,
	ctx context.Context,
	waitDone <-chan error,
) (waitErr error, killErr error, drainErr error) {
	cleanupTimeout := probeGracePeriod + probeCleanupSlack
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()

	var alreadyDrained bool
	for {
		select {
		case waitErr = <-waitDone:
			if waitErr != nil && isContextError(waitErr) {
				killCtx, killCancel := context.WithTimeout(context.Background(), cleanupTimeout)
				killErr = handle.Kill(killCtx)
				killCancel()
				return waitErr, killErr, nil
			}
			if !alreadyDrained {
				drainCtx, drainCancel := context.WithTimeout(context.Background(), cleanupTimeout)
				drainErr = handle.Drain(drainCtx)
				drainCancel()
			}
			return waitErr, nil, drainErr

		case <-poll.C:
			if alreadyDrained || !processPIDGone(handle.PID()) {
				continue
			}
			// Leader reaped but pipe copy still blocks Wait — Drain kills
			// pipe-holding descendants so Wait can finish.
			drainCtx, drainCancel := context.WithTimeout(context.Background(), cleanupTimeout)
			drainErr = handle.Drain(drainCtx)
			drainCancel()
			alreadyDrained = true
			select {
			case waitErr = <-waitDone:
				return waitErr, nil, drainErr
			case <-time.After(cleanupTimeout):
				if waitErr == nil {
					if drainErr != nil {
						waitErr = drainErr
					} else {
						waitErr = fmt.Errorf("probe wait did not complete after drain")
					}
				}
				return waitErr, nil, drainErr
			}
		}
	}
}

func processPIDGone(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// boundedBuffer retains up to limit bytes then discards the rest while still
// accepting Writes (so the child is not blocked on a full pipe).
type boundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	if limit <= 0 {
		limit = maxProbeOutputBytes
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

func (b *boundedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) == 0 {
		return nil
	}
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
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

func execLookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func (s *Service) probe(ctx context.Context, vendor config.AgentVendor, binary string) ([]Model, error) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch vendor {
	case config.AgentVendorOpenCode:
		out, err := s.runner.Run(runCtx, binary, "models")
		if err != nil {
			return nil, err
		}
		return parseOpenCodeModels(out), nil
	case config.AgentVendorCodex:
		out, err := s.runner.Run(runCtx, binary, "debug", "models", "--bundled")
		if err != nil {
			return nil, err
		}
		return parseCodexModels(out)
	case config.AgentVendorCursorCLI:
		out, err := s.runner.Run(runCtx, binary, "models")
		if err != nil {
			// Fallback flag used by some cursor-agent builds.
			out2, err2 := s.runner.Run(runCtx, binary, "--list-models")
			if err2 != nil {
				return nil, err
			}
			return parseCursorModels(out2), nil
		}
		return parseCursorModels(out), nil
	case config.AgentVendorGrokBuild:
		out, err := s.runner.Run(runCtx, binary, "models")
		if err != nil {
			return nil, err
		}
		return parseGrokModels(out), nil
	default:
		return nil, errors.New("probe unsupported")
	}
}

func shortProbeError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	// Collapse multi-line noise; keep a short operator-facing hint.
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const max = 240
	if len(msg) > max {
		msg = msg[:max] + "…"
	}
	if msg == "" {
		return "probe failed"
	}
	return msg
}

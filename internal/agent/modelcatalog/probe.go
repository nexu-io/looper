package modelcatalog

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

	"github.com/nexu-io/looper/internal/agent"
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
// defaultRunner (exec with timeout + process group kill + sanitized env).
// env is the full sanitized process environment (KEY=value entries), never nil
// for production probes — use buildProbeEnv.
type Runner interface {
	Run(ctx context.Context, env []string, name string, args ...string) (stdout []byte, err error)
}

// defaultRunner spawns vendor CLIs under process containment with an allowlisted
// environment matching agent spawn (not the daemon ambient environment).
type defaultRunner struct {
	tracker processcontainment.LiveTracker
}

func (r defaultRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Defense in depth: relative path commands need a worktree Dir (spawn sets
	// cmd.Dir). Catalog List rejects these before probe; refuse here too so a
	// direct Run cannot resolve them from looperd's CWD.
	if isRelativePathCommand(name) {
		return nil, fmt.Errorf("%s", relativeCommandProbeError)
	}

	cmd := exec.Command(name, args...)
	// Always set Env so ambient daemon credentials cannot leak into vendor CLIs.
	// Empty env still means "explicit empty" rather than inherit (exec default).
	if env == nil {
		env = buildProbeEnv(nil)
	}
	cmd.Env = env
	stdout := newBoundedBuffer(maxProbeOutputBytes)
	stderr := newBoundedBuffer(maxProbeOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// BeginTrack before Start so BeginShutdown cannot snapshot an empty handle
	// set while Start→Track is in flight (admission-closed refuse kills in Track).
	handle, release, err := processcontainment.StartTracked(r.tracker, cmd, processcontainment.Options{
		GracePeriod:  probeGracePeriod,
		DrainTimeout: probeGracePeriod + probeCleanupSlack,
	})
	if err != nil {
		return nil, fmt.Errorf("start probe: %w", err)
	}
	if release != nil {
		defer release()
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- handle.Wait(ctx) }()

	waitErr, killErr, drainErr := awaitProbeCommand(handle, ctx, waitDone)
	reportProbeContainmentFailure(r.tracker, killErr, drainErr)

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

func reportProbeContainmentFailure(tracker processcontainment.LiveTracker, errs ...error) {
	if tracker == nil {
		return
	}
	for _, err := range errs {
		if err != nil {
			tracker.ReportDrainFailure(err)
		}
	}
}

// buildProbeEnv builds the sanitized environment for a model catalog probe,
// matching agent spawn allowlisting (BuildCommandEnv) and merging configured
// agent.env. Working directory and prompt are empty: probes are not agents.
func buildProbeEnv(agentEnv map[string]string) []string {
	return agent.BuildCommandEnv("", "", agentEnv)
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

func (s *Service) probe(ctx context.Context, vendor config.AgentVendor, binary string, env []string) ([]Model, error) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch vendor {
	case config.AgentVendorOpenCode:
		out, err := s.runner.Run(runCtx, env, binary, "models")
		if err != nil {
			return nil, err
		}
		return parseOpenCodeModels(out), nil
	case config.AgentVendorCodex:
		out, err := s.runner.Run(runCtx, env, binary, "debug", "models", "--bundled")
		if err != nil {
			return nil, err
		}
		return parseCodexModels(out)
	case config.AgentVendorCursorCLI:
		out, err := s.runner.Run(runCtx, env, binary, "models")
		if err != nil {
			// Fallback flag used by some cursor-agent builds.
			out2, err2 := s.runner.Run(runCtx, env, binary, "--list-models")
			if err2 != nil {
				return nil, err
			}
			return parseCursorModels(out2), nil
		}
		return parseCursorModels(out), nil
	case config.AgentVendorGrokBuild:
		out, err := s.runner.Run(runCtx, env, binary, "models")
		if err != nil {
			return nil, err
		}
		return parseGrokModels(out), nil
	default:
		return nil, errors.New("probe unsupported")
	}
}

// shortProbeError turns a probe failure into a short, client-safe hint for
// sources.probeError. Configured agent.env values are write-only in the config
// API (envKeys only); redact them here so CLI stderr that echoes credentials
// cannot leak those values into cached API responses.
func shortProbeError(err error, agentEnv map[string]string) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	msg = redactConfiguredEnvValues(msg, agentEnv)
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

// redactConfiguredEnvValues replaces every non-empty agent.env value that
// appears in msg with [REDACTED]. Longer values are applied first so a secret
// that is a substring of another is fully covered. Empty values are skipped.
func redactConfiguredEnvValues(msg string, agentEnv map[string]string) string {
	if msg == "" || len(agentEnv) == 0 {
		return msg
	}
	vals := make([]string, 0, len(agentEnv))
	seen := make(map[string]struct{}, len(agentEnv))
	for _, v := range agentEnv {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		vals = append(vals, v)
	}
	sort.Slice(vals, func(i, j int) bool {
		return len(vals[i]) > len(vals[j])
	})
	for _, v := range vals {
		if strings.Contains(msg, v) {
			msg = strings.ReplaceAll(msg, v, "[REDACTED]")
		}
	}
	return msg
}

package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/processcontainment"
)

// TrustedReviewSockEnv is the agent-facing env key for the trusted review-submit
// proxy socket. The path is not a secret: agents may use the socket only to
// invoke `looper review submit`, and the proxy never returns provider tokens.
// Pattern matches SSH_AUTH_SOCK: a capability channel, not a credential dump.
const TrustedReviewSockEnv = "LOOPER_TRUSTED_REVIEW_SOCK"

// trustedReviewProxySkipEnv marks a proxy-spawned looper child so it does not
// re-enter the proxy (the child receives provider tokens directly).
const trustedReviewProxySkipEnv = "LOOPER_TRUSTED_REVIEW_PROXY_CHILD"

// TrustedReviewConfigFDEnv identifies the inherited, read-only pipe containing
// the exact materialized config snapshot for a trusted review child. The pipe
// replaces a named temporary file so the same-UID agent cannot discover or
// rewrite the snapshot between proxy minting and review submission.
const TrustedReviewConfigFDEnv = "LOOPER_TRUSTED_REVIEW_CONFIG_FD"

const (
	maxTrustedReviewProxyConnections   = 4
	maxTrustedReviewProxyRequestBytes  = 2 << 20
	maxTrustedReviewProxyStdinBytes    = 1 << 20
	maxTrustedReviewProxyOutputBytes   = 256 << 10
	maxTrustedReviewProxyResponseBytes = 4 << 20
	maxTrustedReviewConfigSnapshotSize = 4 << 20
)

type trustedReviewProxyRequest struct {
	Argv  []string `json:"argv"`
	Stdin []byte   `json:"stdin"`
	Cwd   string   `json:"cwd"`
}

type trustedReviewProxyResponse struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Error    string `json:"error,omitempty"`
}

// FormatTrustedReviewPRRef builds the canonical owner/repo#N form used to bind
// a review-submit proxy to a single pull request for one agent run.
func FormatTrustedReviewPRRef(repo string, prNumber int64) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || prNumber <= 0 {
		return ""
	}
	return fmt.Sprintf("%s#%d", repo, prNumber)
}

// TrustedReviewProxyPolicy is the daemon-selected review authority bound into
// a trusted review-submit proxy for one agent run. Agent-supplied review-event,
// expected-head, and manual-run identity flags are stripped and replaced with
// these values before the child runs.
type TrustedReviewProxyPolicy struct {
	Clean            string // COMMENT or APPROVE
	Blocking         string // COMMENT or REQUEST_CHANGES
	ExpectedCommitID string
	ReviewerManual   bool
	ReviewerRunID    string
}

// StartTrustedReviewProxy listens on a private Unix socket and runs
// `looper review submit` in a daemon-side child with the run-captured credential
// environment injected. Agents receive only the socket path (via
// TrustedReviewSockEnv), never a secret-bearing wrapper path, config snapshot
// path, or LOOPER_TRUSTED_ENV_FILE.
//
// trustedEnv may be empty for tea-backed Forgejo providers that have no
// tokenEnv. The proxy still binds PR/CWD/policy/config so agents cannot retarget
// review submit; tea credentials resolve from the daemon process environment.
//
// allowedPRRef must be the daemon-selected pull request in owner/repo#N form.
// The proxy rejects any review-submit argv that targets a different PR so a
// prompt-injected agent cannot publish under tokens for another PR.
//
// allowedCwd must be the daemon-selected working directory for that run. Child
// processes always use this CWD; request-supplied cwd is ignored so a
// compromised agent cannot retarget provider-qualified project resolution.
//
// configSnapshot is the complete materialized config captured by the run. The
// proxy sanitizes and retains its encoded bytes in memory, then supplies them
// to each child through an inherited pipe. The agent receives neither a path
// nor the snapshot bytes and therefore cannot rewrite the child's authority.
//
// policy is the daemon-selected effective review-events policy, expected PR
// head, and manual-run identity for the run (including loop-metadata
// overrides). Agent argv may still include local flags for the prompted command
// shape, but the proxy always rewrites them to this bound authority before
// spawning the credential-injected child.
//
// tracker, when non-nil, registers each Supervisor-owned review-submit child
// handle so daemon shutdown can wait for confirmed drain and retain storage on
// Kill/Drain failure (ADR-0015 / #577).
func StartTrustedReviewProxy(realLooper string, trustedEnv map[string]string, allowedPRRef, allowedCwd string, configSnapshot config.Config, policy TrustedReviewProxyPolicy, tracker processcontainment.LiveTracker) (sockPath string, cleanup func(), err error) {
	realLooper = strings.TrimSpace(realLooper)
	if realLooper == "" {
		return "", nil, fmt.Errorf("real looper path is required for trusted review proxy")
	}
	normalizedAllowed, err := normalizeTrustedReviewPRRef(allowedPRRef)
	if err != nil {
		return "", nil, fmt.Errorf("trusted review proxy allowed PR: %w", err)
	}
	boundCwd, err := normalizeTrustedReviewCwd(allowedCwd)
	if err != nil {
		return "", nil, fmt.Errorf("trusted review proxy allowed CWD: %w", err)
	}
	boundPolicy, err := normalizeTrustedReviewProxyPolicy(policy)
	if err != nil {
		return "", nil, fmt.Errorf("trusted review proxy review policy: %w", err)
	}
	if _, err := os.Stat(realLooper); err != nil {
		return "", nil, fmt.Errorf("stat real looper path: %w", err)
	}
	boundConfig, err := marshalTrustedReviewConfigSnapshot(configSnapshot)
	if err != nil {
		return "", nil, err
	}

	dir, err := os.MkdirTemp("", "looper-trusted-review-sock-*")
	if err != nil {
		return "", nil, fmt.Errorf("create trusted review proxy dir: %w", err)
	}
	sockPath = filepath.Join(dir, "sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("listen trusted review proxy: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("chmod trusted review proxy socket: %w", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	proxyContext, cancelProxy := context.WithCancel(context.Background())
	slots := make(chan struct{}, maxTrustedReviewProxyConnections)
	connections := map[net.Conn]struct{}{}
	var connectionsMu sync.Mutex
	closing := false
	unregister := func(conn net.Conn) {
		connectionsMu.Lock()
		delete(connections, conn)
		connectionsMu.Unlock()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-stop:
					return
				default:
					continue
				}
			}
			connectionsMu.Lock()
			if closing {
				connectionsMu.Unlock()
				_ = conn.Close()
				continue
			}
			connections[conn] = struct{}{}
			connectionsMu.Unlock()
			select {
			case slots <- struct{}{}:
				// continue below
			default:
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: "trusted review proxy is busy"})
				_ = conn.Close()
				unregister(conn)
				continue
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { <-slots }()
				defer unregister(c)
				handleTrustedReviewProxyConn(proxyContext, c, realLooper, trustedEnv, normalizedAllowed, boundCwd, boundConfig, boundPolicy, tracker)
			}(conn)
		}
	}()

	var cleanupOnce sync.Once
	cleanup = func() {
		cleanupOnce.Do(func() {
			cancelProxy()
			connectionsMu.Lock()
			closing = true
			for conn := range connections {
				_ = conn.Close()
			}
			connectionsMu.Unlock()
			close(stop)
			_ = listener.Close()
			wg.Wait()
			_ = os.RemoveAll(dir)
		})
	}
	return sockPath, cleanup, nil
}

func handleTrustedReviewProxyConn(ctx context.Context, conn net.Conn, realLooper string, trustedEnv map[string]string, allowedPRRef, allowedCwd string, configSnapshot []byte, policy TrustedReviewProxyPolicy, tracker processcontainment.LiveTracker) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	var req trustedReviewProxyRequest
	limited := &io.LimitedReader{R: conn, N: maxTrustedReviewProxyRequestBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		message := "decode trusted review proxy request: " + err.Error()
		if limited.N <= 0 {
			message = "trusted review proxy request exceeds size limit"
		}
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: message})
		return
	}
	if len(req.Stdin) > maxTrustedReviewProxyStdinBytes {
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: "trusted review proxy stdin exceeds size limit"})
		return
	}
	if err := validateTrustedReviewProxyArgv(req.Argv, allowedPRRef); err != nil {
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	// Authority for clean/blocking event policy is the daemon-bound policy, not
	// agent argv. Rewrite local policy flags after shape/PR validation.
	childArgv := applyTrustedReviewProxyPolicy(req.Argv, policy)
	cmd := exec.Command(realLooper, childArgv...)
	// #577: Supervisor-owned non-agent child uses processcontainment at spawn.
	processcontainment.Configure(cmd)
	configReader, configWriter, err := os.Pipe()
	if err != nil {
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: "create trusted review config pipe: " + err.Error()})
		return
	}
	cmd.ExtraFiles = []*os.File{configReader}
	// Never honor request-supplied cwd: project/provider resolution for
	// provider-qualified same-owner/repo checkouts is CWD-sensitive. The child
	// always runs in the daemon-selected worktree bound at proxy start.
	cmd.Dir = allowedCwd
	cmd.Env = trustedReviewProxyChildEnv(trustedEnv, 3)
	cmd.Stdin = bytes.NewReader(req.Stdin)
	stdout := newTrustedReviewBoundedBuffer(maxTrustedReviewProxyOutputBytes)
	stderr := newTrustedReviewBoundedBuffer(maxTrustedReviewProxyOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = configReader.Close()
		_ = configWriter.Close()
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: err.Error()})
		return
	}
	handle, bindErr := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  2 * time.Second,
		DrainTimeout: 20 * time.Second,
	})
	if bindErr != nil {
		_ = configReader.Close()
		_ = configWriter.Close()
		// Bind failed after Start: force-kill the orphaned process group so it
		// does not outlive the proxy request (same emergency path as shell bind).
		killTrustedReviewStartedWithoutHandle(cmd)
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: "bind trusted review containment handle: " + bindErr.Error()})
		return
	}
	if tracker != nil {
		release := tracker.Track(handle)
		if release != nil {
			defer release()
		}
	}
	_ = configReader.Close()
	configWriteDone := make(chan error, 1)
	go func() {
		_, writeErr := io.Copy(configWriter, bytes.NewReader(configSnapshot))
		if closeErr := configWriter.Close(); writeErr == nil {
			writeErr = closeErr
		}
		configWriteDone <- writeErr
	}()
	// Handle owns exactly-once Wait; do not call cmd.Wait in parallel.
	// waitCtx is cancelable so post-Kill Wait cannot hang forever when an
	// escaped descendant keeps stdio open and cmd.Wait never reaps.
	waitCtx, waitCancel := context.WithCancel(context.Background())
	defer waitCancel()
	waitDone := make(chan error, 1)
	go func() { waitDone <- handle.Wait(waitCtx) }()
	// Poll leader liveness: if the leader is reaped but Wait is still blocked
	// on stdout/stderr copy (descendant holds pipes), Drain before the
	// connection deadline — ctx is not canceled by the conn deadline alone.
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	alreadyDrained := false
loop:
	for {
		select {
		case err = <-waitDone:
			if !alreadyDrained {
				// Normal exit path: confirmed-drain descendants before answering.
				drainCtx, drainCancel := context.WithTimeout(context.Background(), 20*time.Second)
				if drainErr := handle.Drain(drainCtx); drainErr != nil {
					reportTrustedReviewDrainFailure(tracker, drainErr)
					// Join so non-zero child exits still surface ErrNotConfirmedDead.
					if err == nil {
						err = drainErr
					} else {
						err = errors.Join(err, drainErr)
					}
				}
				drainCancel()
			}
			break loop
		case <-ctx.Done():
			// Cancel: confirmed Kill, never signal-only success (#577).
			killCtx, killCancel := context.WithTimeout(context.Background(), 20*time.Second)
			killErr := handle.Kill(killCtx)
			killCancel()
			if killErr != nil {
				reportTrustedReviewDrainFailure(tracker, killErr)
			}
			// Unstick Wait if Kill timed out without reaping the leader, then
			// bound the receive so cleanup cannot hang on an open waitDone.
			waitCancel()
			select {
			case err = <-waitDone:
			case <-time.After(time.Second):
				// Wait still blocked (should not happen once waitCtx is canceled);
				// fail loud so cleanup cannot hang in wg.Wait.
				if killErr != nil {
					err = killErr
				} else {
					err = fmt.Errorf("trusted review child wait did not complete after cancel: %w", ctx.Err())
				}
			}
			if err == nil {
				if killErr != nil {
					err = killErr
				} else {
					err = ctx.Err()
				}
			} else if killErr != nil {
				if errors.Is(err, context.Canceled) {
					// Wait unblocked via waitCancel after Kill; surface kill outcome.
					err = killErr
				} else {
					// Child already exited non-zero (or other wait error); still
					// surface containment failure so undrained descendants are not hidden.
					err = errors.Join(err, killErr)
				}
			}
			break loop
		case <-poll.C:
			if alreadyDrained || !trustedReviewLeaderPIDGone(handle.PID()) {
				continue
			}
			// Leader reaped; Drain kills pipe-holding descendants and unblocks Wait.
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 20*time.Second)
			if drainErr := handle.Drain(drainCtx); drainErr != nil {
				reportTrustedReviewDrainFailure(tracker, drainErr)
				if err == nil {
					err = drainErr
				} else {
					err = errors.Join(err, drainErr)
				}
			}
			drainCancel()
			alreadyDrained = true
			select {
			case waitErr := <-waitDone:
				if err == nil {
					err = waitErr
				} else if waitErr != nil {
					// Keep containment drain failure and child exit together.
					err = errors.Join(waitErr, err)
				}
			case <-time.After(time.Second):
				if err == nil {
					err = fmt.Errorf("trusted review child wait did not complete after drain")
				}
			}
			break loop
		}
	}
	configWriteErr := <-configWriteDone
	resp := trustedReviewProxyResponse{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Pure child exit: ordinary review-submit failure, no Error text.
			resp.ExitCode = exitErr.ExitCode()
		} else if state := handle.ProcessState(); state != nil {
			resp.ExitCode = state.ExitCode()
			if resp.ExitCode == 0 {
				resp.ExitCode = 1
			}
			// Always set Error for non-ExitError outcomes (drain/kill/cancel,
			// including errors.Join of exit + containment failure) so callers
			// do not treat undrained process groups as ordinary submit failures.
			resp.Error = err.Error()
		} else {
			resp.ExitCode = 1
			resp.Error = err.Error()
		}
	}
	if stdout.Truncated() || stderr.Truncated() {
		resp.ExitCode = 1
		// Preserve prior containment/kill/drain Error so ErrNotConfirmedDead is
		// not replaced by truncation-only text when both apply.
		resp.Error = mergeTrustedReviewTruncationError(resp.Error)
	} else if configWriteErr != nil && err == nil {
		resp.ExitCode = 1
		resp.Error = "write trusted review config snapshot: " + configWriteErr.Error()
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// trustedReviewLeaderPIDGone reports whether the leader pid has been reaped.
// Used to detect Wait stuck on pipe-copy after Process.Wait completed.
func trustedReviewLeaderPIDGone(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func reportTrustedReviewDrainFailure(tracker processcontainment.LiveTracker, err error) {
	if tracker == nil || err == nil {
		return
	}
	tracker.ReportDrainFailure(err)
}

const trustedReviewOutputTruncatedMsg = "trusted review proxy child output exceeds size limit"

// mergeTrustedReviewTruncationError keeps an existing containment/kill/drain
// error when output also exceeded the capture cap.
func mergeTrustedReviewTruncationError(existing string) string {
	if existing == "" {
		return trustedReviewOutputTruncatedMsg
	}
	return existing + "; " + trustedReviewOutputTruncatedMsg
}

// killTrustedReviewStartedWithoutHandle is only used when Bind fails after
// Start so the orphaned process group is not left live. Production stop paths
// use Handle.Kill. Mirrors shell.killStartedWithoutHandle: SIGKILL the group
// first, then fall back to Process.Kill + Wait on the leader.
func killTrustedReviewStartedWithoutHandle(cmd *exec.Cmd) {
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

// trustedReviewProxyBlockedFlags are CLI overrides that must never be accepted
// on the trusted review proxy. Config/tool/db path overrides can redirect
// provider baseURL while the daemon still injects the real tokenEnv. Global
// loadConfig review-policy flags can rewrite daemon config before the child
// validates the payload.
//
// Local review-submit `--clean-review-event` / `--blocking-review-event` are
// intentionally NOT blocked: the runner prompts agents to pass them so the
// command shape matches documentation. They are never authoritative — the
// proxy rewrites them to the daemon-bound policy before spawning the child.
var trustedReviewProxyBlockedFlags = map[string]struct{}{
	"config":                          {},
	"db-path":                         {},
	"host":                            {},
	"port":                            {},
	"log-dir":                         {},
	"daemon-mode":                     {},
	"daemon-restart-policy":           {},
	"daemon-restart-throttle-seconds": {},
	"git-path":                        {},
	"gh-path":                         {},
	"looper-path":                     {},
	"osascript-path":                  {},
	// Global loadConfig / review-policy overrides (not review-submit locals).
	"allow-auto-approve":                             {},
	"roles-reviewer-behavior-review-events-clean":    {},
	"reviewer-clean-review-event":                    {},
	"roles-reviewer-behavior-review-events-blocking": {},
	"reviewer-blocking-review-event":                 {},
}

func trustedReviewProxyFlagName(arg string) string {
	arg = strings.TrimSpace(arg)
	if !strings.HasPrefix(arg, "-") {
		return ""
	}
	// Normalize --flag=value / -flag=value / --flag / -flag.
	arg = strings.TrimLeft(arg, "-")
	if arg == "" {
		return ""
	}
	if name, _, ok := strings.Cut(arg, "="); ok {
		return strings.ToLower(strings.TrimSpace(name))
	}
	return strings.ToLower(arg)
}

func validateTrustedReviewProxyArgv(argv []string, allowedPRRef string) error {
	// Reject config/tool/db and global review-policy overrides anywhere in argv
	// first so a compromised agent cannot redirect the daemon-injected provider
	// token via --config, or rewrite daemon loadConfig review-events before the
	// child validates the payload, even after `review submit`. Local
	// --clean-review-event / --blocking-review-event are allowed for command
	// shape only; handleTrustedReviewProxyConn rewrites them to the bound policy.
	for _, arg := range argv {
		if name := trustedReviewProxyFlagName(arg); name != "" {
			if _, blocked := trustedReviewProxyBlockedFlags[name]; blocked {
				return fmt.Errorf("trusted review proxy rejects config/review-policy override flag %q", name)
			}
		}
	}

	// Allow only `review submit` (plus harmless non-override flags). Reject
	// anything that is not a review-submit invocation so the proxy cannot be
	// abused to run arbitrary looper subcommands with provider tokens.
	seenReview := false
	seenSubmit := false
	prRef := ""
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !seenReview {
			if arg == "review" {
				seenReview = true
				continue
			}
			if strings.HasPrefix(arg, "-") {
				// Skip global flag values when present (best-effort; unknown
				// boolean flags do not consume the next token when it looks like a flag).
				if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") && argv[i+1] != "review" {
					i++
				}
				continue
			}
			return fmt.Errorf("trusted review proxy only allows `looper review submit`")
		}
		if !seenSubmit {
			if arg == "submit" {
				seenSubmit = true
				continue
			}
			if strings.HasPrefix(arg, "-") {
				if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
					i++
				}
				continue
			}
			return fmt.Errorf("trusted review proxy only allows `looper review submit`")
		}
		// After `review submit`, collect the first positional as the PR target
		// and skip flag values for subsequent options.
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
			}
			continue
		}
		if prRef == "" {
			prRef = arg
			continue
		}
		// Extra positionals are not part of the allowed review-submit shape.
		return fmt.Errorf("trusted review proxy only allows `looper review submit <repo>#<number> ...`")
	}
	if !seenReview || !seenSubmit {
		return fmt.Errorf("trusted review proxy only allows `looper review submit`")
	}
	if strings.TrimSpace(prRef) == "" {
		return fmt.Errorf("trusted review proxy requires pull request target <repo>#<number>")
	}
	return matchTrustedReviewProxyPRRef(prRef, allowedPRRef)
}

func normalizeTrustedReviewPRRef(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("pull request reference is required")
	}
	parts := strings.Split(value, "#")
	if len(parts) != 2 {
		return "", fmt.Errorf("pull request reference must be <repo>#<number>")
	}
	repo := strings.ToLower(strings.TrimSpace(parts[0]))
	if repo == "" || !strings.Contains(repo, "/") {
		return "", fmt.Errorf("pull request reference must be <repo>#<number>")
	}
	numberPart := strings.TrimSpace(parts[1])
	n, err := strconv.ParseInt(numberPart, 10, 64)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("pull request reference must be <repo>#<number>")
	}
	return fmt.Sprintf("%s#%d", repo, n), nil
}

func matchTrustedReviewProxyPRRef(got, allowed string) error {
	normalizedGot, err := normalizeTrustedReviewPRRef(got)
	if err != nil {
		return fmt.Errorf("trusted review proxy PR target: %w", err)
	}
	normalizedAllowed, err := normalizeTrustedReviewPRRef(allowed)
	if err != nil {
		return fmt.Errorf("trusted review proxy allowed PR: %w", err)
	}
	if normalizedGot != normalizedAllowed {
		return fmt.Errorf("trusted review proxy rejects PR target %q; bound to %s", strings.TrimSpace(got), normalizedAllowed)
	}
	return nil
}

func normalizeTrustedReviewCwd(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("working directory is required")
	}
	cleaned := filepath.Clean(value)
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("working directory is required")
	}
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("working directory must be absolute")
	}
	return cleaned, nil
}

func normalizeTrustedReviewProxyPolicy(policy TrustedReviewProxyPolicy) (TrustedReviewProxyPolicy, error) {
	clean := strings.ToUpper(strings.TrimSpace(policy.Clean))
	blocking := strings.ToUpper(strings.TrimSpace(policy.Blocking))
	expectedCommitID := strings.TrimSpace(policy.ExpectedCommitID)
	reviewerRunID := strings.TrimSpace(policy.ReviewerRunID)
	switch clean {
	case "COMMENT", "APPROVE":
	default:
		return TrustedReviewProxyPolicy{}, fmt.Errorf("clean review event must be COMMENT or APPROVE")
	}
	switch blocking {
	case "COMMENT", "REQUEST_CHANGES":
	default:
		return TrustedReviewProxyPolicy{}, fmt.Errorf("blocking review event must be COMMENT or REQUEST_CHANGES")
	}
	if expectedCommitID == "" {
		return TrustedReviewProxyPolicy{}, fmt.Errorf("expected commit id is required")
	}
	if policy.ReviewerManual && reviewerRunID == "" {
		return TrustedReviewProxyPolicy{}, fmt.Errorf("manual reviewer run id is required")
	}
	return TrustedReviewProxyPolicy{
		Clean: clean, Blocking: blocking, ExpectedCommitID: expectedCommitID,
		ReviewerManual: policy.ReviewerManual, ReviewerRunID: reviewerRunID,
	}, nil
}

// applyTrustedReviewProxyPolicy strips agent-supplied local review-policy flags
// and injects the daemon-bound clean/blocking events so the child validates
// markers against daemon-selected policy only.
func applyTrustedReviewProxyPolicy(argv []string, policy TrustedReviewProxyPolicy) []string {
	stripped := stripTrustedReviewProxyPolicyFlags(argv)
	bound := append(stripped,
		"--clean-review-event", policy.Clean,
		"--blocking-review-event", policy.Blocking,
		"--commit-id", policy.ExpectedCommitID,
	)
	if policy.ReviewerManual {
		bound = append(bound, "--reviewer-manual", "--reviewer-run-id", policy.ReviewerRunID)
	}
	return bound
}

func stripTrustedReviewProxyPolicyFlags(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name := trustedReviewProxyFlagName(arg)
		switch name {
		case "reviewer-manual":
			continue
		case "clean-review-event", "blocking-review-event", "commit-id", "reviewer-run-id":
			if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

func marshalTrustedReviewConfigSnapshot(source config.Config) ([]byte, error) {
	// Review submit needs the materialized provider/project policy and the
	// run's hot settings, but none of these process secrets. Auth mode is not
	// consulted by review submit; clear it together with localToken so the
	// sanitized snapshot remains valid when the daemon uses local-token auth.
	snapshot := source
	snapshot.Server.AuthMode = config.AuthModeNone
	snapshot.Server.LocalToken = nil
	snapshot.Agent.Env = nil
	snapshot.Agent.Params = nil
	snapshot.Daemon.Environment = nil

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode trusted review config snapshot: %w", err)
	}
	if len(encoded) > maxTrustedReviewConfigSnapshotSize {
		return nil, fmt.Errorf("trusted review config snapshot exceeds %d bytes", maxTrustedReviewConfigSnapshotSize)
	}
	return encoded, nil
}

// LoadTrustedReviewConfigSnapshot consumes the exact materialized snapshot
// supplied to a proxy child. It intentionally bypasses normal config file,
// environment, and CLI precedence: those layers were already resolved by the
// daemon when it captured the run. Provider credential variables remain in the
// child environment for the selected transport but cannot rewrite this config.
func LoadTrustedReviewConfigSnapshot() (config.LoadedFileConfig, bool, error) {
	if strings.TrimSpace(os.Getenv(trustedReviewProxySkipEnv)) == "" {
		return config.LoadedFileConfig{}, false, nil
	}
	rawFD := strings.TrimSpace(os.Getenv(TrustedReviewConfigFDEnv))
	if rawFD == "" {
		return config.LoadedFileConfig{}, true, fmt.Errorf("trusted review config descriptor is required")
	}
	fd, err := strconv.ParseUint(rawFD, 10, 64)
	if err != nil || fd < 3 {
		return config.LoadedFileConfig{}, true, fmt.Errorf("trusted review config descriptor is invalid")
	}
	file := os.NewFile(uintptr(fd), "trusted-review-config")
	if file == nil {
		return config.LoadedFileConfig{}, true, fmt.Errorf("trusted review config descriptor is unavailable")
	}
	defer file.Close()
	limited := &io.LimitedReader{R: file, N: maxTrustedReviewConfigSnapshotSize + 1}
	raw, err := io.ReadAll(limited)
	if err != nil {
		return config.LoadedFileConfig{}, true, fmt.Errorf("read trusted review config snapshot: %w", err)
	}
	if len(raw) > maxTrustedReviewConfigSnapshotSize {
		return config.LoadedFileConfig{}, true, fmt.Errorf("trusted review config snapshot exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var snapshot config.Config
	if err := decoder.Decode(&snapshot); err != nil {
		return config.LoadedFileConfig{}, true, fmt.Errorf("decode trusted review config snapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return config.LoadedFileConfig{}, true, fmt.Errorf("decode trusted review config snapshot: %w", err)
	}
	if err := config.Validate(snapshot); err != nil {
		return config.LoadedFileConfig{}, true, fmt.Errorf("validate trusted review config snapshot: %w", err)
	}
	return config.LoadedFileConfig{Config: snapshot}, true, nil
}

func trustedReviewProxyChildEnv(trustedEnv map[string]string, configFD int) []string {
	base := os.Environ()
	envMap := make(map[string]string, len(base)+len(trustedEnv)+3)
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		envMap[key] = value
	}
	for key, value := range trustedEnv {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		envMap[key] = value
	}
	// Security controls are applied after every data-environment source. A
	// configured Agent.Env or provider tokenEnv must not retarget the config,
	// re-enter the proxy, or expose a trusted-env file path.
	delete(envMap, TrustedReviewSockEnv)
	delete(envMap, TrustedEnvFileEnv)
	delete(envMap, TrustedReviewConfigFDEnv)
	delete(envMap, "LOOPER_CONFIG")
	envMap[trustedReviewProxySkipEnv] = "1"
	if configFD >= 3 {
		envMap[TrustedReviewConfigFDEnv] = strconv.Itoa(configFD)
	}
	out := make([]string, 0, len(envMap))
	for key, value := range envMap {
		out = append(out, key+"="+value)
	}
	return out
}

// trustedReviewBoundedBuffer captures child stdout/stderr under a hard cap.
// Methods are safe for concurrent use: cmd.Stdout/Stderr copy goroutines may
// still Write after waitCtx cancel unblocks handle.Wait (containment-failure
// path) while the proxy handler reads String/Truncated for the response.
type trustedReviewBoundedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newTrustedReviewBoundedBuffer(limit int) *trustedReviewBoundedBuffer {
	return &trustedReviewBoundedBuffer{limit: limit}
}

func (b *trustedReviewBoundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(p)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		keep := len(p)
		if keep > remaining {
			keep = remaining
		}
		_, _ = b.buffer.Write(p[:keep])
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return written, nil
}

func (b *trustedReviewBoundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *trustedReviewBoundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// TrustedReviewSockConfigured reports whether this process should proxy
// `review submit` through the daemon-side trusted socket.
func TrustedReviewSockConfigured() bool {
	if strings.TrimSpace(os.Getenv(trustedReviewProxySkipEnv)) != "" {
		return false
	}
	return strings.TrimSpace(os.Getenv(TrustedReviewSockEnv)) != ""
}

// ProxyReviewSubmit forwards a review-submit invocation to the trusted proxy.
// On success it writes the proxy stdout/stderr to the current process streams
// and returns a process-style exit error when the proxied command failed.
// The daemon-side listener enforces the per-run allowed PR binding; this client
// only checks the command shape before dialing.
func ProxyReviewSubmit(argv []string, stdin []byte, cwd string) error {
	sockPath := strings.TrimSpace(os.Getenv(TrustedReviewSockEnv))
	if sockPath == "" {
		return fmt.Errorf("trusted review proxy socket is not configured")
	}
	// Client-side shape check only; PR binding is enforced on the daemon proxy
	// where the allowed owner/repo#N is known. Pass empty allowed here so we do
	// not require a second copy of the binding in the agent process.
	if err := validateTrustedReviewProxyArgvShape(argv); err != nil {
		return err
	}

	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial trusted review proxy: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	req := trustedReviewProxyRequest{Argv: argv, Stdin: stdin, Cwd: cwd}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("encode trusted review proxy request: %w", err)
	}
	var resp trustedReviewProxyResponse
	limited := &io.LimitedReader{R: conn, N: maxTrustedReviewProxyResponseBytes + 1}
	if err := json.NewDecoder(limited).Decode(&resp); err != nil {
		if limited.N <= 0 {
			return fmt.Errorf("trusted review proxy response exceeds size limit")
		}
		return fmt.Errorf("decode trusted review proxy response: %w", err)
	}
	if resp.Stdout != "" {
		_, _ = os.Stdout.WriteString(resp.Stdout)
	}
	if resp.Stderr != "" {
		_, _ = os.Stderr.WriteString(resp.Stderr)
	}
	if resp.Error != "" && resp.ExitCode == 0 {
		return fmt.Errorf("trusted review proxy: %s", resp.Error)
	}
	if resp.ExitCode != 0 {
		if resp.Error != "" {
			return fmt.Errorf("trusted review proxy: %s", resp.Error)
		}
		return &proxyExitError{code: resp.ExitCode}
	}
	return nil
}

// validateTrustedReviewProxyArgvShape checks command shape without PR binding
// so the agent-side client can fail fast on clearly invalid argv before dial.
func validateTrustedReviewProxyArgvShape(argv []string) error {
	// Reuse full validator with a synthetic allowed ref extracted from argv when
	// present; if the PR target is missing/malformed the full validator still
	// fails closed for shape. When present, self-match so binding is not enforced
	// client-side (daemon holds the real binding).
	prRef := extractTrustedReviewProxyPRRef(argv)
	if prRef == "" {
		return validateTrustedReviewProxyArgv(argv, "placeholder/repo#1")
	}
	return validateTrustedReviewProxyArgv(argv, prRef)
}

func extractTrustedReviewProxyPRRef(argv []string) string {
	seenReview := false
	seenSubmit := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !seenReview {
			if arg == "review" {
				seenReview = true
				continue
			}
			if strings.HasPrefix(arg, "-") {
				if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") && argv[i+1] != "review" {
					i++
				}
				continue
			}
			return ""
		}
		if !seenSubmit {
			if arg == "submit" {
				seenSubmit = true
				continue
			}
			if strings.HasPrefix(arg, "-") {
				if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
					i++
				}
				continue
			}
			return ""
		}
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
			}
			continue
		}
		return arg
	}
	return ""
}

type proxyExitError struct {
	code int
}

func (e *proxyExitError) Error() string {
	return fmt.Sprintf("review submit exited with code %d", e.code)
}

func (e *proxyExitError) ExitCode() int {
	if e == nil || e.code == 0 {
		return 1
	}
	return e.code
}

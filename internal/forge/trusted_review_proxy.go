package forge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TrustedReviewSockEnv is the agent-facing env key for the trusted review-submit
// proxy socket. The path is not a secret: agents may use the socket only to
// invoke `looper review submit`, and the proxy never returns provider tokens.
// Pattern matches SSH_AUTH_SOCK: a capability channel, not a credential dump.
const TrustedReviewSockEnv = "LOOPER_TRUSTED_REVIEW_SOCK"

// trustedReviewProxySkipEnv marks a proxy-spawned looper child so it does not
// re-enter the proxy (the child receives provider tokens directly).
const trustedReviewProxySkipEnv = "LOOPER_TRUSTED_REVIEW_PROXY_CHILD"

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

// TrustedReviewProxyPolicy is the daemon-selected clean/blocking review event
// policy bound into a trusted review-submit proxy for one agent run.
// Agent-supplied --clean-review-event / --blocking-review-event values are
// stripped and replaced with this policy before the child runs.
type TrustedReviewProxyPolicy struct {
	Clean    string // COMMENT or APPROVE
	Blocking string // COMMENT or REQUEST_CHANGES
}

// StartTrustedReviewProxy listens on a private Unix socket and runs
// `looper review submit` in a daemon-side child with optional provider tokens
// injected. Agents receive only the socket path (via TrustedReviewSockEnv),
// never a secret-bearing wrapper path or LOOPER_TRUSTED_ENV_FILE.
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
// configPath is the daemon-loaded config file path (for example from --config).
// When non-empty it is injected as LOOPER_CONFIG for the child so review submit
// resolves the same provider/project/review policy as the daemon even when the
// path was not present in the daemon process environment. Agent-supplied
// --config remains rejected.
//
// policy is the daemon-selected effective review-events policy for the run
// (including loop-metadata overrides). Agent argv may still include local
// policy flags for the prompted command shape, but the proxy always rewrites
// them to this bound policy before spawning the token-injected child.
func StartTrustedReviewProxy(realLooper string, trustedEnv map[string]string, allowedPRRef, allowedCwd, configPath string, policy TrustedReviewProxyPolicy) (sockPath string, cleanup func(), err error) {
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
	boundConfigPath := strings.TrimSpace(configPath)
	boundPolicy, err := normalizeTrustedReviewProxyPolicy(policy)
	if err != nil {
		return "", nil, fmt.Errorf("trusted review proxy review policy: %w", err)
	}
	if _, err := os.Stat(realLooper); err != nil {
		return "", nil, fmt.Errorf("stat real looper path: %w", err)
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
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				handleTrustedReviewProxyConn(c, realLooper, trustedEnv, normalizedAllowed, boundCwd, boundConfigPath, boundPolicy)
			}(conn)
		}
	}()

	cleanup = func() {
		close(stop)
		_ = listener.Close()
		wg.Wait()
		_ = os.RemoveAll(dir)
	}
	return sockPath, cleanup, nil
}

func handleTrustedReviewProxyConn(conn net.Conn, realLooper string, trustedEnv map[string]string, allowedPRRef, allowedCwd, configPath string, policy TrustedReviewProxyPolicy) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	var req trustedReviewProxyRequest
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: "decode trusted review proxy request: " + err.Error()})
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
	// Never honor request-supplied cwd: project/provider resolution for
	// provider-qualified same-owner/repo checkouts is CWD-sensitive. The child
	// always runs in the daemon-selected worktree bound at proxy start.
	cmd.Dir = allowedCwd
	cmd.Env = trustedReviewProxyChildEnv(trustedEnv, configPath)
	cmd.Stdin = bytes.NewReader(req.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	resp := trustedReviewProxyResponse{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			resp.ExitCode = 1
			resp.Error = err.Error()
		}
	}
	_ = json.NewEncoder(conn).Encode(resp)
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
	return TrustedReviewProxyPolicy{Clean: clean, Blocking: blocking}, nil
}

// applyTrustedReviewProxyPolicy strips agent-supplied local review-policy flags
// and injects the daemon-bound clean/blocking events so the child validates
// markers against daemon-selected policy only.
func applyTrustedReviewProxyPolicy(argv []string, policy TrustedReviewProxyPolicy) []string {
	stripped := stripTrustedReviewProxyPolicyFlags(argv)
	return append(stripped,
		"--clean-review-event", policy.Clean,
		"--blocking-review-event", policy.Blocking,
	)
}

func stripTrustedReviewProxyPolicyFlags(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name := trustedReviewProxyFlagName(arg)
		if name == "clean-review-event" || name == "blocking-review-event" {
			if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

func trustedReviewProxyChildEnv(trustedEnv map[string]string, configPath string) []string {
	base := os.Environ()
	envMap := make(map[string]string, len(base)+len(trustedEnv)+3)
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		envMap[key] = value
	}
	// Prevent proxy re-entry and never expose a trusted-env file path to the child
	// beyond the direct token keys the daemon already holds in memory.
	delete(envMap, TrustedReviewSockEnv)
	delete(envMap, TrustedEnvFileEnv)
	envMap[trustedReviewProxySkipEnv] = "1"
	// Daemon-loaded config path wins over ambient LOOPER_CONFIG so children use
	// the same config as looperd when it was started with --config only.
	if path := strings.TrimSpace(configPath); path != "" {
		envMap["LOOPER_CONFIG"] = path
	}
	for key, value := range trustedEnv {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		envMap[key] = value
	}
	out := make([]string, 0, len(envMap))
	for key, value := range envMap {
		out = append(out, key+"="+value)
	}
	return out
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
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
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

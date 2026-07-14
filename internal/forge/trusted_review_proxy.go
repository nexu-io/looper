package forge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

// StartTrustedReviewProxy listens on a private Unix socket and runs
// `looper review submit` in a daemon-side child with provider tokens injected.
// Agents receive only the socket path (via TrustedReviewSockEnv), never a
// secret-bearing wrapper path or LOOPER_TRUSTED_ENV_FILE.
func StartTrustedReviewProxy(realLooper string, trustedEnv map[string]string) (sockPath string, cleanup func(), err error) {
	realLooper = strings.TrimSpace(realLooper)
	noop := func() {}
	if realLooper == "" {
		return "", nil, fmt.Errorf("real looper path is required for trusted review proxy")
	}
	if len(trustedEnv) == 0 {
		return "", noop, nil
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
				handleTrustedReviewProxyConn(c, realLooper, trustedEnv)
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

func handleTrustedReviewProxyConn(conn net.Conn, realLooper string, trustedEnv map[string]string) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	var req trustedReviewProxyRequest
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: "decode trusted review proxy request: " + err.Error()})
		return
	}
	if err := validateTrustedReviewProxyArgv(req.Argv); err != nil {
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	cmd := exec.Command(realLooper, req.Argv...)
	if strings.TrimSpace(req.Cwd) != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = trustedReviewProxyChildEnv(trustedEnv)
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

func validateTrustedReviewProxyArgv(argv []string) error {
	// Allow global flags before the "review submit" command path.
	// Reject anything that is not a review-submit invocation so the proxy
	// cannot be abused to run arbitrary looper subcommands with provider tokens.
	seenReview := false
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
		if arg == "submit" {
			return nil
		}
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
			}
			continue
		}
		return fmt.Errorf("trusted review proxy only allows `looper review submit`")
	}
	return fmt.Errorf("trusted review proxy only allows `looper review submit`")
}

func trustedReviewProxyChildEnv(trustedEnv map[string]string) []string {
	base := os.Environ()
	envMap := make(map[string]string, len(base)+len(trustedEnv)+2)
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
func ProxyReviewSubmit(argv []string, stdin []byte, cwd string) error {
	sockPath := strings.TrimSpace(os.Getenv(TrustedReviewSockEnv))
	if sockPath == "" {
		return fmt.Errorf("trusted review proxy socket is not configured")
	}
	if err := validateTrustedReviewProxyArgv(argv); err != nil {
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

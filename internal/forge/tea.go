package forge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
)

// TeaAuthError is a typed, actionable failure for tea-backed Forgejo auth.
// Code values are stable health/status tokens: tea_missing, tea_login_missing,
// tea_login_host_mismatch, tea_auth_failed.
type TeaAuthError struct {
	Code    string
	Message string
}

func (err *TeaAuthError) Error() string {
	if err == nil {
		return ""
	}
	if err.Code == "" {
		return err.Message
	}
	if err.Message == "" {
		return err.Code
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

const (
	TeaErrorMissing           = "tea_missing"
	TeaErrorLoginMissing      = "tea_login_missing"
	TeaErrorLoginHostMismatch = "tea_login_host_mismatch"
	TeaErrorAuthFailed        = "tea_auth_failed"
	maxTeaCommandOutputBytes  = maxForgejoResponseBodyBytes + 64*1024
	defaultTeaCommandTimeout  = defaultForgejoTimeout
	teaLoginsListTimeout      = 5 * time.Second
)

// TeaLogin describes a tea CLI login entry from `tea logins list -o json`.
// Tokens are never present in this output and must never be requested.
// Default accepts both JSON bool and string forms: tea 0.14.x emits
// "default":"true", while some builds emit a real boolean.
type TeaLogin struct {
	Name    string         `json:"name"`
	URL     string         `json:"url"`
	SSHHost string         `json:"ssh_host"`
	User    string         `json:"user"`
	Default teaDefaultFlag `json:"default"`
}

// teaDefaultFlag unmarshals tea's login "default" field, which is a bool in
// some tea builds and a string ("true"/"false") in Homebrew tea 0.14.x.
type teaDefaultFlag bool

func (f *teaDefaultFlag) UnmarshalJSON(data []byte) error {
	if f == nil {
		return errors.New("teaDefaultFlag: nil receiver")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = false
		return nil
	}
	var asBool bool
	if err := json.Unmarshal(trimmed, &asBool); err == nil {
		*f = teaDefaultFlag(asBool)
		return nil
	}
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err != nil {
		return fmt.Errorf("tea login default: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(asString)) {
	case "true", "1", "yes":
		*f = true
	case "false", "0", "no", "":
		*f = false
	default:
		return fmt.Errorf("tea login default: unsupported value %q", asString)
	}
	return nil
}

func (f teaDefaultFlag) Bool() bool { return bool(f) }

// TeaCommandRunner executes tea CLI invocations. Tests inject a fake runner.
type TeaCommandRunner interface {
	Run(ctx context.Context, teaPath string, args []string, stdin string, timeout time.Duration) (shell.Result, error)
}

type defaultTeaRunner struct{}

func (defaultTeaRunner) Run(ctx context.Context, teaPath string, args []string, stdin string, timeout time.Duration) (shell.Result, error) {
	return shell.Run(ctx, shell.Options{
		Command:          teaPath,
		Args:             args,
		Stdin:            stdin,
		Timeout:          timeout,
		MaxCapturedBytes: maxTeaCommandOutputBytes,
	})
}

// ResolveTeaPath returns an absolute tea executable path from config or PATH.
func ResolveTeaPath(provider config.ProviderConfig, lookPath func(string) (string, error)) (string, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if provider.TeaPath != nil && strings.TrimSpace(*provider.TeaPath) != "" {
		path := strings.TrimSpace(*provider.TeaPath)
		if _, err := os.Stat(path); err != nil {
			return "", &TeaAuthError{Code: TeaErrorMissing, Message: fmt.Sprintf("configured teaPath %q is not available", path)}
		}
		return path, nil
	}
	resolved, err := lookPath("tea")
	if err != nil || strings.TrimSpace(resolved) == "" {
		return "", &TeaAuthError{Code: TeaErrorMissing, Message: "tea CLI not found on PATH; install tea or set providers[].teaPath"}
	}
	return resolved, nil
}

// ListTeaLogins runs `tea logins list -o json` and returns login metadata only.
// It never reads tea's config file or tokens.
func ListTeaLogins(ctx context.Context, teaPath string, runner TeaCommandRunner) ([]TeaLogin, error) {
	if runner == nil {
		runner = defaultTeaRunner{}
	}
	result, err := runner.Run(ctx, teaPath, []string{"logins", "list", "-o", "json"}, "", teaLoginsListTimeout)
	if err != nil {
		if isCommandNotFound(err) {
			return nil, &TeaAuthError{Code: TeaErrorMissing, Message: "tea CLI not found"}
		}
		return nil, &TeaAuthError{Code: TeaErrorAuthFailed, Message: "failed to list tea logins"}
	}
	if result.ExitCode != 0 {
		return nil, &TeaAuthError{Code: TeaErrorAuthFailed, Message: sanitizeTeaCLIError(result.Stderr, result.Stdout)}
	}
	var logins []TeaLogin
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &logins); err != nil {
		return nil, &TeaAuthError{Code: TeaErrorAuthFailed, Message: "tea logins list returned invalid JSON"}
	}
	return logins, nil
}

// FindTeaLogin returns the login with the given name, or tea_login_missing.
func FindTeaLogin(logins []TeaLogin, name string) (TeaLogin, error) {
	want := strings.TrimSpace(name)
	for _, login := range logins {
		if strings.TrimSpace(login.Name) == want {
			return login, nil
		}
	}
	return TeaLogin{}, &TeaAuthError{Code: TeaErrorLoginMissing, Message: fmt.Sprintf("tea login %q is not configured; run tea login add", want)}
}

// MatchTeaLoginHost reports whether the tea login URL matches the provider base URL.
func MatchTeaLoginHost(loginURL, providerBaseURL string) bool {
	loginBase, err := parseForgejoBaseURL(loginURL)
	if err != nil {
		return false
	}
	providerBase, err := parseForgejoBaseURL(providerBaseURL)
	if err != nil {
		return false
	}
	return forgejoBaseURLsEqual(loginBase, providerBase)
}

func forgejoBaseURLsEqual(first, second *url.URL) bool {
	if first == nil || second == nil {
		return false
	}
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(first.Hostname(), second.Hostname()) &&
		normalizeHTTPPort(first.Scheme, first.Port()) == normalizeHTTPPort(second.Scheme, second.Port()) &&
		strings.TrimRight(first.Path, "/") == strings.TrimRight(second.Path, "/")
}

func normalizeHTTPPort(scheme, port string) string {
	if (strings.EqualFold(scheme, "https") && (port == "" || port == "443")) ||
		(strings.EqualFold(scheme, "http") && (port == "" || port == "80")) {
		return ""
	}
	return port
}

// ValidateTeaLoginForProvider lists tea logins and requires an explicit login
// whose URL matches the provider base URL. Never uses tea's default login.
func ValidateTeaLoginForProvider(ctx context.Context, provider config.ProviderConfig, runner TeaCommandRunner, lookPath func(string) (string, error)) (teaPath string, login TeaLogin, err error) {
	teaPath, err = ResolveTeaPath(provider, lookPath)
	if err != nil {
		return "", TeaLogin{}, err
	}
	if provider.TeaLogin == nil || strings.TrimSpace(*provider.TeaLogin) == "" {
		return "", TeaLogin{}, &TeaAuthError{Code: TeaErrorLoginMissing, Message: "teaLogin is required when auth is tea"}
	}
	loginName := strings.TrimSpace(*provider.TeaLogin)
	logins, err := ListTeaLogins(ctx, teaPath, runner)
	if err != nil {
		return "", TeaLogin{}, err
	}
	login, err = FindTeaLogin(logins, loginName)
	if err != nil {
		return "", TeaLogin{}, err
	}
	if !MatchTeaLoginHost(login.URL, provider.BaseURL) {
		return "", TeaLogin{}, &TeaAuthError{
			Code:    TeaErrorLoginHostMismatch,
			Message: fmt.Sprintf("tea login %q URL does not match provider baseUrl", loginName),
		}
	}
	return teaPath, login, nil
}

// MatchingTeaLogins returns tea logins whose URL matches baseURL (for discovery UX).
func MatchingTeaLogins(ctx context.Context, baseURL, teaPath string, runner TeaCommandRunner) ([]TeaLogin, error) {
	if strings.TrimSpace(teaPath) == "" {
		resolved, err := exec.LookPath("tea")
		if err != nil {
			return nil, &TeaAuthError{Code: TeaErrorMissing, Message: "tea CLI not found on PATH"}
		}
		teaPath = resolved
	}
	logins, err := ListTeaLogins(ctx, teaPath, runner)
	if err != nil {
		return nil, err
	}
	matches := make([]TeaLogin, 0)
	for _, login := range logins {
		if MatchTeaLoginHost(login.URL, baseURL) {
			matches = append(matches, login)
		}
	}
	return matches, nil
}

type teaTransport struct {
	teaPath string
	login   string
	timeout time.Duration
	runner  TeaCommandRunner
	baseURL *url.URL
}

func newTeaTransport(teaPath, login string, baseURL *url.URL, timeout time.Duration, runner TeaCommandRunner) *teaTransport {
	if runner == nil {
		runner = defaultTeaRunner{}
	}
	if timeout <= 0 {
		timeout = defaultTeaCommandTimeout
	}
	return &teaTransport{
		teaPath: teaPath,
		login:   login,
		timeout: timeout,
		runner:  runner,
		baseURL: baseURL,
	}
}

func (t *teaTransport) doRaw(ctx context.Context, method string, path string, query url.Values, payload any) (rawResponse, error) {
	endpoint := buildTeaAPIEndpoint(path, query)
	args := []string{"api", "--login", t.login, "-i", "-X", method, endpoint}
	stdin := ""
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return rawResponse{}, fmt.Errorf("forgejo tea API encode %s %s: %w", method, path, err)
		}
		stdin = string(encoded)
		args = append(args, "-d", "@-")
	}
	result, err := t.runner.Run(ctx, t.teaPath, args, stdin, t.timeout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return rawResponse{}, err
		}
		var execErr *shell.CommandExecutionError
		if errors.As(err, &execErr) {
			if strings.Contains(strings.ToLower(execErr.Message), "timed out") {
				return rawResponse{}, fmt.Errorf("forgejo tea API %s %s timed out", method, path)
			}
			result = execErr.Result
		} else if isCommandNotFound(err) {
			return rawResponse{}, &TeaAuthError{Code: TeaErrorMissing, Message: "tea CLI not found"}
		} else {
			return rawResponse{}, fmt.Errorf("forgejo tea API %s %s failed: %w", method, path, err)
		}
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return rawResponse{}, fmt.Errorf("forgejo tea API %s %s response exceeds %d bytes", method, path, maxForgejoResponseBodyBytes)
	}
	if result.ExitCode != 0 {
		return rawResponse{}, classifyTeaAPIFailure(method, path, result)
	}
	statusCode, headers, err := parseTeaIncludeHeaders(result.Stderr)
	if err != nil {
		// Some tea versions may put status on stderr only when -i is used; if
		// headers are missing but body is present and exit is 0, treat as 200.
		if strings.TrimSpace(result.Stderr) == "" && len(bytes.TrimSpace([]byte(result.Stdout))) > 0 {
			statusCode = http.StatusOK
			headers = make(http.Header)
		} else {
			return rawResponse{}, fmt.Errorf("forgejo tea API %s %s: %w", method, path, err)
		}
	}
	body := []byte(result.Stdout)
	if len(body) > maxForgejoResponseBodyBytes {
		return rawResponse{}, fmt.Errorf("forgejo tea API %s %s response exceeds %d bytes", method, path, maxForgejoResponseBodyBytes)
	}
	if statusCode < 200 || statusCode >= 300 {
		return rawResponse{}, &ForgejoHTTPError{
			Method:     method,
			Path:       path,
			StatusCode: statusCode,
			Message:    sanitizeForgejoErrorBody(body, ""),
		}
	}
	return rawResponse{body: body, header: headers}, nil
}

func buildTeaAPIEndpoint(path string, query url.Values) string {
	trimmed := strings.TrimSpace(path)
	var endpoint string
	switch {
	case strings.HasPrefix(trimmed, "http://"), strings.HasPrefix(trimmed, "https://"), strings.HasPrefix(trimmed, "/api/"):
		// Absolute URLs and /api/* paths are left alone so tea does not re-prefix /api/v1.
		endpoint = trimmed
	default:
		endpoint = "/" + strings.TrimLeft(trimmed, "/")
	}
	if encoded := query.Encode(); encoded != "" {
		if strings.Contains(endpoint, "?") {
			endpoint += "&" + encoded
		} else {
			endpoint += "?" + encoded
		}
	}
	return endpoint
}

func parseTeaIncludeHeaders(stderr string) (int, http.Header, error) {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return 0, nil, errors.New("tea -i produced no status/headers on stderr")
	}
	scanner := bufio.NewScanner(strings.NewReader(trimmed))
	scanner.Buffer(make([]byte, 0, 64*1024), maxTeaCommandOutputBytes)
	if !scanner.Scan() {
		return 0, nil, errors.New("tea -i status line missing")
	}
	statusLine := strings.TrimSpace(scanner.Text())
	// HTTP/1.1 200 OK
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, nil, fmt.Errorf("tea -i status line invalid: %q", truncateForError(statusLine, 80))
	}
	var statusCode int
	if _, err := fmt.Sscanf(parts[1], "%d", &statusCode); err != nil || statusCode <= 0 {
		return 0, nil, fmt.Errorf("tea -i status code invalid: %q", truncateForError(statusLine, 80))
	}
	headers := make(http.Header)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			break
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		headers.Add(key, value)
	}
	return statusCode, headers, nil
}

func classifyTeaAPIFailure(method, path string, result shell.Result) error {
	combined := strings.TrimSpace(result.Stderr + "\n" + result.Stdout)
	lower := strings.ToLower(combined)
	switch {
	case strings.Contains(lower, "does not exist") && strings.Contains(lower, "login"):
		return &TeaAuthError{Code: TeaErrorLoginMissing, Message: sanitizeTeaCLIError(result.Stderr, result.Stdout)}
	case strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401") || strings.Contains(lower, "bad credentials") || strings.Contains(lower, "authentication failed"):
		return &TeaAuthError{Code: TeaErrorAuthFailed, Message: "tea authentication failed for the selected login"}
	case strings.Contains(lower, "forbidden") || strings.Contains(lower, "403"):
		return &ForgejoHTTPError{Method: method, Path: path, StatusCode: http.StatusForbidden, Message: sanitizeTeaCLIError(result.Stderr, result.Stdout)}
	default:
		// Try parse -i headers even on non-zero exit.
		if statusCode, _, err := parseTeaIncludeHeaders(result.Stderr); err == nil && statusCode >= 400 {
			return &ForgejoHTTPError{
				Method:     method,
				Path:       path,
				StatusCode: statusCode,
				Message:    sanitizeForgejoErrorBody([]byte(result.Stdout), ""),
			}
		}
		return &TeaAuthError{Code: TeaErrorAuthFailed, Message: sanitizeTeaCLIError(result.Stderr, result.Stdout)}
	}
}

func sanitizeTeaCLIError(stderr, stdout string) string {
	message := strings.TrimSpace(stderr)
	if message == "" {
		message = strings.TrimSpace(stdout)
	}
	message = strings.TrimPrefix(message, "Error: ")
	message = strings.TrimSpace(message)
	if message == "" {
		return "tea command failed"
	}
	// Defensive redaction of common token-shaped substrings; tea should not
	// print tokens, but Looper must never surface them if it does.
	message = redactTokenLike(message)
	return truncateForError(message, 240)
}

func redactTokenLike(message string) string {
	// Long opaque tokens (gitea/forgejo tokens are typically 40+ hex/alnum).
	var b strings.Builder
	for _, field := range strings.Fields(message) {
		if len(field) >= 32 && isMostlyTokenCharset(field) {
			b.WriteString("[REDACTED]")
		} else {
			b.WriteString(field)
		}
		b.WriteByte(' ')
	}
	return strings.TrimSpace(b.String())
}

func isMostlyTokenCharset(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func truncateForError(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max] + "…"
}

func isCommandNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && errors.Is(pathErr.Err, os.ErrNotExist) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "executable file not found") || strings.Contains(message, "no such file")
}

// TeaPathBaseName returns only the binary basename for safe logging/display.
func TeaPathBaseName(path string) string {
	return filepath.Base(strings.TrimSpace(path))
}

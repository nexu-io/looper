package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
)

type recordingTeaRunner struct {
	mu          sync.Mutex
	calls       []teaCall
	loginsJSON  string
	apiHandlers map[string]teaAPIResponse // key: method+" "+endpoint
	defaultAPI  *teaAPIResponse
	failStart   error
	delay       time.Duration
}

type teaCall struct {
	Path  string
	Args  []string
	Stdin string
}

type teaAPIResponse struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

func (r *recordingTeaRunner) Run(ctx context.Context, teaPath string, args []string, stdin string, timeout time.Duration) (shell.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, teaCall{Path: teaPath, Args: append([]string(nil), args...), Stdin: stdin})
	delay := r.delay
	r.mu.Unlock()

	if r.failStart != nil {
		return shell.Result{}, r.failStart
	}
	if delay > 0 {
		select {
		case <-ctx.Done():
			return shell.Result{}, ctx.Err()
		case <-time.After(delay):
		}
	}

	if len(args) >= 1 && args[0] == "logins" {
		return shell.Result{ExitCode: 0, Stdout: r.loginsJSON}, nil
	}
	if len(args) >= 1 && args[0] == "api" {
		method := "GET"
		endpoint := ""
		for i := 0; i < len(args); i++ {
			if args[i] == "-X" && i+1 < len(args) {
				method = args[i+1]
			}
		}
		// endpoint is the first non-flag arg after "api"
		for i := 1; i < len(args); i++ {
			arg := args[i]
			if arg == "-X" || arg == "--method" || arg == "--login" || arg == "-l" || arg == "-d" || arg == "--data" {
				i++
				continue
			}
			if strings.HasPrefix(arg, "-") {
				continue
			}
			endpoint = arg
			break
		}
		key := method + " " + endpoint
		r.mu.Lock()
		resp, ok := r.apiHandlers[key]
		if !ok && r.defaultAPI != nil {
			resp = *r.defaultAPI
			ok = true
		}
		r.mu.Unlock()
		if !ok {
			return shell.Result{ExitCode: 1, Stderr: "unexpected tea api " + key}, nil
		}
		if resp.Err != nil {
			return shell.Result{}, resp.Err
		}
		return shell.Result{ExitCode: resp.ExitCode, Stdout: resp.Stdout, Stderr: resp.Stderr}, nil
	}
	return shell.Result{ExitCode: 1, Stderr: "unexpected tea args: " + strings.Join(args, " ")}, nil
}

func (r *recordingTeaRunner) callsSnapshot() []teaCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]teaCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestNewForgejoClientFromConfigTeaUsesSelectedLogin(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{
			{Name: "default-login", URL: "https://code.example.com", Default: "true"},
			{Name: "selected-login", URL: "https://code.example.com", Default: "false"},
			{Name: "other-host", URL: "https://other.example.com", Default: "false"},
		}),
		apiHandlers: map[string]teaAPIResponse{
			"GET /user": {
				Stdout:   `{"id":7,"login":"alice"}`,
				Stderr:   "HTTP/1.1 200 OK\nContent-Type: application/json\n\n",
				ExitCode: 0,
			},
			"POST /repos/acme/looper/issues/1/comments": {
				Stdout:   `{"id":99,"body":"hello","html_url":"https://code.example.com/comments/99","updated_at":"2026-07-14T00:00:00Z","user":{"id":7,"login":"alice"}}`,
				Stderr:   "HTTP/1.1 201 Created\nContent-Type: application/json\n\n",
				ExitCode: 0,
			},
			"GET /repos/acme/looper/issues?limit=50&page=1&state=open": {
				Stdout:   `[{"number":1,"title":"one","body":"","state":"open","html_url":"https://x/1","updated_at":"2026-07-14T00:00:00Z","user":{"id":1,"login":"bob"}}]`,
				Stderr:   "HTTP/1.1 200 OK\nLink: </repos/acme/looper/issues?page=2>; rel=\"next\"\nX-Total-Pages: 2\n\n",
				ExitCode: 0,
			},
			"GET /repos/acme/looper/issues?limit=50&page=2&state=open": {
				Stdout:   `[{"number":2,"title":"two","body":"","state":"open","html_url":"https://x/2","updated_at":"2026-07-14T00:00:00Z","user":{"id":2,"login":"cara"}}]`,
				Stderr:   "HTTP/1.1 200 OK\n\n",
				ExitCode: 0,
			},
		},
	}

	auth := config.ProviderAuthTea
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: auth, TeaLogin: stringPtr("selected-login"),
	}
	client, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner))
	if err != nil {
		t.Fatalf("NewForgejoClientFromConfig() error = %v", err)
	}

	ctx := context.Background()
	user, err := client.CurrentUser(ctx)
	if err != nil || user.Login != "alice" {
		t.Fatalf("CurrentUser() = %#v, %v", user, err)
	}
	comment, err := client.CreateIssueComment(ctx, CreateCommentInput{IssueNumber: 1, Body: "hello"})
	if err != nil || comment.ID != 99 {
		t.Fatalf("CreateIssueComment() = %#v, %v", comment, err)
	}
	issues, err := client.ListOpenIssues(ctx, ListIssuesInput{})
	if err != nil || len(issues) != 2 {
		t.Fatalf("ListOpenIssues() = %#v, %v", issues, err)
	}

	calls := runner.callsSnapshot()
	var apiCalls []teaCall
	for _, call := range calls {
		if len(call.Args) > 0 && call.Args[0] == "api" {
			apiCalls = append(apiCalls, call)
			if !containsArg(call.Args, "--login", "selected-login") {
				t.Fatalf("tea api call missing selected login: %#v", call.Args)
			}
			if containsArg(call.Args, "--login", "default-login") {
				t.Fatalf("tea api used default login: %#v", call.Args)
			}
		}
	}
	if len(apiCalls) < 3 {
		t.Fatalf("api calls = %d, want at least 3", len(apiCalls))
	}
	// POST body via stdin
	var post teaCall
	for _, call := range apiCalls {
		if containsArgValue(call.Args, "-X", "POST") {
			post = call
			break
		}
	}
	if post.Stdin == "" || !strings.Contains(post.Stdin, `"body":"hello"`) {
		t.Fatalf("POST stdin = %q, want JSON body", post.Stdin)
	}
	if !containsArgValue(post.Args, "-d", "@-") {
		t.Fatalf("POST args = %#v, want -d @-", post.Args)
	}
	for _, call := range calls {
		joined := strings.Join(call.Args, " ") + " " + call.Stdin
		if strings.Contains(joined, "super-secret") || strings.Contains(strings.ToLower(joined), "token=") {
			t.Fatalf("tea invocation leaked token material: %#v", call)
		}
	}
}

func TestNewForgejoClientFromConfigTeaLoginHostMismatch(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://other.example.com"}}),
	}
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	_, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner))
	if err == nil {
		t.Fatal("expected host mismatch error")
	}
	var teaErr *TeaAuthError
	if !errors.As(err, &teaErr) || teaErr.Code != TeaErrorLoginHostMismatch {
		t.Fatalf("error = %v, want tea_login_host_mismatch", err)
	}
}

func TestNewForgejoClientFromConfigTeaLoginMissing(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "other", URL: "https://code.example.com"}}),
	}
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	_, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner))
	var teaErr *TeaAuthError
	if !errors.As(err, &teaErr) || teaErr.Code != TeaErrorLoginMissing {
		t.Fatalf("error = %v, want tea_login_missing", err)
	}
}

func TestNewForgejoClientFromConfigTeaMissingBinary(t *testing.T) {
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	_, err := NewForgejoClientFromConfig(provider, "acme/looper", WithLookPath(func(string) (string, error) {
		return "", errors.New("not found")
	}))
	var teaErr *TeaAuthError
	if !errors.As(err, &teaErr) || teaErr.Code != TeaErrorMissing {
		t.Fatalf("error = %v, want tea_missing", err)
	}
}

func TestTeaTransportAuthFailedAndRedaction(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://code.example.com"}}),
		apiHandlers: map[string]teaAPIResponse{
			"GET /user": {
				Stdout:   "",
				Stderr:   "Error: unauthorized token abcdefghijklmnopqrstuvwxyz0123456789secret rejected",
				ExitCode: 1,
			},
		},
	}
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	client, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = client.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwxyz0123456789secret") {
		t.Fatalf("error leaked token-like material: %v", err)
	}
	var teaErr *TeaAuthError
	if !errors.As(err, &teaErr) || teaErr.Code != TeaErrorAuthFailed {
		t.Fatalf("error = %v, want tea_auth_failed", err)
	}
}

func TestTeaTransportCancellation(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://code.example.com"}}),
		delay:      200 * time.Millisecond,
		defaultAPI: &teaAPIResponse{Stdout: `{}`, Stderr: "HTTP/1.1 200 OK\n\n", ExitCode: 0},
	}
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	client, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.CurrentUser(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		// tea runner returns ctx.Err on cancel during delay; if cancel already done before call, may still race.
		if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "cancel")) {
			t.Fatalf("CurrentUser() error = %v, want canceled", err)
		}
	}
}

func TestTeaTransportHTTPStatusError(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://code.example.com"}}),
		apiHandlers: map[string]teaAPIResponse{
			"GET /repos/acme/looper/issues/1": {
				Stdout:   `{"message":"not found"}`,
				Stderr:   "HTTP/1.1 404 Not Found\nContent-Type: application/json\n\n",
				ExitCode: 0,
			},
		},
	}
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	client, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = client.ViewIssue(context.Background(), 1)
	var httpErr *ForgejoHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		t.Fatalf("error = %v, want HTTP 404", err)
	}
}

func TestProbeForgejoProviderTeaStates(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://code.example.com"}}),
		apiHandlers: map[string]teaAPIResponse{
			"GET /user": {
				Stdout:   `{"id":1,"login":"alice"}`,
				Stderr:   "HTTP/1.1 200 OK\n\n",
				ExitCode: 0,
			},
			"GET /repos/acme/looper": {
				Stdout:   `{"permissions":{"admin":true,"push":true,"pull":true}}`,
				Stderr:   "HTTP/1.1 200 OK\n\n",
				ExitCode: 0,
			},
		},
	}
	// Use a non-routable base so unauthenticated HTTP version probe fails closed without hanging long.
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	// Force tea path via options; version probe may be unreachable but auth should still validate via tea.
	health := ProbeForgejoProvider(context.Background(), provider, []ForgejoProbeProject{{ID: "p", Repo: "acme/looper"}}, WithTeaRunner(runner), WithTimeout(50*time.Millisecond))
	if health.Authentication != AuthenticationValid {
		t.Fatalf("authentication = %s, want valid (identity=%v)", health.Authentication, health.Identity)
	}
	if health.Identity == nil || health.Identity.Login != "alice" {
		t.Fatalf("identity = %#v", health.Identity)
	}
	if len(health.Projects) != 1 || health.Projects[0].Access != AccessWritable {
		t.Fatalf("projects = %#v", health.Projects)
	}
}

func TestMatchTeaLoginHostNormalizes(t *testing.T) {
	if !MatchTeaLoginHost("https://Code.Example.com/", "https://code.example.com") {
		t.Fatal("expected host match with case/path normalization")
	}
	if MatchTeaLoginHost("https://code.example.com", "https://other.example.com") {
		t.Fatal("expected host mismatch")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func containsArg(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsArgValue(args []string, key, value string) bool {
	return containsArg(args, key, value)
}

func TestBuildTeaAPIEndpoint(t *testing.T) {
	got := buildTeaAPIEndpoint("repos/acme/looper/issues", map[string][]string{"state": {"open"}, "page": {"1"}})
	if !strings.HasPrefix(got, "/repos/acme/looper/issues?") || !strings.Contains(got, "state=open") || !strings.Contains(got, "page=1") {
		t.Fatalf("endpoint = %q", got)
	}
	abs := buildTeaAPIEndpoint("https://code.example.com/swagger.v1.json", nil)
	if abs != "https://code.example.com/swagger.v1.json" {
		t.Fatalf("absolute endpoint = %q", abs)
	}
}

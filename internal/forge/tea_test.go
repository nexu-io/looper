package forge

import (
	"context"
	"encoding/json"
	"errors"
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

// fakeTeaLookPath satisfies ResolveTeaPath without requiring a real tea binary.
// Pair with WithTeaRunner so CI hosts without tea installed stay hermetic.
func fakeTeaLookPath(string) (string, error) {
	return "/usr/bin/fake-tea", nil
}

func TestNewForgejoClientFromConfigTeaUsesSelectedLogin(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{
			{Name: "default-login", URL: "https://code.example.com", Default: true},
			{Name: "selected-login", URL: "https://code.example.com", Default: false},
			{Name: "other-host", URL: "https://other.example.com", Default: false},
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
	client, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner), WithLookPath(fakeTeaLookPath))
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
	_, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner), WithLookPath(fakeTeaLookPath))
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
	_, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner), WithLookPath(fakeTeaLookPath))
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

func TestListTeaLoginsAcceptsStringDefaultFromTea014(t *testing.T) {
	// Homebrew tea 0.14.x emits "default":"true" (string), not a JSON bool.
	// Regression: unmarshaling that form used to fail as tea_auth_failed.
	runner := &recordingTeaRunner{
		loginsJSON: `[
  {
    "name": "powerformer-code",
    "url": "https://code.powerformer.net",
    "ssh_host": "code.powerformer.net",
    "user": "mrcfps",
    "default": "true"
  },
  {
    "name": "other",
    "url": "https://other.example.com",
    "ssh_host": "other.example.com",
    "user": "bob",
    "default": false
  }
]`,
	}
	logins, err := ListTeaLogins(context.Background(), "/usr/bin/fake-tea", runner)
	if err != nil {
		t.Fatalf("ListTeaLogins() error = %v", err)
	}
	if len(logins) != 2 {
		t.Fatalf("len(logins) = %d, want 2", len(logins))
	}
	if logins[0].Name != "powerformer-code" || !logins[0].Default.Bool() {
		t.Fatalf("logins[0] = %#v, want powerformer-code default=true", logins[0])
	}
	if logins[1].Name != "other" || logins[1].Default.Bool() {
		t.Fatalf("logins[1] = %#v, want other default=false", logins[1])
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

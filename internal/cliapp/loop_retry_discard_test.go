package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestLoopRetryDiscardWorktreeChangesRequiresConfirm(t *testing.T) {
	t.Parallel()

	configPath := writeCLIConfig(t, "http://127.0.0.1:1", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})
	args := []string{"--config", configPath, "loop", "retry", "3108", "--discard-worktree-changes"}
	exitCode := app.Run(context.Background(), args)
	if exitCode == 0 {
		t.Fatalf("Run() exit code = 0, want failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--discard-worktree-changes requires --confirm") {
		t.Fatalf("stderr = %q, want confirm requirement error", stderr.String())
	}
}

func TestLoopRetryDiscardWorktreeChangesWiresAPIBodyAndHumanOutput(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/loops/3108/retry" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("unmarshal body: %v body=%q", err, raw)
		}
		writeEnvelope(t, w, pkgapi.Success("req_retry_discard", map[string]any{
			"loop": map[string]any{
				"id": "loop_retry_discard", "seq": 3108, "projectId": "project_1",
				"type": "fixer", "targetType": "pull_request", "status": "queued",
			},
			"queueItemId":            "queue_new",
			"mode":                   "auto",
			"resetAttempts":          true,
			"discardWorktreeChanges": true,
			"worktreeDiscard": map[string]any{
				"worktreePath": "/tmp/managed/wt",
				"discarded":    true,
				"noOp":         false,
				"reason":       "discarded",
			},
		}))
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})
	args := []string{"--config", configPath, "loop", "retry", "3108", "--discard-worktree-changes", "--confirm"}
	if exitCode := app.Run(context.Background(), args); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if gotBody["discardWorktreeChanges"] != true {
		t.Fatalf("request body = %#v, want discardWorktreeChanges=true", gotBody)
	}
	if gotBody["mode"] != "auto" || gotBody["resetAttempts"] != true {
		t.Fatalf("request body = %#v, want mode=auto resetAttempts=true", gotBody)
	}
	out := stdout.String()
	for _, needle := range []string{"Loop retry queued", "discardWorktreeChanges", "/tmp/managed/wt", "worktreeDiscardNoOp"} {
		if !strings.Contains(out, needle) {
			t.Fatalf("stdout missing %q\n%s", needle, out)
		}
	}
}

func TestLoopRetryDirtyWorktreePromptYesDiscards(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	var sawWorktree bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3491/worktree":
			sawWorktree = true
			writeEnvelope(t, w, pkgapi.Success("req_wt", map[string]any{
				"loopId": "loop_dirty", "seq": 3491, "present": true,
				"worktreePath": "/tmp/managed/dirty-wt", "branch": "feat/dirty",
				"managed": true, "clean": false, "dirty": true, "reason": "dirty",
			}))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/loops/3491/retry":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if err := json.Unmarshal(raw, &gotBody); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			writeEnvelope(t, w, pkgapi.Success("req_retry", map[string]any{
				"loop": map[string]any{
					"id": "loop_dirty", "seq": 3491, "projectId": "project_1",
					"type": "fixer", "targetType": "pull_request", "status": "queued",
				},
				"queueItemId": "queue_new", "mode": "auto", "resetAttempts": true,
				"discardWorktreeChanges": true,
				"worktreeDiscard": map[string]any{
					"worktreePath": "/tmp/managed/dirty-wt",
					"discarded":    true, "noOp": false, "reason": "discarded",
				},
			}))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdin:      strings.NewReader("y\n"),
		Stdout:     stdout,
		Stderr:     stderr,
		HTTPClient: server.Client(),
	})
	args := []string{"--config", configPath, "retry", "3491"}
	if exitCode := app.Run(context.Background(), args); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !sawWorktree {
		t.Fatal("expected worktree preflight GET")
	}
	if gotBody["discardWorktreeChanges"] != true {
		t.Fatalf("retry body = %#v, want discardWorktreeChanges=true", gotBody)
	}
	if !strings.Contains(stderr.String(), "Worktree is dirty") {
		t.Fatalf("stderr missing dirty prompt context: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Loop retry queued") {
		t.Fatalf("stdout = %q, want retry queued", stdout.String())
	}
}

func TestLoopRetryDirtyWorktreePromptNoGuidesJump(t *testing.T) {
	t.Parallel()

	retryCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3491/worktree":
			writeEnvelope(t, w, pkgapi.Success("req_wt", map[string]any{
				"loopId": "loop_dirty", "seq": 3491, "present": true,
				"worktreePath": "/tmp/managed/dirty-wt", "branch": "feat/dirty",
				"managed": true, "clean": false, "dirty": true, "reason": "dirty",
			}))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/retry"):
			retryCalled = true
			t.Fatalf("retry must not be called when operator declines discard")
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdin:      strings.NewReader("n\n"),
		Stdout:     stdout,
		Stderr:     stderr,
		HTTPClient: server.Client(),
	})
	args := []string{"--config", configPath, "retry", "3491"}
	if exitCode := app.Run(context.Background(), args); exitCode == 0 {
		t.Fatalf("Run() exit code = 0, want failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if retryCalled {
		t.Fatal("retry POST was called")
	}
	errOut := stderr.String()
	for _, needle := range []string{
		"retry cancelled: worktree is dirty",
		"looper jump '3491'",
		"/tmp/managed/dirty-wt",
		"--discard-worktree-changes --confirm",
	} {
		if !strings.Contains(errOut, needle) {
			t.Fatalf("stderr missing %q\n%s", needle, errOut)
		}
	}
}

func TestLoopRetryDirtyWorktreeJSONRefusesWithoutDiscardFlag(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3491/worktree" {
			writeEnvelope(t, w, pkgapi.Success("req_wt", map[string]any{
				"loopId": "loop_dirty", "seq": 3491, "present": true,
				"worktreePath": "/tmp/managed/dirty-wt", "branch": "feat/dirty",
				"managed": true, "clean": false, "dirty": true, "reason": "dirty",
			}))
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})
	args := []string{"--config", configPath, "--json", "retry", "3491"}
	if exitCode := app.Run(context.Background(), args); exitCode == 0 {
		t.Fatalf("Run() exit code = 0, want failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "retry cancelled: worktree is dirty") {
		t.Fatalf("stderr = %q, want json dirty refusal", stderr.String())
	}
	if !strings.Contains(stderr.String(), "looper jump '3491'") {
		t.Fatalf("stderr missing jump guidance: %q", stderr.String())
	}
}

func TestLoopRetryCleanWorktreeSkipsPrompt(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3108/worktree":
			writeEnvelope(t, w, pkgapi.Success("req_wt", map[string]any{
				"loopId": "loop_clean", "seq": 3108, "present": true,
				"worktreePath": "/tmp/managed/clean-wt", "branch": "feat/clean",
				"managed": true, "clean": true, "dirty": false, "reason": "already_clean",
			}))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/loops/3108/retry":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			writeEnvelope(t, w, pkgapi.Success("req_retry", map[string]any{
				"loop": map[string]any{
					"id": "loop_clean", "seq": 3108, "projectId": "project_1",
					"type": "fixer", "targetType": "pull_request", "status": "queued",
				},
				"queueItemId": "queue_new", "mode": "auto", "resetAttempts": true,
				"discardWorktreeChanges": false,
			}))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})
	args := []string{"--config", configPath, "retry", "3108"}
	if exitCode := app.Run(context.Background(), args); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if _, ok := gotBody["discardWorktreeChanges"]; ok {
		t.Fatalf("retry body = %#v, want no discardWorktreeChanges", gotBody)
	}
}

func TestJumpFallsBackToLoopWorktreeWhenNoActiveRun(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/active/3491":
			writeEnvelope(t, w, pkgapi.Failure("req_active", pkgapi.ErrorCodeActiveRunNotFound, "active run not found", nil))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3491/worktree":
			writeEnvelope(t, w, pkgapi.Success("req_wt", map[string]any{
				"loopId": "loop_dirty", "seq": 3491, "present": true,
				"worktreePath": "/tmp/managed/dirty-wt", "branch": "feat/dirty",
				"managed": true, "clean": false, "dirty": true, "reason": "dirty",
			}))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})
	args := []string{"--config", configPath, "jump", "3491"}
	if exitCode := app.Run(context.Background(), args); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "cd -- '/tmp/managed/dirty-wt'" {
		t.Fatalf("stdout = %q, want cd to worktree", got)
	}
}

func TestJumpRefusesMissingWorktreePath(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/active/3491":
			writeEnvelope(t, w, pkgapi.Failure("req_active", pkgapi.ErrorCodeActiveRunNotFound, "active run not found", nil))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3491/worktree":
			writeEnvelope(t, w, pkgapi.Success("req_wt", map[string]any{
				"loopId": "loop_missing", "seq": 3491, "present": false,
				"worktreePath": "/tmp/managed/gone", "branch": "feat/gone",
				"managed": true, "reason": "worktree_missing",
			}))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})
	if exitCode := app.Run(context.Background(), []string{"--config", configPath, "jump", "3491"}); exitCode == 0 {
		t.Fatalf("Run() exit code = 0, want failure; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "missing on disk") {
		t.Fatalf("stderr = %q, want missing-on-disk error", stderr.String())
	}
}

func TestJumpDoesNotFallbackOnActiveRunServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/runs/active/3491" {
			writeEnvelope(t, w, pkgapi.Failure("req_active", pkgapi.ErrorCodeInternalError, "database down", nil))
			return
		}
		t.Fatalf("unexpected fallback request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})
	if exitCode := app.Run(context.Background(), []string{"--config", configPath, "jump", "3491"}); exitCode == 0 {
		t.Fatalf("Run() exit code = 0, want failure")
	}
	if !strings.Contains(stderr.String(), "database down") {
		t.Fatalf("stderr = %q, want original active-run error", stderr.String())
	}
}

func TestLoopRetryUnmanagedDirtyRefusesDiscardOffer(t *testing.T) {
	t.Parallel()

	retryCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3491/worktree":
			writeEnvelope(t, w, pkgapi.Success("req_wt", map[string]any{
				"loopId": "loop_unmanaged", "seq": 3491, "present": true,
				"worktreePath": "/tmp/primary-repo", "branch": "main",
				"managed": false, "clean": false, "dirty": true, "reason": "unmanaged",
			}))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/retry"):
			retryCalled = true
			t.Fatalf("retry must not run for unmanaged dirty worktree")
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdin:      strings.NewReader("y\n"),
		Stdout:     stdout,
		Stderr:     stderr,
		HTTPClient: server.Client(),
	})
	if exitCode := app.Run(context.Background(), []string{"--config", configPath, "retry", "3491"}); exitCode == 0 {
		t.Fatalf("Run() exit code = 0, want failure; stderr=%q", stderr.String())
	}
	if retryCalled {
		t.Fatal("retry POST was called")
	}
	if !strings.Contains(stderr.String(), "not Looper-managed") {
		t.Fatalf("stderr = %q, want unmanaged guidance", stderr.String())
	}
	if strings.Contains(stderr.String(), "--discard-worktree-changes") {
		t.Fatalf("stderr should not offer discard for unmanaged path: %q", stderr.String())
	}
}

func TestLoopRetryOldDaemonWithoutWorktreeRouteStillRetries(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3108/worktree":
			writeEnvelope(t, w, pkgapi.Failure("req_wt", pkgapi.ErrorCodeRouteNotFound, "Unknown route", nil))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/loops/3108/retry":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			writeEnvelope(t, w, pkgapi.Success("req_retry", map[string]any{
				"loop": map[string]any{
					"id": "loop_old", "seq": 3108, "projectId": "project_1",
					"type": "fixer", "targetType": "pull_request", "status": "queued",
				},
				"queueItemId": "queue_new", "mode": "auto", "resetAttempts": true,
				"discardWorktreeChanges": false,
			}))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})
	if exitCode := app.Run(context.Background(), []string{"--config", configPath, "retry", "3108"}); exitCode != 0 {
		t.Fatalf("Run() exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if _, ok := gotBody["discardWorktreeChanges"]; ok {
		t.Fatalf("body = %#v, want plain retry without discard", gotBody)
	}
}

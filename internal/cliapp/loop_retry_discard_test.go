package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// retryPreflightCase drives CLI retry against a fake /worktree (+ optional /retry) daemon.
type retryPreflightCase struct {
	name           string
	selector       string
	stdin          string
	jsonMode       bool
	worktree       map[string]any
	worktreeStatus int // 0 = success envelope; else ErrorCode via Failure
	expectExitOK   bool
	expectDiscard  *bool // nil = field must be absent
	errContains    []string
	errNotContains []string
	stdoutContains []string
}

func TestLoopRetryWorktreePreflight(t *testing.T) {
	t.Parallel()

	trueVal, falseVal := true, false
	dirtyManaged := map[string]any{
		"loopId": "loop_dirty", "seq": 3491, "present": true,
		"worktreePath": "/tmp/managed/dirty-wt", "branch": "feat/dirty",
		"managed": true, "clean": false, "dirty": true, "reason": "dirty",
	}
	cases := []retryPreflightCase{
		{
			name: "dirty_prompt_yes_discards", selector: "3491", stdin: "y\n",
			worktree: dirtyManaged, expectExitOK: true, expectDiscard: &trueVal,
			errContains:    []string{"Worktree is dirty"},
			stdoutContains: []string{"Loop retry queued"},
		},
		{
			name: "dirty_prompt_no_guides_jump", selector: "3491", stdin: "n\n",
			worktree: dirtyManaged, expectExitOK: false,
			errContains: []string{
				"retry cancelled: worktree is dirty",
				"looper jump '3491'",
				"/tmp/managed/dirty-wt",
				"--discard-worktree-changes --confirm",
			},
		},
		{
			name: "dirty_json_refuses_without_discard", selector: "3491", jsonMode: true,
			worktree: dirtyManaged, expectExitOK: false,
			errContains: []string{"retry cancelled: worktree is dirty", "looper jump '3491'"},
		},
		{
			name: "clean_skips_prompt", selector: "3108",
			worktree: map[string]any{
				"loopId": "loop_clean", "seq": 3108, "present": true,
				"worktreePath": "/tmp/managed/clean-wt", "branch": "feat/clean",
				"managed": true, "clean": true, "dirty": false, "reason": "already_clean",
			},
			expectExitOK: true, expectDiscard: &falseVal, // falseVal marker: field absent
		},
		{
			name: "unmanaged_dirty_refuses_discard_offer", selector: "3491", stdin: "y\n",
			worktree: map[string]any{
				"loopId": "loop_unmanaged", "seq": 3491, "present": true,
				"worktreePath": "/tmp/primary-repo", "branch": "main",
				"managed": false, "clean": false, "dirty": true, "reason": "unmanaged",
			},
			expectExitOK:   false,
			errContains:    []string{"not Looper-managed"},
			errNotContains: []string{"--discard-worktree-changes"},
		},
		{
			name: "old_daemon_without_worktree_route_still_retries", selector: "3108",
			worktreeStatus: http.StatusNotFound, expectExitOK: true, expectDiscard: &falseVal,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotBody map[string]any
			retryCalled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wtPath := "/api/v1/loops/" + tc.selector + "/worktree"
				retryPath := "/api/v1/loops/" + tc.selector + "/retry"
				switch {
				case r.Method == http.MethodGet && r.URL.Path == wtPath:
					if tc.worktreeStatus == http.StatusNotFound {
						writeEnvelope(t, w, pkgapi.Failure("req_wt", pkgapi.ErrorCodeRouteNotFound, "Unknown route", nil))
						return
					}
					writeEnvelope(t, w, pkgapi.Success("req_wt", tc.worktree))
				case r.Method == http.MethodPost && r.URL.Path == retryPath:
					retryCalled = true
					if !tc.expectExitOK {
						t.Fatalf("retry must not be called for case %s", tc.name)
					}
					raw, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(raw, &gotBody)
					writeEnvelope(t, w, pkgapi.Success("req_retry", map[string]any{
						"loop": map[string]any{
							"id": "loop_" + tc.selector, "seq": 3491, "projectId": "project_1",
							"type": "fixer", "targetType": "pull_request", "status": "queued",
						},
						"queueItemId": "queue_new", "mode": "auto", "resetAttempts": true,
						"discardWorktreeChanges": gotBody["discardWorktreeChanges"] == true,
						"worktreeDiscard": map[string]any{
							"worktreePath": "/tmp/managed/dirty-wt",
							"discarded":    gotBody["discardWorktreeChanges"] == true,
							"noOp":         false, "reason": "discarded",
						},
					}))
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()

			configPath := writeCLIConfig(t, server.URL, "")
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			deps := Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()}
			if tc.stdin != "" {
				deps.Stdin = strings.NewReader(tc.stdin)
			}
			args := []string{"--config", configPath}
			if tc.jsonMode {
				args = append(args, "--json")
			}
			args = append(args, "retry", tc.selector)
			exitCode := New(deps).Run(context.Background(), args)
			if tc.expectExitOK && exitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", exitCode, stderr.String())
			}
			if !tc.expectExitOK && exitCode == 0 {
				t.Fatalf("exit=0 want failure stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if tc.expectDiscard != nil {
				if *tc.expectDiscard {
					if gotBody["discardWorktreeChanges"] != true {
						t.Fatalf("body=%#v want discardWorktreeChanges=true", gotBody)
					}
				} else if _, ok := gotBody["discardWorktreeChanges"]; ok {
					t.Fatalf("body=%#v want no discardWorktreeChanges", gotBody)
				}
			} else if retryCalled {
				t.Fatal("retry POST was called unexpectedly")
			}
			errOut := stderr.String()
			for _, needle := range tc.errContains {
				if !strings.Contains(errOut, needle) {
					t.Fatalf("stderr missing %q\n%s", needle, errOut)
				}
			}
			for _, needle := range tc.errNotContains {
				if strings.Contains(errOut, needle) {
					t.Fatalf("stderr must not contain %q\n%s", needle, errOut)
				}
			}
			for _, needle := range tc.stdoutContains {
				if !strings.Contains(stdout.String(), needle) {
					t.Fatalf("stdout missing %q\n%s", needle, stdout.String())
				}
			}
		})
	}
}

func TestJumpWorktreeResolution(t *testing.T) {
	t.Parallel()

	existing := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")

	type jumpCase struct {
		name         string
		active       func(w http.ResponseWriter, r *http.Request)
		worktree     map[string]any
		allowWT      bool
		expectExitOK bool
		stdoutExact  string
		errContains  []string
	}
	cases := []jumpCase{
		{
			name: "fallback_to_loop_worktree_when_no_active_run",
			active: func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(t, w, pkgapi.Failure("req_active", pkgapi.ErrorCodeActiveRunNotFound, "active run not found", nil))
			},
			worktree: map[string]any{
				"loopId": "loop_dirty", "seq": 3491, "present": true,
				"worktreePath": existing, "branch": "feat/dirty",
				"managed": true, "clean": false, "dirty": true, "reason": "dirty",
			},
			allowWT: true, expectExitOK: true,
			stdoutExact: "cd -- " + quoteShellArg(existing),
		},
		{
			name: "refuses_missing_loop_worktree_path",
			active: func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(t, w, pkgapi.Failure("req_active", pkgapi.ErrorCodeActiveRunNotFound, "active run not found", nil))
			},
			worktree: map[string]any{
				"loopId": "loop_missing", "seq": 3491, "present": false,
				"worktreePath": missing, "branch": "feat/gone",
				"managed": true, "reason": "worktree_missing",
			},
			allowWT: true, expectExitOK: false, errContains: []string{"missing on disk"},
		},
		{
			name: "refuses_missing_active_run_worktree_path",
			active: func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(t, w, pkgapi.Success("req_active", map[string]any{
					"seq": 3491, "loopId": "loop_active", "projectId": "project_1",
					"worktree": map[string]any{"path": missing, "branch": "feat/gone"},
				}))
			},
			expectExitOK: false, errContains: []string{"missing on disk", missing},
		},
		{
			name: "active_run_existing_path_jumps",
			active: func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(t, w, pkgapi.Success("req_active", map[string]any{
					"seq": 3491, "loopId": "loop_active", "projectId": "project_1",
					"worktree": map[string]any{"path": existing, "branch": "feat/ok"},
				}))
			},
			expectExitOK: true, stdoutExact: "cd -- " + quoteShellArg(existing),
		},
		{
			name: "does_not_fallback_on_active_run_server_error",
			active: func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(t, w, pkgapi.Failure("req_active", pkgapi.ErrorCodeInternalError, "database down", nil))
			},
			expectExitOK: false, errContains: []string{"database down"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/active/3491":
					tc.active(w, r)
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/loops/3491/worktree":
					if !tc.allowWT {
						t.Fatalf("unexpected fallback request %s %s", r.Method, r.URL.Path)
					}
					writeEnvelope(t, w, pkgapi.Success("req_wt", tc.worktree))
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()

			configPath := writeCLIConfig(t, server.URL, "")
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			exitCode := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()}).
				Run(context.Background(), []string{"--config", configPath, "jump", "3491"})
			if tc.expectExitOK && exitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", exitCode, stderr.String())
			}
			if !tc.expectExitOK && exitCode == 0 {
				t.Fatalf("exit=0 want failure stdout=%q", stdout.String())
			}
			if tc.stdoutExact != "" {
				if got := strings.TrimSpace(stdout.String()); got != tc.stdoutExact {
					t.Fatalf("stdout=%q want %q", got, tc.stdoutExact)
				}
			}
			for _, needle := range tc.errContains {
				if !strings.Contains(stderr.String(), needle) {
					t.Fatalf("stderr missing %q\n%s", needle, stderr.String())
				}
			}
		})
	}
}

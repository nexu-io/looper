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

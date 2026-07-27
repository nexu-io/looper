package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/spf13/cobra"
)

func TestCommandRuntimeTrustedReviewConfigIgnoresChildPrecedenceLayers(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	capturedGHPath := filepath.Join(t.TempDir(), "daemon-cli-gh")
	cfg.Tools.GHPath = &capturedGHPath
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(config snapshot) error = %v", err)
	}

	// In a real proxy child the conflicting LOOPER_GH_PATH may originate in
	// daemon ambient env or captured Agent.Env; neither may re-apply over the
	// daemon's already materialized CLI winner. Provider credentials remain
	// ordinary child env because the selected transport still needs them.
	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	t.Setenv("LOOPER_GH_PATH", filepath.Join(t.TempDir(), "agent-env-gh"))
	t.Setenv("FORGEJO_TOKEN", "provider-credential")
	installTrustedReviewConfigFD(t, raw)
	runtime := &commandRuntime{argv: []string{"review", "submit", "acme/looper#42", "--gh-path", filepath.Join(t.TempDir(), "child-cli-gh")}}
	loaded, err := runtime.loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if loaded.Config.Tools.GHPath == nil || *loaded.Config.Tools.GHPath != capturedGHPath {
		t.Fatalf("loadConfig().Tools.GHPath = %v, want captured daemon CLI winner %q", loaded.Config.Tools.GHPath, capturedGHPath)
	}
	if got := os.Getenv("FORGEJO_TOKEN"); got != "provider-credential" {
		t.Fatalf("FORGEJO_TOKEN after loadConfig() = %q, want provider credential preserved", got)
	}
}

func TestCommandRuntimeTrustedReviewConfigMemoizesOneShotFD(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	capturedGHPath := filepath.Join(t.TempDir(), "daemon-cli-gh")
	cfg.Tools.GHPath = &capturedGHPath
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(config snapshot) error = %v", err)
	}

	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	installTrustedReviewConfigFD(t, raw)
	runtime := &commandRuntime{argv: []string{"review", "submit", "acme/looper#42"}}

	first, err := runtime.loadConfig()
	if err != nil {
		t.Fatalf("first loadConfig() error = %v", err)
	}
	if os.Getenv(forge.TrustedReviewConfigFDEnv) != "" {
		t.Fatalf("TrustedReviewConfigFDEnv still set after first load; want cleared")
	}
	// Descriptor is closed; a second uncached LoadTrustedReviewConfigSnapshot
	// would EBADF. Memoized loadConfig must return the same snapshot.
	second, err := runtime.loadConfig()
	if err != nil {
		t.Fatalf("second loadConfig() error = %v (want memoized success, not EBADF)", err)
	}
	if first.Config.Tools.GHPath == nil || second.Config.Tools.GHPath == nil ||
		*first.Config.Tools.GHPath != *second.Config.Tools.GHPath ||
		*second.Config.Tools.GHPath != capturedGHPath {
		t.Fatalf("memoized loadConfig mismatch: first=%v second=%v want %q", first.Config.Tools.GHPath, second.Config.Tools.GHPath, capturedGHPath)
	}
}

func TestCommandRuntimeTrustedReviewConfigMemoizesOriginalError(t *testing.T) {
	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	// Empty pipe (writer closed immediately) → decode/EOF, not EBADF.
	installTrustedReviewConfigFD(t, nil)
	runtime := &commandRuntime{argv: []string{"review", "submit", "acme/looper#42"}}

	_, firstErr := runtime.loadConfig()
	if firstErr == nil {
		t.Fatal("first loadConfig() error = nil, want decode/EOF failure")
	}

	_, secondErr := runtime.loadConfig()
	if secondErr == nil {
		t.Fatal("second loadConfig() error = nil, want memoized original error")
	}
	if firstErr.Error() != secondErr.Error() {
		t.Fatalf("memoized error mismatch:\n first=%v\nsecond=%v", firstErr, secondErr)
	}
	if strings.Contains(strings.ToLower(secondErr.Error()), "bad file descriptor") {
		t.Fatalf("second loadConfig() = %v, want original snapshot error not EBADF", secondErr)
	}
}

func TestMaybeRunAutoUpgradeSkipsTrustedReviewProxyChild(t *testing.T) {
	// Leave a closed FD selector so any accidental loadConfig would EBADF.
	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	t.Setenv(forge.TrustedReviewConfigFDEnv, "3")
	runtime := &commandRuntime{argv: []string{"review", "submit", "acme/looper#42"}}
	cmd := &cobra.Command{Use: "submit"}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	if err := runtime.maybeRunAutoUpgrade(cmd, nil); err != nil {
		t.Fatalf("maybeRunAutoUpgrade() error = %v", err)
	}
	if runtime.trustedReviewConfigLoaded {
		t.Fatal("maybeRunAutoUpgrade loaded trusted config; want complete skip")
	}
	if os.Getenv(forge.TrustedReviewConfigFDEnv) != "3" {
		t.Fatalf("TrustedReviewConfigFDEnv = %q, want untouched when auto-upgrade skips", os.Getenv(forge.TrustedReviewConfigFDEnv))
	}
}

func TestAppRunTrustedReviewChildDoesNotEBADFAfterAutoUpgradePreRun(t *testing.T) {
	// Regression for release-installed review-submit children: root
	// PersistentPreRunE (auto-upgrade) used to loadConfig() and close FD 3
	// before review submit could read the daemon snapshot.
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	// Distinctive snapshot value proves daemon authority reached review submit
	// after PersistentPreRun (not a live-file fallback).
	sentinelGH := filepath.Join(t.TempDir(), "snapshot-authority-gh")
	cfg.Tools.GHPath = &sentinelGH
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(config snapshot) error = %v", err)
	}

	// Install-shaped path + stable channel so release classification would
	// apply if auto-upgrade were not skipped for trusted children.
	fakeHome := t.TempDir()
	binDir := filepath.Join(fakeHome, ".looper", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	fakeExec := filepath.Join(binDir, "looper")
	if err := os.WriteFile(fakeExec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(fake exec) error = %v", err)
	}

	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	// Ambient override must not win over the snapshot.
	t.Setenv("LOOPER_GH_PATH", filepath.Join(t.TempDir(), "must-not-win"))
	installTrustedReviewConfigFD(t, raw)

	var stdout, stderr bytes.Buffer
	app := New(Deps{
		Stdout:         &stdout,
		Stderr:         &stderr,
		Stdin:          bytes.NewBufferString(`{"body":"diagnostic","comments":[]}`),
		HomeDir:        fakeHome,
		ExecutablePath: fakeExec,
		CLIChannel:     "stable",
	})
	code := app.Run(context.Background(), []string{
		"review", "submit", "acme/looper#42",
		"--event", "COMMENT",
		"--commit-id", "0000000000000000000000000000000000000000",
	})
	if code == 0 {
		t.Fatal("app.Run() exit = 0, want non-zero after config load (no real gh/PR)")
	}
	combined := strings.ToLower(stdout.String() + "\n" + stderr.String())
	if strings.Contains(combined, "bad file descriptor") {
		t.Fatalf("app.Run output contains EBADF:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	// Snapshot must have been readable; failure should be downstream of config
	// (gateway/PR/auth), not trusted-review-config FD transport.
	if strings.Contains(combined, "trusted review config") && strings.Contains(combined, "descriptor") {
		t.Fatalf("app.Run hit trusted config descriptor failure:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
}

// installTrustedReviewConfigFD writes snapshot bytes into a pipe and exposes a
// duplicated read FD via TrustedReviewConfigFDEnv. The loader owns and closes
// that duplicate exclusively; the original pipe ends are closed by cleanup so
// finalizers cannot double-close a reused descriptor number.
func installTrustedReviewConfigFD(t *testing.T, snapshot []byte) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	writeDone := make(chan error, 1)
	go func() {
		var writeErr error
		if len(snapshot) > 0 {
			_, writeErr = writer.Write(snapshot)
		}
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		writeDone <- writeErr
	}()

	dupFD, err := syscall.Dup(int(reader.Fd()))
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatalf("Dup(config reader) error = %v", err)
	}
	if err := reader.Close(); err != nil {
		_ = syscall.Close(dupFD)
		t.Fatalf("Close(original reader) error = %v", err)
	}
	t.Setenv(forge.TrustedReviewConfigFDEnv, strconv.Itoa(dupFD))
	// Loader owns dupFD exclusively and closes it. Do not close here: after the
	// loader closes, the number may be reused and a second close would be unsafe.
	t.Cleanup(func() {
		if err := <-writeDone; err != nil {
			t.Errorf("write config snapshot error = %v", err)
		}
	})
}

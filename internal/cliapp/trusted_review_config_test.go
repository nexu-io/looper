package cliapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
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

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := writer.Write(raw)
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		writeDone <- writeErr
	}()

	// In a real proxy child the conflicting LOOPER_GH_PATH may originate in
	// daemon ambient env or captured Agent.Env; neither may re-apply over the
	// daemon's already materialized CLI winner. Provider credentials remain
	// ordinary child env because the selected transport still needs them.
	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	t.Setenv(forge.TrustedReviewConfigFDEnv, strconv.FormatUint(uint64(reader.Fd()), 10))
	t.Setenv("LOOPER_GH_PATH", filepath.Join(t.TempDir(), "agent-env-gh"))
	t.Setenv("FORGEJO_TOKEN", "provider-credential")
	runtime := &commandRuntime{argv: []string{"review", "submit", "acme/looper#42", "--gh-path", filepath.Join(t.TempDir(), "child-cli-gh")}}
	loaded, err := runtime.loadConfig()
	// LoadTrustedReviewConfigSnapshot owns and closes the inherited descriptor.
	// Mark this test's original os.File wrapper closed immediately so its later
	// finalizer cannot close an unrelated descriptor that the OS reused.
	_ = reader.Close()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if loaded.Config.Tools.GHPath == nil || *loaded.Config.Tools.GHPath != capturedGHPath {
		t.Fatalf("loadConfig().Tools.GHPath = %v, want captured daemon CLI winner %q", loaded.Config.Tools.GHPath, capturedGHPath)
	}
	if got := os.Getenv("FORGEJO_TOKEN"); got != "provider-credential" {
		t.Fatalf("FORGEJO_TOKEN after loadConfig() = %q, want provider credential preserved", got)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write config snapshot error = %v", err)
	}
}

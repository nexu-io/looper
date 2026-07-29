package cliapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/spf13/cobra"
)

// A proxy child loads config more than once per process: the root
// PersistentPreRunE runs before the bound subcommand's RunE. The inherited
// descriptor carries one snapshot and is closed after the first read, so the
// second load must reuse the first result instead of reading a closed pipe.
func TestCommandRuntimeTrustedReviewConfigSurvivesRepeatedLoads(t *testing.T) {
	capturedGHPath := filepath.Join(t.TempDir(), "daemon-cli-gh")
	runtime := newTrustedReviewProxyChildRuntime(t, capturedGHPath)

	first, err := runtime.loadConfig()
	if err != nil {
		t.Fatalf("first loadConfig() error = %v", err)
	}
	second, err := runtime.loadConfig()
	if err != nil {
		t.Fatalf("second loadConfig() error = %v", err)
	}

	for label, loaded := range map[string]config.LoadedFileConfig{"first": first, "second": second} {
		if loaded.Config.Tools.GHPath == nil || *loaded.Config.Tools.GHPath != capturedGHPath {
			t.Fatalf("%s loadConfig().Tools.GHPath = %v, want captured daemon CLI winner %q", label, loaded.Config.Tools.GHPath, capturedGHPath)
		}
	}
}

// The auto-upgrade pre-run hook is the concrete second loader in a proxy
// child. A bound review-submit child must never replace its own binary
// mid-run, so the hook is skipped before it can reach any config load.
func TestShouldSkipAutoUpgradeForTrustedReviewProxyChild(t *testing.T) {
	cmd := &cobra.Command{Use: "submit"}
	parent := &cobra.Command{Use: "review"}
	root := &cobra.Command{Use: "looper"}
	root.AddCommand(parent)
	parent.AddCommand(cmd)

	if shouldSkipAutoUpgrade(cmd) {
		t.Fatal("shouldSkipAutoUpgrade(review submit) = true without proxy-child env, want false")
	}

	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	if !shouldSkipAutoUpgrade(cmd) {
		t.Fatal("shouldSkipAutoUpgrade(review submit) = false in a proxy child, want true")
	}
}

func newTrustedReviewProxyChildRuntime(t *testing.T, capturedGHPath string) *commandRuntime {
	t.Helper()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
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
	t.Cleanup(func() {
		// LoadTrustedReviewConfigSnapshot owns and closes the inherited
		// descriptor. Mark this test's wrapper closed so its finalizer cannot
		// close an unrelated descriptor the OS reused.
		_ = reader.Close()
		if writeErr := <-writeDone; writeErr != nil {
			t.Errorf("write config snapshot error = %v", writeErr)
		}
	})

	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	t.Setenv(forge.TrustedReviewConfigFDEnv, strconv.FormatUint(uint64(reader.Fd()), 10))
	return &commandRuntime{argv: []string{"review", "submit", "acme/looper#42"}}
}

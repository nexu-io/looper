package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestReloadConfigHidesSecretBearingUnstructuredDecodeErrors(t *testing.T) {
	t.Parallel()

	const secret = "SUPER_SECRET_VALUE"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("notifications:\n  osascript:\n    enabled: false\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(initial) error = %v", err)
	}
	load := func() (config.LoadedFileConfig, error) {
		return config.LoadFile(config.LoadFileOptions{
			ConfigPathOverride: path,
			LookupEnv:          func(string) (string, bool) { return "", false },
		})
	}
	initial, err := load()
	if err != nil {
		t.Fatalf("LoadFile(initial) error = %v", err)
	}
	logger := &configReloadDiagnosticLogger{}
	rt := New(Options{Config: initial.Config, InitialConfig: initial, ReloadConfig: load, Logger: logger})

	// yaml.v3 includes the invalid composite key's scalar in this error. That
	// makes the test exercise a real parser disclosure rather than a fake error.
	malformed := "agent:\n  env:\n    ? [" + secret + "]\n    : value\n"
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatalf("WriteFile(malformed) error = %v", err)
	}
	_, rawErr := load()
	if rawErr == nil || !strings.Contains(rawErr.Error(), secret) {
		t.Fatalf("LoadFile(malformed) error = %v, want secret-bearing parser error", rawErr)
	}
	reloadErr := rt.ReloadConfig(context.Background())
	if reloadErr == nil {
		t.Fatal("ReloadConfig() error = nil, want rejected candidate")
	}
	if strings.Contains(reloadErr.Error(), secret) || strings.Contains(reloadErr.Error(), "invalid map key") {
		t.Fatalf("ReloadConfig() error = %q, exposed parser details", reloadErr)
	}

	status := rt.ConfigReloadStatus()
	if strings.Contains(status.LastError, secret) || strings.Contains(status.LastError, "invalid map key") {
		t.Fatalf("ConfigReloadStatus().LastError = %q, exposed parser details", status.LastError)
	}
	if got := logger.joined(); strings.Contains(got, secret) || strings.Contains(got, "invalid map key") {
		t.Fatalf("logger output = %q, exposed parser details", got)
	}
}

type configReloadDiagnosticLogger struct {
	mu      sync.Mutex
	entries []string
}

func (l *configReloadDiagnosticLogger) Debug(message string, context map[string]any) {
	l.append(message, context)
}
func (l *configReloadDiagnosticLogger) Info(message string, context map[string]any) {
	l.append(message, context)
}
func (l *configReloadDiagnosticLogger) Warn(message string, context map[string]any) {
	l.append(message, context)
}
func (l *configReloadDiagnosticLogger) Error(message string, context map[string]any) {
	l.append(message, context)
}

func (l *configReloadDiagnosticLogger) append(message string, context map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, message+" "+fmt.Sprint(context))
}

func (l *configReloadDiagnosticLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.entries, "\n")
}

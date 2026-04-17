package bootstrap

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/config"
)

func TestCreateLoggerWritesStructuredJSONAndRoutesStreams(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	fixedTime := time.Date(2026, time.April, 17, 12, 34, 56, 789000000, time.FixedZone("PDT", -7*60*60))

	logger, err := CreateLogger(config.LoggingConfig{Level: config.LogLevelInfo, MaxSizeMB: 10, MaxFiles: 5}, logDir, LoggerOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Now:    func() time.Time { return fixedTime },
	})
	if err != nil {
		t.Fatalf("CreateLogger() error = %v", err)
	}

	logger.Debug("skipped", map[string]any{"debug": true})
	logger.Info("started", map[string]any{"component": "bootstrap"})
	logger.Warn("careful", nil)
	logger.Error("failed", map[string]any{"attempt": float64(2)})

	rawLog, err := os.ReadFile(LogFilePath(logDir))
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(rawLog)), "\n")
	if len(lines) != 3 {
		t.Fatalf("log lines = %d, want 3", len(lines))
	}

	entries := decodeEntries(t, lines)

	assertEntry(t, entries[0], fixedTime, config.LogLevelInfo, "started", map[string]any{"component": "bootstrap"})
	assertEntry(t, entries[1], fixedTime, config.LogLevelWarn, "careful", nil)
	assertEntry(t, entries[2], fixedTime, config.LogLevelError, "failed", map[string]any{"attempt": float64(2)})

	stdoutLines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(stdoutLines) != 1 {
		t.Fatalf("stdout lines = %d, want 1", len(stdoutLines))
	}
	assertEntry(t, decodeEntries(t, stdoutLines)[0], fixedTime, config.LogLevelInfo, "started", map[string]any{"component": "bootstrap"})

	stderrLines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(stderrLines) != 2 {
		t.Fatalf("stderr lines = %d, want 2", len(stderrLines))
	}
	decodedStderr := decodeEntries(t, stderrLines)
	assertEntry(t, decodedStderr[0], fixedTime, config.LogLevelWarn, "careful", nil)
	assertEntry(t, decodedStderr[1], fixedTime, config.LogLevelError, "failed", map[string]any{"attempt": float64(2)})
}

func TestCreateLoggerCreatesLogDirectory(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "nested", "logs")

	logger, err := CreateLogger(config.LoggingConfig{Level: config.LogLevelDebug, MaxSizeMB: 10, MaxFiles: 5}, logDir, LoggerOptions{})
	if err != nil {
		t.Fatalf("CreateLogger() error = %v", err)
	}

	logger.Info("created", nil)

	if _, err := os.Stat(LogFilePath(logDir)); err != nil {
		t.Fatalf("os.Stat() error = %v", err)
	}
	if _, err := os.Stat(logDir); err != nil {
		t.Fatalf("os.Stat(logDir) error = %v", err)
	}
}

func TestCreateLoggerRotatesBySizeAndRetainsMaxFiles(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	logger, err := CreateLogger(config.LoggingConfig{Level: config.LogLevelInfo, MaxSizeMB: 1, MaxFiles: 2}, logDir, LoggerOptions{})
	if err != nil {
		t.Fatalf("CreateLogger() error = %v", err)
	}

	message1 := strings.Repeat("a", 700_000)
	message2 := strings.Repeat("b", 700_000)
	message3 := strings.Repeat("c", 700_000)

	logger.Info(message1, nil)
	logger.Info(message2, nil)
	logger.Info(message3, nil)

	activeLog, err := os.ReadFile(LogFilePath(logDir))
	if err != nil {
		t.Fatalf("os.ReadFile(active) error = %v", err)
	}
	if !strings.Contains(string(activeLog), message3) {
		t.Fatalf("active log does not contain newest message")
	}
	if strings.Contains(string(activeLog), message2) || strings.Contains(string(activeLog), message1) {
		t.Fatalf("active log retained rotated messages")
	}

	rotatedPath := LogFilePath(logDir) + ".1"
	rotatedLog, err := os.ReadFile(rotatedPath)
	if err != nil {
		t.Fatalf("os.ReadFile(rotated) error = %v", err)
	}
	if !strings.Contains(string(rotatedLog), message2) {
		t.Fatalf("rotated log does not contain previous message")
	}
	if strings.Contains(string(rotatedLog), message1) {
		t.Fatalf("rotated log retained expired message")
	}

	if _, err := os.Stat(LogFilePath(logDir) + ".2"); !os.IsNotExist(err) {
		t.Fatalf("expected no second archive, got err = %v", err)
	}
}

func TestCreateLoggerRotationWithSingleRetainedFileDropsArchives(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	logger, err := CreateLogger(config.LoggingConfig{Level: config.LogLevelInfo, MaxSizeMB: 1, MaxFiles: 1}, logDir, LoggerOptions{})
	if err != nil {
		t.Fatalf("CreateLogger() error = %v", err)
	}

	message1 := strings.Repeat("x", 700_000)
	message2 := strings.Repeat("y", 700_000)

	logger.Info(message1, nil)
	logger.Info(message2, nil)

	activeLog, err := os.ReadFile(LogFilePath(logDir))
	if err != nil {
		t.Fatalf("os.ReadFile(active) error = %v", err)
	}
	if !strings.Contains(string(activeLog), message2) {
		t.Fatalf("active log does not contain newest message")
	}
	if strings.Contains(string(activeLog), message1) {
		t.Fatalf("active log retained expired message")
	}

	if _, err := os.Stat(LogFilePath(logDir) + ".1"); !os.IsNotExist(err) {
		t.Fatalf("expected no archive files, got err = %v", err)
	}
}

func TestCreateLoggerRotatesArchiveChainAndCleansStaleFiles(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	staleArchivePath := LogFilePath(logDir) + ".3"
	if err := os.WriteFile(staleArchivePath, []byte("stale"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(stale archive) error = %v", err)
	}

	logger, err := CreateLogger(config.LoggingConfig{Level: config.LogLevelInfo, MaxSizeMB: 1, MaxFiles: 3}, logDir, LoggerOptions{})
	if err != nil {
		t.Fatalf("CreateLogger() error = %v", err)
	}

	message1 := strings.Repeat("m", 700_000)
	message2 := strings.Repeat("n", 700_000)
	message3 := strings.Repeat("o", 700_000)
	message4 := strings.Repeat("p", 700_000)

	logger.Info(message1, nil)
	logger.Info(message2, nil)
	logger.Info(message3, nil)
	logger.Info(message4, nil)

	assertFileContains(t, LogFilePath(logDir), message4)
	assertFileContains(t, LogFilePath(logDir)+".1", message3)
	assertFileContains(t, LogFilePath(logDir)+".2", message2)

	if _, err := os.Stat(staleArchivePath); !os.IsNotExist(err) {
		t.Fatalf("expected stale archive to be removed, got err = %v", err)
	}
	if _, err := os.Stat(LogFilePath(logDir) + ".4"); !os.IsNotExist(err) {
		t.Fatalf("expected no archive beyond retention, got err = %v", err)
	}
}

func TestFormatLocalTimestampMatchesExpectedShape(t *testing.T) {
	value := time.Date(2026, time.April, 17, 12, 34, 56, 789000000, time.FixedZone("IST", 5*60*60+30*60))

	got := FormatLocalTimestamp(value)
	const want = "2026-04-17T12:34:56.789+05:30"
	if got != want {
		t.Fatalf("FormatLocalTimestamp() = %q, want %q", got, want)
	}
}

func decodeEntries(t *testing.T, lines []string) []map[string]any {
	t.Helper()

	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("json.Unmarshal(%q) error = %v", line, err)
		}
		entries = append(entries, entry)
	}

	return entries
}

func assertEntry(t *testing.T, entry map[string]any, wantTime time.Time, wantLevel config.LogLevel, wantMessage string, wantContext map[string]any) {
	t.Helper()

	if got := entry["ts"]; got != FormatLocalTimestamp(wantTime) {
		t.Fatalf("entry[ts] = %#v, want %q", got, FormatLocalTimestamp(wantTime))
	}

	if got := entry["level"]; got != string(wantLevel) {
		t.Fatalf("entry[level] = %#v, want %q", got, wantLevel)
	}

	if got := entry["message"]; got != wantMessage {
		t.Fatalf("entry[message] = %#v, want %q", got, wantMessage)
	}

	contextValue, hasContext := entry["context"]
	if wantContext == nil {
		if hasContext {
			t.Fatalf("entry[context] = %#v, want omitted", contextValue)
		}
		return
	}

	gotContext, ok := contextValue.(map[string]any)
	if !ok {
		t.Fatalf("entry[context] = %#v, want object", contextValue)
	}

	if len(gotContext) != len(wantContext) {
		t.Fatalf("len(entry[context]) = %d, want %d", len(gotContext), len(wantContext))
	}

	for key, wantValue := range wantContext {
		if gotValue := gotContext[key]; gotValue != wantValue {
			t.Fatalf("entry[context][%q] = %#v, want %#v", key, gotValue, wantValue)
		}
	}
}

func assertFileContains(t *testing.T, path string, want string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	if !strings.Contains(string(content), want) {
		t.Fatalf("%s does not contain expected content", path)
	}
}

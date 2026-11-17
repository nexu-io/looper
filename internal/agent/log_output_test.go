package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOutputUsesPersistedLogsAndFallsBackToCapturedOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout.log")
	if err := os.WriteFile(path, []byte("live output"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private.log")
	if err := os.WriteFile(outside, []byte("unrelated private data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, want string }{
		{path, "live output"},
		{filepath.Join(dir, "missing.log"), "captured output"},
		{outside, "captured output"},
	} {
		raw, _ := json.Marshal(map[string]string{"stdout": "captured output", "stdoutLogPath": tc.path, "stderr": "diagnostic"})
		payload := string(raw)
		stdout, stderr := ReadOutput(dir, &payload)
		if stdout != tc.want || stderr != "diagnostic" {
			t.Fatalf("ReadOutput(%q) = %q, %q", tc.path, stdout, stderr)
		}
	}
	if stdout, stderr := ReadOutput(dir, nil); stdout != "" || stderr != "" {
		t.Fatalf("missing output: %q, %q", stdout, stderr)
	}
}

func TestReadAgentOutputLogReadsTailToPreserveCompletionMarker(t *testing.T) {
	t.Parallel()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "stdout.log")
	completionLine := "__LOOPER_RESULT__={\"summary\":\"done from tail\"}\n"
	headSize := maxPersistedAgentLogReadBytes - len(completionLine) + 1023
	fullLog := strings.Repeat("x", headSize) + "\n" + completionLine
	if err := os.WriteFile(logPath, []byte(fullLog), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	persisted, ok := readAgentOutputLog(logDir, logPath)
	if !ok {
		t.Fatal("readAgentOutputLog() = ok false, want true")
	}
	if len(persisted) != maxPersistedAgentLogReadBytes {
		t.Fatalf("len(persisted) = %d, want %d", len(persisted), maxPersistedAgentLogReadBytes)
	}
	if !strings.HasSuffix(persisted, completionLine) {
		t.Fatal("persisted log missing completion tail suffix")
	}
}

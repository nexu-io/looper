package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	completionMarkerEnv     = "LOOPER_COMPLETION_MARKER"
	envFakeAgentMode        = "LOOPER_E2E_FAKE_AGENT_MODE"
	envFakeAgentArtifactDir = "LOOPER_E2E_FAKE_AGENT_ARTIFACT_DIR"
	envFakeAgentStatePath   = "LOOPER_E2E_FAKE_AGENT_STATE_PATH"
	envFakeAgentWriteFile   = "LOOPER_E2E_FAKE_AGENT_WRITE_FILE"
	envFakeAgentModifyFile  = "LOOPER_E2E_FAKE_AGENT_MODIFY_FILE"
	envFakeAgentSleepMS     = "LOOPER_E2E_FAKE_AGENT_SLEEP_MS"
	envFakeAgentGitPath     = "LOOPER_E2E_FAKE_AGENT_GIT_PATH"
	defaultCompletionMarker = "__LOOPER_RESULT__="
)

type evidence struct {
	CWD       string            `json:"cwd"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Timestamp string            `json:"timestamp"`
	Mode      string            `json:"mode"`
	PID       int               `json:"pid"`
}

func main() {
	mode := strings.TrimSpace(os.Getenv(envFakeAgentMode))
	if mode == "" {
		mode = "success-no-diff"
	}
	artifactDir := strings.TrimSpace(os.Getenv(envFakeAgentArtifactDir))
	if artifactDir == "" {
		artifactDir = "."
	}
	_ = os.MkdirAll(artifactDir, 0o755)
	_ = writeEvidence(artifactDir, mode)
	if sleepMS, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(envFakeAgentSleepMS))); sleepMS > 0 {
		time.Sleep(time.Duration(sleepMS) * time.Millisecond)
	}
	marker := strings.TrimSpace(os.Getenv(completionMarkerEnv))
	if marker == "" {
		marker = defaultCompletionMarker
	}
	switch mode {
	case "success-with-diff", "write-file":
		path := envOr(envFakeAgentWriteFile, "agent-output.txt")
		mustWriteFile(path, []byte("changed by fake agent\n"))
		printCompletion(marker, map[string]any{"summary": "fake agent wrote file", "changedFiles": []string{path}})
	case "success-no-diff":
		printCompletion(marker, map[string]any{"summary": "fake agent no diff"})
	case "modify-file":
		path := envOr(envFakeAgentModifyFile, "README.md")
		mustAppendFile(path, []byte("modified by fake agent\n"))
		printCompletion(marker, map[string]any{"summary": "fake agent modified file", "changedFiles": []string{path}})
	case "commit":
		path := envOr(envFakeAgentWriteFile, "agent-commit.txt")
		mustWriteFile(path, []byte("commit from fake agent\n"))
		gitPath := envOr(envFakeAgentGitPath, "git")
		mustRun(gitPath, "add", path)
		mustRun(gitPath, "commit", "-m", "fake agent commit")
		sha := strings.TrimSpace(mustOutput(gitPath, "rev-parse", "HEAD"))
		printCompletion(marker, map[string]any{"summary": "fake agent committed changes", "changedFiles": []string{path}, "commits": []string{sha}})
	case "commit-with-review-replies":
		path := envOr(envFakeAgentWriteFile, "fix-target.txt")
		mustWriteFile(path, []byte("fixed by fake agent\n"))
		gitPath := envOr(envFakeAgentGitPath, "git")
		mustRun(gitPath, "add", path)
		mustRun(gitPath, "commit", "-m", "fake agent commit")
		sha := strings.TrimSpace(mustOutput(gitPath, "rev-parse", "HEAD"))
		printCompletion(marker, map[string]any{
			"summary":      "fake agent committed changes",
			"changedFiles": []string{path},
			"commits":      []string{sha},
			"review_thread_replies": []map[string]any{{
				"fixItemId":   "comment-1",
				"threadId":    "thread-1",
				"explanation": "Updated fix-target.txt to address the review feedback.",
			}},
		})
	case "transient-failure":
		statePath := strings.TrimSpace(os.Getenv(envFakeAgentStatePath))
		if firstRun(statePath) {
			_, _ = fmt.Fprintln(os.Stderr, "transient fake-agent failure")
			os.Exit(1)
		}
		printCompletion(marker, map[string]any{"summary": "fake agent recovered"})
	case "malformed-marker":
		_, _ = fmt.Printf("%s{bad json}\n", marker)
	case "timeout", "no-marker":
		_, _ = fmt.Fprintln(os.Stdout, "fake agent finished without completion marker")
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unsupported fake-agent mode %q\n", mode)
		os.Exit(2)
	}
}

func writeEvidence(dir string, mode string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	data := evidence{CWD: cwd, Args: os.Args[1:], Env: collectEnv(), Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Mode: mode, PID: os.Getpid()}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cwd-evidence.json"), payload, 0o644)
}

func collectEnv() map[string]string {
	keys := []string{"HOME", completionMarkerEnv, envFakeAgentMode, envFakeAgentArtifactDir, envFakeAgentStatePath, envFakeAgentWriteFile, envFakeAgentModifyFile, envFakeAgentGitPath}
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			result[key] = value
		}
	}
	return result
}

func envOr(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func mustWriteFile(path string, content []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		panic(err)
	}
}

func mustAppendFile(path string, content []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	if _, err := file.Write(content); err != nil {
		panic(err)
	}
}

func mustRun(command string, args ...string) {
	cmd := exec.Command(command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic(err)
	}
}

func mustOutput(command string, args ...string) string {
	cmd := exec.Command(command, args...)
	output, err := cmd.Output()
	if err != nil {
		panic(err)
	}
	return string(output)
}

func firstRun(statePath string) bool {
	if strings.TrimSpace(statePath) == "" {
		return true
	}
	if _, err := os.Stat(statePath); err == nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(statePath, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
		panic(err)
	}
	return true
}

func printCompletion(marker string, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	_, _ = fmt.Printf("%s%s\n", marker, string(encoded))
}

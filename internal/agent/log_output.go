package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxPersistedAgentLogReadBytes = 16 * 1024 * 1024

// ReadOutput returns captured agent output, preferring bounded persisted logs.
func ReadOutput(logDir string, outputJSON *string) (string, string) {
	if outputJSON == nil || strings.TrimSpace(*outputJSON) == "" {
		return "", ""
	}
	var payload struct {
		Stdout        string `json:"stdout"`
		Stderr        string `json:"stderr"`
		StdoutLogPath string `json:"stdoutLogPath"`
		StderrLogPath string `json:"stderrLogPath"`
	}
	if err := json.Unmarshal([]byte(*outputJSON), &payload); err != nil {
		return "", ""
	}
	if content, ok := readAgentOutputLog(logDir, payload.StdoutLogPath); ok {
		payload.Stdout = content
	}
	if content, ok := readAgentOutputLog(logDir, payload.StderrLogPath); ok {
		payload.Stderr = content
	}
	return payload.Stdout, payload.Stderr
}

func readAgentOutputLog(logDir string, path string) (string, bool) {
	if strings.TrimSpace(path) == "" {
		return "", false
	}
	if !isPathWithinDirectory(path, logDir) {
		return "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", false
	}
	if info.Size() > maxPersistedAgentLogReadBytes {
		if _, err := file.Seek(info.Size()-maxPersistedAgentLogReadBytes, io.SeekStart); err != nil {
			return "", false
		}
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxPersistedAgentLogReadBytes))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func isPathWithinDirectory(path string, directory string) bool {
	if strings.TrimSpace(directory) == "" {
		return false
	}
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(filepath.Clean(directory))
	if err != nil {
		return false
	}
	if absPath == absDir {
		return false
	}
	return strings.HasPrefix(absPath, absDir+string(os.PathSeparator))
}

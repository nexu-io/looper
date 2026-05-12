package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type invocation struct {
	Timestamp string   `json:"timestamp"`
	CWD       string   `json:"cwd"`
	Args      []string `json:"args"`
}

func main() {
	logPath := stringsOrDefault(os.Getenv("LOOPER_E2E_FAKE_OSASCRIPT_LOG"), filepath.Join(os.TempDir(), "fake-osascript.jsonl"))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	defer file.Close()
	cwd, _ := os.Getwd()
	payload, _ := json.Marshal(invocation{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), CWD: cwd, Args: os.Args[1:]})
	_, _ = file.Write(append(payload, '\n'))
	_, _ = fmt.Fprintln(os.Stdout, "ok")
}

func stringsOrDefault(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

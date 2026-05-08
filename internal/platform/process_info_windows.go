//go:build windows

package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

func ProcessCommand(pid int) (string, error) {
	out, err := exec.Command("wmic", "process", "where", fmt.Sprintf("ProcessId=%d", pid), "get", "CommandLine", "/format:list").Output()
	if err != nil {
		return "", fmt.Errorf("get process command: %w", err)
	}
	line := strings.TrimSpace(string(out))
	_, after, ok := strings.Cut(line, "=")
	if !ok {
		return "", fmt.Errorf("parse wmic output for pid %d", pid)
	}
	return strings.TrimSpace(after), nil
}

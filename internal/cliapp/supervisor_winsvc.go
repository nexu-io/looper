package cliapp

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

const windowsServiceName = "looperd"

func (r *commandRuntime) startWindowsServiceDaemon(ctx context.Context, out io.Writer, loaded config.LoadedFileConfig, binary *resolvedDaemonBinary, args []string, cwd string, env []string, client *DaemonAPIClient, apiURL string) error {
	if r.platform() != "windows" {
		return fmt.Errorf("daemon.mode=windows-service is only supported on Windows")
	}
	scPath, err := r.lookPath()("sc")
	if err != nil {
		return fmt.Errorf("sc.exe not found: Windows service supervision requires sc.exe (Service Controller)")
	}
	logDir := loaded.Config.Daemon.LogDir
	if err := r.installWindowsService(ctx, scPath, binary.Path, logDir, loaded); err != nil {
		return err
	}
	result, err := r.runCommand(ctx, scPath, []string{"start", windowsServiceName}, daemonCommandTimeout)
	if err != nil {
		return fmt.Errorf("sc start failed: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sc start failed: %s", strings.TrimSpace(result.Stderr))
	}
	statePath, _ := r.resolveDaemonStatePath()
	now := time.Now().UTC()
	state := daemonLifecycleState{
		SchemaVersion: daemonStateSchemaVersion,
		Mode:          config.DaemonModeWindowsService,
		PID:           0,
		StartedAt:     &now,
		BinaryPath:    binary.Path,
		Logs:          daemonLogState(loaded),
		Supervisor: &daemonSupervisorState{
			Source:                 "windows-service",
			Label:                  windowsServiceName,
			RestartPolicy:          loaded.Config.Daemon.RestartPolicy,
			RestartThrottleSeconds: loaded.Config.Daemon.RestartThrottleSeconds,
		},
	}
	_ = r.writeDaemonLifecycleState(statePath, state)
	_, _ = fmt.Fprintf(out, "Started looperd as Windows Service (%s)\nService name: %s\nLogs: %s\n", binary.Path, windowsServiceName, logDir)
	return nil
}

func (r *commandRuntime) refreshWindowsServiceLifecycleState(ctx context.Context, statePath string, state *daemonLifecycleState) (*daemonLifecycleState, error) {
	if state == nil || r.platform() != "windows" {
		return state, nil
	}
	scPath, err := r.lookPath()("sc")
	if err != nil {
		return state, fmt.Errorf("sc.exe not found: cannot check Windows Service status")
	}
	result, err := r.runCommand(ctx, scPath, []string{"query", windowsServiceName}, daemonCommandTimeout)
	if err != nil || result.ExitCode != 0 {
		if state.PID == 0 {
			return state, nil
		}
		updated := *state
		updated.PID = 0
		now := time.Now().UTC()
		updated.LastExit = &daemonLifecycleExitState{At: now, Reason: fmt.Sprintf("Windows Service %s is not running", windowsServiceName), LogPath: state.Logs.Main}
		updated.LastError = updated.LastExit.Reason
		if err := r.writeDaemonLifecycleState(statePath, updated); err != nil {
			return state, err
		}
		pidPath, err := r.resolveDaemonPIDFilePath()
		if err == nil {
			r.removePIDFile(pidPath)
		}
		return &updated, nil
	}
	return state, nil
}

func (r *commandRuntime) stopWindowsServiceDaemon(ctx context.Context, loaded config.LoadedFileConfig, state *daemonLifecycleState) error {
	if r.platform() != "windows" {
		return fmt.Errorf("daemon.mode=windows-service is only supported on Windows")
	}
	scPath, err := r.lookPath()("sc")
	if err != nil {
		return fmt.Errorf("sc.exe not found: cannot stop Windows Service looperd")
	}
	result, err := r.runCommand(ctx, scPath, []string{"stop", windowsServiceName}, daemonCommandTimeout)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sc stop failed: %s", strings.TrimSpace(result.Stderr))
	}
	result, err = r.runCommand(ctx, scPath, []string{"delete", windowsServiceName}, daemonCommandTimeout)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sc delete failed: %s", strings.TrimSpace(result.Stderr))
	}
	statePath, _ := r.resolveDaemonStatePath()
	if state != nil {
		now := time.Now().UTC()
		state.LastExit = &daemonLifecycleExitState{At: now, Reason: "stopped by looper daemon stop", LogPath: state.Logs.Main}
		state.PID = 0
		_ = r.writeDaemonLifecycleState(statePath, *state)
	}
	pidPath, _ := r.resolveDaemonPIDFilePath()
	r.removePIDFile(pidPath)
	return nil
}

func (r *commandRuntime) installWindowsService(ctx context.Context, scPath, binaryPath, logDir string, loaded config.LoadedFileConfig) error {
	daemonArgs := looperdArgsForMode(loaded, config.DaemonModeForeground)
	programArgs := append([]string{binaryPath}, daemonArgs...)
	binPath := strings.Join(programArgs, " ")

	displayName := "Looper Daemon"
	description := "Looper automation daemon"

	_, _ = r.runCommand(ctx, scPath, []string{"delete", windowsServiceName}, daemonCommandTimeout)
	createArgs := []string{
		"create", windowsServiceName,
		"binPath=", binPath,
		"start=", "auto",
		"DisplayName=", displayName,
	}
	result, err := r.runCommand(ctx, scPath, createArgs, daemonCommandTimeout)
	if err != nil {
		return fmt.Errorf("sc create failed: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sc create failed: %s", strings.TrimSpace(result.Stderr))
	}
	_, _ = r.runCommand(ctx, scPath, []string{"description", windowsServiceName, description}, daemonCommandTimeout)
	if logDir != "" {
		logPath := filepath.Join(logDir, "looperd.log")
		_, _ = r.runCommand(ctx, scPath, []string{"qc", windowsServiceName}, daemonCommandTimeout)
		_ = logPath
	}
	return nil
}

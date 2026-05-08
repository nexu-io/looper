package cliapp

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

const systemdServiceName = "looperd"

func (r *commandRuntime) systemdUnitPath() (string, error) {
	homeDir, err := r.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".config", "systemd", "user", systemdServiceName+".service"), nil
}

func (r *commandRuntime) installSystemdUnit(ctx context.Context, binaryPath, logDir string, loaded config.LoadedFileConfig) (string, error) {
	unitPath, err := r.systemdUnitPath()
	if err != nil {
		return "", err
	}
	if err := r.mkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return "", fmt.Errorf("create systemd user unit directory: %w", err)
	}
	args := looperdArgsForMode(loaded, config.DaemonModeForeground)
	programArgs := append([]string{binaryPath}, args...)

	envBlock := ""
	if len(loaded.Config.Daemon.Environment) > 0 {
		keys := make([]string, 0, len(loaded.Config.Daemon.Environment))
		for k := range loaded.Config.Daemon.Environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var envBuf bytes.Buffer
		for _, k := range keys {
			envBuf.WriteString(fmt.Sprintf("Environment=%q=%q\n", k, loaded.Config.Daemon.Environment[k]))
		}
		envBlock = envBuf.String()
	}

	restart := "on-failure"
	switch loaded.Config.Daemon.RestartPolicy {
	case config.DaemonRestartAlways:
		restart = "always"
	case config.DaemonRestartNever:
		restart = "no"
	}

	unitContent := fmt.Sprintf(`[Unit]
Description=Looper Daemon
After=network.target

[Service]
Type=simple
ExecStart=%s
WorkingDirectory=%s
Restart=%s
RestartSec=%d
%s
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, html.EscapeString(strings.Join(programArgs, " ")),
		html.EscapeString(loaded.Config.Daemon.WorkingDirectory),
		restart,
		loaded.Config.Daemon.RestartThrottleSeconds,
		envBlock,
		html.EscapeString(filepath.Join(logDir, "looperd.log")),
		html.EscapeString(filepath.Join(logDir, "looperd.log")))

	if err := r.writeFile(unitPath, []byte(unitContent), 0o644); err != nil {
		return "", fmt.Errorf("write systemd unit: %w", err)
	}
	return unitPath, nil
}

func (r *commandRuntime) startSystemdDaemon(ctx context.Context, out io.Writer, loaded config.LoadedFileConfig, binary *resolvedDaemonBinary, args []string, cwd string, env []string, client *DaemonAPIClient, apiURL string) error {
	if r.platform() != "linux" {
		return fmt.Errorf("daemon.mode=systemd is only supported on Linux")
	}
	systemctlPath, err := r.lookPath()("systemctl")
	if err != nil {
		return fmt.Errorf("systemctl not found: systemd supervision requires systemctl")
	}
	logDir := loaded.Config.Daemon.LogDir
	unitPath, err := r.installSystemdUnit(ctx, binary.Path, logDir, loaded)
	if err != nil {
		return err
	}
	_, _ = r.runCommand(ctx, systemctlPath, []string{"--user", "daemon-reload"}, daemonCommandTimeout)
	_, _ = r.runCommand(ctx, systemctlPath, []string{"--user", "stop", systemdServiceName}, daemonCommandTimeout)
	result, err := r.runCommand(ctx, systemctlPath, []string{"--user", "start", systemdServiceName}, daemonCommandTimeout)
	if err != nil {
		return fmt.Errorf("systemctl start failed: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("systemctl start failed: %s", strings.TrimSpace(result.Stderr))
	}
	statePath, _ := r.resolveDaemonStatePath()
	now := time.Now().UTC()
	state := daemonLifecycleState{
		SchemaVersion: daemonStateSchemaVersion,
		Mode:          config.DaemonModeSystemd,
		PID:           0,
		StartedAt:     &now,
		BinaryPath:    binary.Path,
		Logs:          daemonLogState(loaded),
		Supervisor: &daemonSupervisorState{
			Source:                 "systemd",
			Label:                  systemdServiceName,
			RestartPolicy:          loaded.Config.Daemon.RestartPolicy,
			RestartThrottleSeconds: loaded.Config.Daemon.RestartThrottleSeconds,
		},
	}
	_ = r.writeDaemonLifecycleState(statePath, state)
	_, _ = fmt.Fprintf(out, "Started looperd under systemd supervision (%s)\nSupervisor: systemd user unit %s\nUnit: %s\nLogs: %s\n", binary.Path, systemdServiceName, unitPath, logDir)
	return nil
}

func (r *commandRuntime) refreshSystemdLifecycleState(ctx context.Context, statePath string, state *daemonLifecycleState) (*daemonLifecycleState, error) {
	if state == nil || r.platform() != "linux" {
		return state, nil
	}
	systemctlPath, err := r.lookPath()("systemctl")
	if err != nil {
		return state, fmt.Errorf("systemctl not found: cannot check systemd service status")
	}
	result, err := r.runCommand(ctx, systemctlPath, []string{"--user", "is-active", systemdServiceName}, daemonCommandTimeout)
	if err != nil || result.ExitCode != 0 {
		if state.PID == 0 {
			return state, nil
		}
		updated := *state
		updated.PID = 0
		now := time.Now().UTC()
		updated.LastExit = &daemonLifecycleExitState{At: now, Reason: fmt.Sprintf("systemd service %s is not active", systemdServiceName), LogPath: state.Logs.Main}
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

func (r *commandRuntime) stopSystemdDaemon(ctx context.Context, loaded config.LoadedFileConfig, state *daemonLifecycleState) error {
	if r.platform() != "linux" {
		return fmt.Errorf("daemon.mode=systemd is only supported on Linux")
	}
	systemctlPath, err := r.lookPath()("systemctl")
	if err != nil {
		return fmt.Errorf("systemctl not found: cannot stop systemd-supervised looperd")
	}
	result, err := r.runCommand(ctx, systemctlPath, []string{"--user", "stop", systemdServiceName}, daemonCommandTimeout)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("systemctl stop failed: %s", strings.TrimSpace(result.Stderr))
	}
	unitPath, _ := r.systemdUnitPath()
	_ = r.removeFile(unitPath)
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

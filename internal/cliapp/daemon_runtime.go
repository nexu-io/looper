package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/spf13/cobra"
)

const (
	daemonCommandTimeout       = 5 * time.Second
	daemonStartReadyTimeout    = 30 * time.Second
	daemonStartReadyPollPeriod = 100 * time.Millisecond
)

type commandExecutionResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type runCommandFunc func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error)
type spawnDetachedFunc func(command string, args []string, cwd string, env []string) (int, error)
type killProcessFunc func(pid int, signal int) error
type readFileFunc func(path string) ([]byte, error)
type writeFileFunc func(path string, data []byte, perm os.FileMode) error
type removeFileFunc func(path string) error
type mkdirAllFunc func(path string, perm os.FileMode) error
type sleepFunc func(time.Duration)
type getwdFunc func() (string, error)

type daemonVersionState struct {
	Version    string
	Source     string
	BinaryPath *string
}

type daemonStatusOutput struct {
	Mode                   config.DaemonMode          `json:"mode"`
	ConfiguredMode         config.DaemonMode          `json:"configuredMode"`
	RunningMode            *config.DaemonMode         `json:"runningMode,omitempty"`
	RestartPolicy          config.DaemonRestartPolicy `json:"restartPolicy"`
	RestartThrottleSeconds int                        `json:"restartThrottleSeconds"`
	ConfigPath             string                     `json:"configPath"`
	LogDir                 string                     `json:"logDir"`
	APIReachable           bool                       `json:"apiReachable"`
	DaemonVersion          *string                    `json:"daemonVersion"`
	DaemonVersionSource    *string                    `json:"daemonVersionSource"`
	DaemonBinaryPath       *string                    `json:"daemonBinaryPath"`
	Lifecycle              daemonLifecycleStatus      `json:"lifecycle"`
	Status                 json.RawMessage            `json:"status"`
	Health                 json.RawMessage            `json:"health"`
}

type daemonLogsOutput struct {
	LogPath  string   `json:"logPath"`
	LogPaths []string `json:"logPaths"`
	Full     bool     `json:"full"`
	Lines    []string `json:"lines"`
}

func (r *commandRuntime) daemonStatus(cmd *cobra.Command, args []string) error {
	_ = args

	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}

	client := r.apiClientFromLoaded(loaded)
	statusPayload, statusErr := r.getJSONWithClient(cmd.Context(), client, "/api/v1/status")
	apiReachable := statusErr == nil
	var healthPayload json.RawMessage
	if statusErr != nil {
		healthPayload, err = r.getJSONWithClient(cmd.Context(), client, "/api/v1/healthz")
		if err == nil {
			apiReachable = true
		}
	}

	versionState, err := r.detectDaemonVersionState(cmd.Context(), statusPayload)
	if err != nil {
		return err
	}
	lifecycle, err := r.daemonLifecycleStatus(cmd.Context(), loaded)
	if err != nil {
		return err
	}

	output := daemonStatusOutput{
		Mode:                   loaded.Config.Daemon.Mode,
		ConfiguredMode:         loaded.Config.Daemon.Mode,
		RestartPolicy:          loaded.Config.Daemon.RestartPolicy,
		RestartThrottleSeconds: loaded.Config.Daemon.RestartThrottleSeconds,
		ConfigPath:             loaded.Metadata.ConfigPath,
		LogDir:                 loaded.Config.Daemon.LogDir,
		APIReachable:           apiReachable,
		Lifecycle:              lifecycle,
		Status:                 statusPayload,
		Health:                 healthPayload,
	}
	if lifecycle.State != nil && lifecycle.Process == "running" {
		runningMode := lifecycle.State.Mode
		output.RunningMode = &runningMode
	}
	if versionState != nil {
		output.DaemonVersion = &versionState.Version
		output.DaemonVersionSource = &versionState.Source
		output.DaemonBinaryPath = versionState.BinaryPath
	}

	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}

	return writeHumanDaemonStatus(cmd.OutOrStdout(), output)
}

func (r *commandRuntime) daemonStart(cmd *cobra.Command, args []string) error {
	_ = args

	ctx := cmd.Context()
	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}
	client := r.localAPIClientFromLoaded(loaded)
	apiURL := client.baseURL
	if !r.skipAPIStartProbe {
		lifecycle, err := r.daemonLifecycleStatus(ctx, loaded)
		if err != nil {
			return err
		}
		if lifecycle.Process == "running" {
			if lifecycle.State != nil && lifecycle.State.Mode != loaded.Config.Daemon.Mode {
				return fmt.Errorf("looperd is already running in %s mode; refusing to start %s mode. Stop the existing daemon first with `looper daemon stop`", lifecycle.State.Mode, loaded.Config.Daemon.Mode)
			}
			if lifecycle.State != nil {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "looperd already appears to be running in %s mode (pid %d)\n", lifecycle.State.Mode, lifecycle.State.PID); err != nil {
					return err
				}
				return nil
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "looperd already appears to be running from an existing pid file"); err != nil {
				return err
			}
			return nil
		}
		if lifecycle.Process == "not-looperd" {
			pid := 0
			if lifecycle.State != nil {
				pid = lifecycle.State.PID
			} else if filePID, ok := r.readPIDFile(lifecycle.PIDPath); ok {
				pid = filePID
			}
			return fmt.Errorf("daemon lifecycle state or pid file points to alive non-looperd process %d; refusing to start %s mode", pid, loaded.Config.Daemon.Mode)
		}
	}

	if loaded.Config.Daemon.Mode == config.DaemonModeLaunchd {
		if !r.skipAPIStartProbe {
			probe, err := r.probeDaemonStatus(ctx, client)
			if err != nil {
				return err
			}
			if probe.isLooperd {
				return fmt.Errorf("looperd already appears to be serving %s but is not recorded as launchd-supervised; stop the existing daemon before starting launchd supervision", apiURL)
			}
		}
		binary, err := r.resolveDaemonBinary(ctx)
		if err != nil {
			return err
		}
		env := os.Environ()
		extractedArgs := ExtractConfigArgs(r.argv)
		callerCWD, err := r.daemonSpawnCallerCWD(extractedArgs, env)
		if err != nil {
			return err
		}
		forwardedArgs := normalizeForwardedConfigPathArgs(extractedArgs, callerCWD)
		cwd := loaded.Config.Daemon.WorkingDirectory
		if strings.TrimSpace(callerCWD) == "" && strings.TrimSpace(cwd) != "" && !filepath.IsAbs(cwd) {
			callerCWD, err = r.getwd()
			if err != nil {
				return fmt.Errorf("resolve current working directory for launchd WorkingDirectory: %w", err)
			}
		}
		if strings.TrimSpace(callerCWD) != "" && strings.TrimSpace(cwd) != "" && !filepath.IsAbs(cwd) {
			cwd = filepath.Join(callerCWD, cwd)
		}
		return r.startLaunchdDaemon(ctx, cmd.OutOrStdout(), loaded, binary, forwardedArgs, cwd, daemonSpawnEnv(env, loaded.Metadata.ConfigPath, callerCWD), client, apiURL)
	}

	pidFilePath, err := r.resolveDaemonPIDFilePath()
	if err != nil {
		return err
	}

	if existingPID, ok := r.readPIDFile(pidFilePath); ok {
		if r.isProcessAlive(existingPID) {
			isLooperd, err := r.isLooperdProcess(ctx, existingPID)
			if err != nil {
				return err
			}
			if isLooperd {
				probe, err := r.probeDaemonStatus(ctx, client)
				if err != nil {
					return err
				}
				if !probe.isLooperd {
					return fmt.Errorf("Daemon pid %d appears to be looperd, but %s did not confirm a running looperd", existingPID, apiURL)
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "looperd already appears to be running at %s (pid %d)\n", apiURL, existingPID); err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Phase 1 process management is minimal: use `looper daemon restart` or stop the process manually if needed.")
				return err
			}

			probe, err := r.probeDaemonStatus(ctx, client)
			if err != nil {
				return err
			}
			if probe.isLooperd {
				r.removePIDFile(pidFilePath)
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "looperd already appears to be running at %s (pid file points to non-looperd pid %d)\n", apiURL, existingPID); err != nil {
					return err
				}
				return nil
			}
			return fmt.Errorf("Daemon pid file points to alive non-looperd process %d; refusing to overwrite it", existingPID)
		} else {
			r.removePIDFile(pidFilePath)
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed stale daemon pid file for pid %d\n", existingPID); err != nil {
				return err
			}
		}
	}
	if !r.skipAPIStartProbe {
		probe, err := r.probeDaemonStatus(ctx, client)
		if err != nil {
			return err
		}
		if probe.isLooperd {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "looperd already appears to be running at %s (pid file missing)\n", apiURL); err != nil {
				return err
			}
			return nil
		}
	}

	binary, err := r.resolveDaemonBinary(ctx)
	if err != nil {
		return err
	}

	cwd := loaded.Config.Daemon.WorkingDirectory
	startupLogPath, err := r.createStartupLogPath(loaded)
	if err != nil {
		return err
	}

	env := os.Environ()
	extractedArgs := ExtractConfigArgs(r.argv)
	callerCWD, err := r.daemonSpawnCallerCWD(extractedArgs, env)
	if err != nil {
		return err
	}
	forwardedArgs := normalizeForwardedConfigPathArgs(extractedArgs, callerCWD)
	if strings.TrimSpace(callerCWD) != "" && strings.TrimSpace(cwd) != "" && !filepath.IsAbs(cwd) {
		cwd = filepath.Join(callerCWD, cwd)
	}

	r.startupOutputPath = startupLogPath
	pid, err := r.spawnDetached(binary.Path, forwardedArgs, cwd, daemonSpawnEnv(env, loaded.Metadata.ConfigPath, callerCWD))
	if err != nil {
		return fmt.Errorf("Failed to start looperd: %w", err)
	}
	if pid <= 0 {
		return fmt.Errorf("Failed to start looperd: process did not report a pid")
	}

	err = r.waitForDaemonReady(ctx, client, pid, startupLogPath, apiURL, r.daemonStartReadyTimeout(), daemonStartReadyPollPeriod)
	if err != nil {
		if r.isProcessAlive(pid) {
			_ = r.killProcess(pid, int(syscall.SIGTERM))
		}
		r.removePIDFile(pidFilePath)
		if statePath, stateErr := r.resolveDaemonStatePath(); stateErr == nil {
			now := time.Now().UTC()
			state := daemonLifecycleState{SchemaVersion: daemonStateSchemaVersion, Mode: config.DaemonModeForeground, PID: pid, StartedAt: &now, BinaryPath: binary.Path, Logs: daemonLogState(loaded), LastExit: &daemonLifecycleExitState{At: now, Reason: "detached looperd exited or failed readiness during startup", LogPath: startupLogPath}, LastError: err.Error()}
			_ = r.writeDaemonLifecycleState(statePath, state)
		}
		return err
	}

	if err := r.mkdirAll(filepath.Dir(pidFilePath), 0o755); err != nil {
		return fmt.Errorf("create daemon pid directory: %w", err)
	}
	if err := r.writeFile(pidFilePath, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		return fmt.Errorf("write daemon pid file: %w", err)
	}
	statePath, err := r.resolveDaemonStatePath()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	state := daemonLifecycleState{SchemaVersion: daemonStateSchemaVersion, Mode: config.DaemonModeForeground, PID: pid, StartedAt: &now, BinaryPath: binary.Path, Logs: daemonLogState(loaded)}
	if err := r.writeDaemonLifecycleState(statePath, state); err != nil {
		return fmt.Errorf("write daemon lifecycle state: %w", err)
	}

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Started looperd detached, not supervised (%s) with pid %d\n", binary.Path, pid); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "PID file: %s\n", pidFilePath); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "State file: %s\n", statePath); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Startup log: %s\n", startupLogPath); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Detached mode does not restart after crashes or system reboot. Use --daemon-mode launchd on macOS for supervised lifecycle management.")
	return err
}

func (r *commandRuntime) daemonStartReadyTimeout() time.Duration {
	if r.app != nil && r.app.deps.DaemonStartTimeout > 0 {
		return r.app.deps.DaemonStartTimeout
	}
	return daemonStartReadyTimeout
}

func (r *commandRuntime) daemonSpawnCallerCWD(args []string, env []string) (string, error) {
	if !forwardedConfigArgsNeedCWD(args) && !daemonSpawnEnvNeedsCWD(env) {
		return "", nil
	}
	cwd, err := r.getwd()
	if err != nil {
		return "", fmt.Errorf("resolve current working directory for daemon config paths: %w", err)
	}
	if strings.TrimSpace(cwd) == "" {
		return "", fmt.Errorf("resolve current working directory for daemon config paths: current working directory is empty")
	}
	return cwd, nil
}

var forwardedConfigPathFlagNames = map[string]struct{}{
	"config":         {},
	"db-path":        {},
	"log-dir":        {},
	"git-path":       {},
	"gh-path":        {},
	"looper-path":    {},
	"osascript-path": {},
}

var daemonSpawnPathEnvNames = map[string]struct{}{
	"LOOPER_DB_PATH":           {},
	"LOOPER_LOG_DIR":           {},
	"LOOPER_WORKING_DIRECTORY": {},
	"LOOPER_GIT_PATH":          {},
	"LOOPER_GH_PATH":           {},
	"LOOPER_LOOPER_PATH":       {},
	"LOOPER_OSASCRIPT_PATH":    {},
}

func forwardedConfigArgsNeedCWD(args []string) bool {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		name, value, hasValue := splitLongFlag(arg)
		if _, ok := forwardedConfigPathFlagNames[name]; !ok {
			continue
		}
		if hasValue {
			if value != "" && !filepath.IsAbs(value) {
				return true
			}
			continue
		}
		if index+1 < len(args) && args[index+1] != "" && !filepath.IsAbs(args[index+1]) {
			return true
		}
		index++
	}
	return false
}

func normalizeForwardedConfigPathArgs(args []string, cwd string) []string {
	normalized := append([]string{}, args...)
	if strings.TrimSpace(cwd) == "" {
		return normalized
	}
	for index := 0; index < len(normalized); index++ {
		arg := normalized[index]
		name, value, hasValue := splitLongFlag(arg)
		if _, ok := forwardedConfigPathFlagNames[name]; !ok {
			continue
		}
		if hasValue {
			if value != "" && !filepath.IsAbs(value) {
				normalized[index] = "--" + name + "=" + filepath.Join(cwd, value)
			}
			continue
		}
		if index+1 < len(normalized) {
			if value := normalized[index+1]; value != "" && !filepath.IsAbs(value) {
				normalized[index+1] = filepath.Join(cwd, value)
			}
			index++
		}
	}
	return normalized
}

func splitLongFlag(arg string) (name string, value string, hasValue bool) {
	if !strings.HasPrefix(arg, "--") {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(arg, "--")
	if equals := strings.Index(trimmed, "="); equals >= 0 {
		return trimmed[:equals], trimmed[equals+1:], true
	}
	return trimmed, "", false
}

func daemonSpawnEnv(env []string, configPath string, cwd string) []string {
	spawnEnv := append([]string{}, env...)
	if strings.TrimSpace(cwd) != "" {
		for index, entry := range spawnEnv {
			name, value, ok := strings.Cut(entry, "=")
			if !ok {
				continue
			}
			if _, pathEnv := daemonSpawnPathEnvNames[name]; pathEnv && value != "" && !filepath.IsAbs(value) {
				spawnEnv[index] = name + "=" + filepath.Join(cwd, value)
			}
		}
	}
	if strings.TrimSpace(configPath) == "" {
		return spawnEnv
	}
	for index, entry := range spawnEnv {
		if strings.HasPrefix(entry, "LOOPER_CONFIG=") {
			spawnEnv[index] = "LOOPER_CONFIG=" + configPath
			return spawnEnv
		}
	}
	return append(spawnEnv, "LOOPER_CONFIG="+configPath)
}

func daemonSpawnEnvNeedsCWD(env []string) bool {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, pathEnv := daemonSpawnPathEnvNames[name]; pathEnv && value != "" && !filepath.IsAbs(value) {
			return true
		}
	}
	return false
}

func (r *commandRuntime) daemonRestart(cmd *cobra.Command, args []string) error {
	_ = args

	stopped, err := r.stopDaemonProcess(cmd.Context(), cmd.OutOrStdout(), true)
	if err != nil {
		return err
	}

	r.skipAPIStartProbe = stopped
	return r.daemonStart(cmd, nil)
}

func (r *commandRuntime) daemonStop(cmd *cobra.Command, args []string) error {
	_ = args

	_, err := r.stopDaemonProcess(cmd.Context(), cmd.OutOrStdout(), false)
	return err
}

func (r *commandRuntime) stopDaemonProcess(ctx context.Context, out io.Writer, startIfMissing bool) (bool, error) {
	statePath, err := r.resolveDaemonStatePath()
	if err != nil {
		return false, err
	}
	state, err := r.readDaemonLifecycleState(statePath)
	if err != nil {
		return false, err
	}
	if state != nil && state.Mode == config.DaemonModeLaunchd {
		loaded, err := r.loadConfig()
		if err != nil {
			return false, err
		}
		return r.stopLaunchdDaemon(ctx, out, loaded, state, startIfMissing)
	}

	pidFilePath, err := r.resolveDaemonPIDFilePath()
	if err != nil {
		return false, err
	}

	existingPID, ok := r.readPIDFile(pidFilePath)
	if !ok {
		loaded, loadErr := r.loadConfig()
		if loadErr == nil && loaded.Config.Daemon.Mode == config.DaemonModeLaunchd {
			return r.stopLaunchdDaemon(ctx, out, loaded, nil, startIfMissing)
		}
		if startIfMissing {
			if _, err := fmt.Fprintln(out, "No daemon pid file found; starting daemon."); err != nil {
				return false, err
			}
			return false, nil
		}
		if _, err := fmt.Fprintln(out, "No daemon pid file found; nothing to stop."); err != nil {
			return false, err
		}
		return false, nil
	}

	if !r.isProcessAlive(existingPID) {
		if startIfMissing {
			if _, err := fmt.Fprintf(out, "Daemon pid %d is stale; starting daemon.\n", existingPID); err != nil {
				return false, err
			}
			r.removePIDFile(pidFilePath)
			return false, nil
		}
		r.removePIDFile(pidFilePath)
		if _, err := fmt.Fprintf(out, "Removed stale daemon pid file for pid %d\n", existingPID); err != nil {
			return false, err
		}
		return false, nil
	}

	isLooperd, err := r.isLooperdProcess(ctx, existingPID)
	if err != nil {
		return false, err
	}
	if !isLooperd {
		if _, err := fmt.Fprintf(out, "Daemon pid %d does not appear to be looperd; treating pid file as stale.\n", existingPID); err != nil {
			return false, err
		}
		r.removePIDFile(pidFilePath)
		if startIfMissing {
			return false, nil
		}
		return false, nil
	}

	if err := r.killProcess(existingPID, int(syscall.SIGTERM)); err != nil {
		return false, fmt.Errorf("stop looperd pid %d: %w", existingPID, err)
	}
	if err := r.waitForProcessExit(existingPID, 2*time.Second, 100*time.Millisecond); err != nil {
		return false, err
	}
	r.removePIDFile(pidFilePath)
	if state != nil {
		now := time.Now().UTC()
		state.LastExit = &daemonLifecycleExitState{At: now, Reason: "stopped by looper daemon stop", LogPath: state.Logs.Main}
		state.PID = 0
		_ = r.writeDaemonLifecycleState(statePath, *state)
	}
	if _, err := fmt.Fprintf(out, "Stopped looperd pid %d\n", existingPID); err != nil {
		return false, err
	}
	return true, nil
}

func (r *commandRuntime) daemonLogs(cmd *cobra.Command, args []string) error {
	_ = args

	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}

	full := getBoolFlag(cmd, "full")
	lineCountValue := strings.TrimSpace(getStringFlag(cmd, "lines"))
	lineCount := int64(50)
	if lineCountValue != "" {
		if full {
			return fmt.Errorf("--full cannot be combined with --lines")
		}
		lineCount, err = parsePositiveInt(lineCountValue, "--lines")
		if err != nil {
			return err
		}
	}

	logPath := filepath.Join(loaded.Config.Daemon.LogDir, "looperd.log")
	var content string
	var logPaths []string
	if getBoolFlag(cmd, "startup") {
		logPath = filepath.Join(loaded.Config.Daemon.LogDir, "startup")
		content, logPaths, err = r.readStartupDaemonLogs(logPath)
	} else {
		content, logPaths, err = r.readRetainedDaemonLogs(logPath, loaded.Config.Logging.MaxFiles)
	}
	if err != nil {
		return err
	}

	lines := splitLogLines(content)
	if !full {
		lines = tailLines(strings.Join(lines, "\n"), int(lineCount))
	}
	output := daemonLogsOutput{LogPath: logPath, LogPaths: logPaths, Full: full, Lines: lines}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s (retained files: %d; default tail: %d lines; use --full to show all retained logs)\n", logPath, len(logPaths), lineCount); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	return nil
}

func (r *commandRuntime) readStartupDaemonLogs(startupDir string) (string, []string, error) {
	paths, err := filepath.Glob(filepath.Join(startupDir, "looperd-*.log"))
	if err != nil {
		return "", nil, err
	}
	if len(paths) == 0 {
		return "", nil, os.ErrNotExist
	}
	sort.Strings(paths)
	if len(paths) > 10 {
		paths = paths[len(paths)-10:]
	}
	var builder strings.Builder
	readPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		raw, err := r.readFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", nil, err
		}
		if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString(strings.TrimRight(string(raw), "\n"))
		builder.WriteString("\n")
		readPaths = append(readPaths, path)
	}
	if len(readPaths) == 0 {
		return "", nil, os.ErrNotExist
	}
	return strings.TrimRight(builder.String(), "\n"), readPaths, nil
}

func (r *commandRuntime) readRetainedDaemonLogs(logPath string, maxFiles int) (string, []string, error) {
	paths := retainedDaemonLogPaths(logPath, maxFiles)
	var builder strings.Builder
	readPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		raw, err := r.readFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", nil, err
		}
		if len(raw) == 0 {
			readPaths = append(readPaths, path)
			continue
		}
		if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString(strings.TrimRight(string(raw), "\n"))
		builder.WriteString("\n")
		readPaths = append(readPaths, path)
	}
	if len(readPaths) == 0 {
		return "", nil, os.ErrNotExist
	}
	return strings.TrimRight(builder.String(), "\n"), readPaths, nil
}

func retainedDaemonLogPaths(logPath string, maxFiles int) []string {
	if maxFiles < 1 {
		maxFiles = 1
	}
	paths := make([]string, 0, maxFiles)
	for index := maxFiles - 1; index >= 1; index-- {
		paths = append(paths, fmt.Sprintf("%s.%d", logPath, index))
	}
	paths = append(paths, logPath)
	return paths
}

func splitLogLines(content string) []string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return []string{}
	}
	return strings.Split(content, "\n")
}

func (r *commandRuntime) loadConfig() (config.LoadedFileConfig, error) {
	return config.LoadFile(config.LoadFileOptions{Args: ExtractConfigArgs(r.argv)})
}

func (r *commandRuntime) getJSONWithClient(ctx context.Context, client *DaemonAPIClient, path string) (json.RawMessage, error) {
	var payload json.RawMessage
	if err := client.Get(ctx, path, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (r *commandRuntime) apiClient() (*DaemonAPIClient, error) {
	loaded, err := r.loadConfig()
	if err != nil {
		return nil, err
	}
	return r.apiClientFromLoaded(loaded), nil
}

func (r *commandRuntime) apiClientFromLoaded(loaded config.LoadedFileConfig) *DaemonAPIClient {
	baseURL := ""
	if loaded.Config.Server.BaseURL != nil && strings.TrimSpace(*loaded.Config.Server.BaseURL) != "" {
		baseURL = strings.TrimSpace(*loaded.Config.Server.BaseURL)
	} else {
		baseURL = fmt.Sprintf("http://%s:%d", loaded.Config.Server.Host, loaded.Config.Server.Port)
	}

	return r.newAPIClientForLoaded(loaded, baseURL)
}

func (r *commandRuntime) localAPIClientFromLoaded(loaded config.LoadedFileConfig) *DaemonAPIClient {
	baseURL := fmt.Sprintf("http://%s:%d", loaded.Config.Server.Host, loaded.Config.Server.Port)
	return r.newAPIClientForLoaded(loaded, baseURL)
}

func (r *commandRuntime) newAPIClientForLoaded(loaded config.LoadedFileConfig, baseURL string) *DaemonAPIClient {
	token := ""
	if loaded.Config.Server.AuthMode == config.AuthModeLocalToken && loaded.Config.Server.LocalToken != nil {
		token = strings.TrimSpace(*loaded.Config.Server.LocalToken)
	}

	return NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: baseURL, Token: token, HTTPClient: r.httpClient()})
}

func (r *commandRuntime) detectDaemonVersionState(ctx context.Context, statusPayload json.RawMessage) (*daemonVersionState, error) {
	serviceBinary := extractDaemonServiceBinary(statusPayload)
	if serviceBinary.Version != "" {
		state := &daemonVersionState{Version: serviceBinary.Version, Source: "api"}
		if serviceBinary.Path != "" {
			state.BinaryPath = stringPtr(serviceBinary.Path)
		}
		return state, nil
	}

	managedVersion, err := r.readManagedDaemonVersion(ctx)
	if err != nil {
		return nil, err
	}
	if managedVersion != nil {
		return managedVersion, nil
	}

	return r.readPathDaemonVersion(ctx)
}

type daemonServiceBinary struct {
	Name    string
	Version string
	Path    string
}

func extractDaemonServiceBinary(payload json.RawMessage) daemonServiceBinary {
	if len(payload) == 0 {
		return daemonServiceBinary{}
	}

	var decoded struct {
		Service struct {
			Version string `json:"version"`
			Binary  struct {
				Name string `json:"name"`
				Path string `json:"path"`
			} `json:"binary"`
		} `json:"service"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return daemonServiceBinary{}
	}

	return daemonServiceBinary{
		Name:    strings.TrimSpace(decoded.Service.Binary.Name),
		Version: strings.TrimSpace(decoded.Service.Version),
		Path:    strings.TrimSpace(decoded.Service.Binary.Path),
	}
}

type daemonStatusProbe struct {
	reachable bool
	isLooperd bool
	payload   json.RawMessage
}

func (r *commandRuntime) probeDaemonStatus(ctx context.Context, client *DaemonAPIClient) (daemonStatusProbe, error) {
	payload, err := r.getJSONWithClient(ctx, client, "/api/v1/status")
	if err != nil {
		var apiErr *DaemonAPIError
		if errors.As(err, &apiErr) && apiErr.Status > 0 {
			return daemonStatusProbe{reachable: true}, fmt.Errorf("configured API endpoint %s responded but did not return looperd status: %w", client.baseURL, err)
		}
		return daemonStatusProbe{}, nil
	}

	if !isLooperdStatusPayload(payload) {
		return daemonStatusProbe{reachable: true, payload: payload}, fmt.Errorf("configured API endpoint %s is occupied by another service: /api/v1/status did not identify looperd", client.baseURL)
	}
	return daemonStatusProbe{reachable: true, isLooperd: true, payload: payload}, nil
}

func isLooperdStatusPayload(payload json.RawMessage) bool {
	binary := extractDaemonServiceBinary(payload)
	if binary.Name == looperdBinaryName {
		return true
	}
	return binary.Version != "" && filepath.Base(binary.Path) == looperdBinaryName
}

func (r *commandRuntime) createStartupLogPath(loaded config.LoadedFileConfig) (string, error) {
	startupLogDir := filepath.Join(loaded.Config.Daemon.LogDir, "startup")
	if err := r.mkdirAll(startupLogDir, 0o700); err != nil {
		return "", fmt.Errorf("create daemon startup log directory: %w", err)
	}
	r.pruneStartupLogs(startupLogDir, loaded.Config.Logging.MaxFiles*2)
	return filepath.Join(startupLogDir, fmt.Sprintf("looperd-%s.log", time.Now().UTC().Format("20060102T150405.000000000Z"))), nil
}

func (r *commandRuntime) pruneStartupLogs(startupLogDir string, keep int) {
	if keep < 1 {
		keep = 10
	}
	paths, err := filepath.Glob(filepath.Join(startupLogDir, "looperd-*.log"))
	if err != nil || len(paths) <= keep {
		return
	}
	sort.Strings(paths)
	for _, path := range paths[:len(paths)-keep] {
		_ = r.removeFile(path)
	}
}

func (r *commandRuntime) waitForDaemonReady(ctx context.Context, client *DaemonAPIClient, pid int, startupLogPath string, apiURL string, timeout time.Duration, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastProbeErr error
	for time.Now().Before(deadline) {
		if !r.isProcessAlive(pid) {
			return r.daemonStartupFailureError(fmt.Sprintf("Failed to start looperd: process %d exited during startup", pid), startupLogPath, lastProbeErr)
		}
		payload, err := r.getJSONWithClient(ctx, client, "/api/v1/status")
		if err != nil {
			lastProbeErr = err
		} else if isLooperdStatusPayload(payload) {
			if !r.isProcessAlive(pid) {
				return r.daemonStartupFailureError(fmt.Sprintf("Failed to start looperd: process %d exited during startup", pid), startupLogPath, lastProbeErr)
			}
			return nil
		} else {
			lastProbeErr = fmt.Errorf("/api/v1/status at %s did not identify looperd", apiURL)
		}
		r.sleep(interval)
	}

	message := fmt.Sprintf("Timed out waiting for looperd pid %d to become ready at %s", pid, apiURL)
	return r.daemonStartupFailureError(message, startupLogPath, lastProbeErr)
}

func (r *commandRuntime) daemonStartupFailureError(message string, startupLogPath string, cause error) error {
	var builder strings.Builder
	builder.WriteString(message)
	builder.WriteString(fmt.Sprintf("; startup log: %s", startupLogPath))
	if cause != nil {
		builder.WriteString(fmt.Sprintf("; last readiness error: %v", cause))
	}
	if tail := r.readStartupLogTail(startupLogPath, 20); tail != "" {
		builder.WriteString("; startup log tail:\n")
		builder.WriteString(tail)
	}
	return errors.New(builder.String())
}

func (r *commandRuntime) readStartupLogTail(path string, lineCount int) string {
	raw, err := r.readFile(path)
	if err != nil {
		return ""
	}
	lines := tailLines(strings.TrimRight(string(raw), "\n"), lineCount)
	return strings.Join(lines, "\n")
}

func extractDaemonVersion(payload json.RawMessage) string {
	return extractDaemonServiceBinary(payload).Version
}

type resolvedDaemonBinary struct {
	Path   string
	Source string
}

type daemonBinaryUsability struct {
	Path    string
	Version string
	Exists  bool
}

func (r *commandRuntime) resolveDaemonBinary(ctx context.Context) (*resolvedDaemonBinary, error) {
	managed, err := r.checkManagedDaemonBinary(ctx)
	if err != nil {
		return nil, err
	}
	if managed.Exists {
		return &resolvedDaemonBinary{Path: managed.Path, Source: "installed"}, nil
	}

	version, err := r.runVersionCommandStrict(ctx, looperdBinaryName)
	if version != "" {
		return &resolvedDaemonBinary{Path: looperdBinaryName, Source: "path"}, nil
	}
	_ = err

	return nil, fmt.Errorf("Cannot find looperd binary. Lookup order: ~/.looper/bin/looperd, then $PATH.")
}

func (r *commandRuntime) checkManagedDaemonBinary(ctx context.Context) (daemonBinaryUsability, error) {
	binaryPath, err := r.managedDaemonBinaryPath()
	if err != nil {
		return daemonBinaryUsability{}, err
	}
	info, err := os.Stat(binaryPath)
	if err != nil {
		if os.IsNotExist(err) {
			version, versionErr := r.runVersionCommandStrict(ctx, binaryPath)
			if versionErr == nil && strings.TrimSpace(version) != "" {
				return daemonBinaryUsability{Path: binaryPath, Version: version, Exists: true}, nil
			}
			return daemonBinaryUsability{Path: binaryPath}, nil
		}
		return daemonBinaryUsability{}, fmt.Errorf("check looperd at %s: %w", binaryPath, err)
	}
	if info.IsDir() {
		return daemonBinaryUsability{}, fmt.Errorf("found looperd at %s, but it is a directory\n\nFix: remove or rename %s\nThen reinstall: looper daemon install --force", binaryPath, binaryPath)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return daemonBinaryUsability{}, fmt.Errorf("found looperd at %s, but it is not executable\n\nFix: chmod +x %s\nOr reinstall: looper daemon install --force", binaryPath, binaryPath)
	}
	version, err := r.runVersionCommandStrict(ctx, binaryPath)
	if err != nil {
		return daemonBinaryUsability{}, unusableManagedDaemonError(binaryPath, fmt.Sprintf("version check failed: %v", err))
	}
	if strings.TrimSpace(version) == "" {
		return daemonBinaryUsability{}, unusableManagedDaemonError(binaryPath, "it did not report a version")
	}
	return daemonBinaryUsability{Path: binaryPath, Version: version, Exists: true}, nil
}

func unusableManagedDaemonError(path string, reason string) error {
	return fmt.Errorf("found looperd at %s, but %s\n\nFix: looper daemon install --force", path, reason)
}

func (r *commandRuntime) readManagedDaemonVersion(ctx context.Context) (*daemonVersionState, error) {
	state, err := r.checkManagedDaemonBinary(ctx)
	if err != nil {
		return nil, err
	}
	if !state.Exists {
		return nil, nil
	}
	return &daemonVersionState{Version: state.Version, Source: "binary", BinaryPath: stringPtr(state.Path)}, nil
}

func (r *commandRuntime) readPathDaemonVersion(ctx context.Context) (*daemonVersionState, error) {
	return r.readDaemonVersion(ctx, looperdBinaryName)
}

func (r *commandRuntime) readDaemonVersion(ctx context.Context, command string) (*daemonVersionState, error) {
	version, err := r.runVersionCommand(ctx, command)
	if err != nil {
		return nil, err
	}
	if version == "" {
		return nil, nil
	}
	return &daemonVersionState{Version: version, Source: "binary", BinaryPath: stringPtr(command)}, nil
}

func (r *commandRuntime) runVersionCommand(ctx context.Context, command string) (string, error) {
	version, err := r.runVersionCommandStrict(ctx, command)
	if err != nil {
		return "", nil
	}
	return version, nil
}

func (r *commandRuntime) runVersionCommandStrict(ctx context.Context, command string) (string, error) {
	result, err := r.runCommand(ctx, command, []string{"--version"}, daemonCommandTimeout)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return "", errors.New(message)
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (r *commandRuntime) readPIDFile(path string) (int, bool) {
	raw, err := r.readFile(path)
	if err != nil {
		return 0, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return 0, false
	}
	pid, err := parsePositiveInt(trimmed, "pid")
	if err != nil {
		return 0, false
	}
	return int(pid), true
}

func (r *commandRuntime) removePIDFile(path string) {
	if err := r.removeFile(path); err != nil && !os.IsNotExist(err) {
		return
	}
}

func (r *commandRuntime) resolveDaemonPIDFilePath() (string, error) {
	homeDir, err := r.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".looper", "looperd.pid"), nil
}

func (r *commandRuntime) managedDaemonBinaryPath() (string, error) {
	homeDir, err := r.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".looper", "bin", looperdBinaryName), nil
}

func (r *commandRuntime) isProcessAlive(pid int) bool {
	return r.killProcess(pid, 0) == nil
}

func (r *commandRuntime) isLooperdProcess(ctx context.Context, pid int) (bool, error) {
	command, err := r.readProcessCommand(ctx, pid)
	if err != nil {
		return false, err
	}
	if command == "" {
		return false, nil
	}

	tokens := splitProcessCommand(command)
	if len(tokens) == 0 {
		return false, nil
	}

	executable := filepath.Base(tokens[0])
	if executable == looperdBinaryName {
		return true, nil
	}
	if executable != "node" {
		return false, nil
	}
	if len(tokens) < 2 {
		return false, nil
	}
	return filepath.Base(tokens[1]) == looperdBinaryName, nil
}

func splitProcessCommand(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}

	tokens := make([]string, 0)
	var current strings.Builder
	var quote rune
	escaped := false

	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}

	for _, r := range command {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}

		switch {
		case r == '\\' && quote != 0:
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			current.WriteRune(r)
		}
	}

	flush()
	return tokens
}

func (r *commandRuntime) readProcessCommand(ctx context.Context, pid int) (string, error) {
	result, err := r.runCommand(ctx, "ps", []string{"-p", fmt.Sprintf("%d", pid), "-o", "command="}, daemonCommandTimeout)
	if err != nil {
		return "", fmt.Errorf("inspect process %d with ps: %w", pid, err)
	}
	if result.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (r *commandRuntime) waitForProcessExit(pid int, timeout time.Duration, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !r.isProcessAlive(pid) {
			return nil
		}
		r.sleep(interval)
	}
	return fmt.Errorf("Timed out waiting for looperd pid %d to exit", pid)
}

func (r *commandRuntime) runCommand(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
	if r.app.deps.RunCommand != nil {
		return r.app.deps.RunCommand(ctx, command, args, timeout)
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, command, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	result := commandExecutionResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	if runCtx.Err() != nil {
		return result, runCtx.Err()
	}
	return result, err
}

func (r *commandRuntime) spawnDetached(command string, args []string, cwd string, env []string) (int, error) {
	if r.app.deps.SpawnDetached != nil {
		return r.app.deps.SpawnDetached(command, args, cwd, env)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer devNull.Close()

	startupLog, err := os.OpenFile(r.startupOutputPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer startupLog.Close()

	cmd := exec.Command(command, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = devNull
	cmd.Stdout = startupLog
	cmd.Stderr = startupLog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return 0, err
	}
	return pid, nil
}

func (r *commandRuntime) killProcess(pid int, signal int) error {
	if r.app.deps.KillProcess != nil {
		return r.app.deps.KillProcess(pid, signal)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.Signal(signal))
}

func (r *commandRuntime) readFile(path string) ([]byte, error) {
	if r.app.deps.ReadFile != nil {
		return r.app.deps.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (r *commandRuntime) writeFile(path string, data []byte, perm os.FileMode) error {
	if r.app.deps.WriteFile != nil {
		return r.app.deps.WriteFile(path, data, perm)
	}
	return os.WriteFile(path, data, perm)
}

func (r *commandRuntime) removeFile(path string) error {
	if r.app.deps.RemoveFile != nil {
		return r.app.deps.RemoveFile(path)
	}
	return os.Remove(path)
}

func (r *commandRuntime) mkdirAll(path string, perm os.FileMode) error {
	if r.app.deps.MkdirAll != nil {
		return r.app.deps.MkdirAll(path, perm)
	}
	return os.MkdirAll(path, perm)
}

func (r *commandRuntime) sleep(duration time.Duration) {
	if r.app.deps.Sleep != nil {
		r.app.deps.Sleep(duration)
		return
	}
	time.Sleep(duration)
}

func (r *commandRuntime) getwd() (string, error) {
	if r.app.deps.Getwd != nil {
		return r.app.deps.Getwd()
	}
	return os.Getwd()
}

func tailLines(content string, count int) []string {
	if count <= 0 {
		return []string{}
	}
	if content == "" {
		return []string{}
	}
	lines := strings.Split(content, "\n")
	if count >= len(lines) {
		return lines
	}
	return lines[len(lines)-count:]
}

func writeHumanDaemonStatus(w io.Writer, payload daemonStatusOutput) error {
	entries := [][2]any{{"configuredMode", payload.ConfiguredMode}, {"runningMode", payload.RunningMode}, {"restartPolicy", payload.RestartPolicy}, {"restartThrottleSeconds", payload.RestartThrottleSeconds}, {"configPath", payload.ConfigPath}, {"logDir", payload.LogDir}, {"apiReachable", payload.APIReachable}, {"daemonVersion", payload.DaemonVersion}, {"daemonVersionSource", payload.DaemonVersionSource}, {"daemonBinaryPath", payload.DaemonBinaryPath}}
	if payload.ConfiguredMode == config.DaemonModeForeground {
		entries = append(entries, [2]any{"restartBehavior", "not supervised; detached mode does not restart after crashes or reboot"})
	}
	printSection(w, "Daemon", entries)
	if payload.Lifecycle.StatePath != "" {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		lifecycleEntries := [][2]any{{"process", payload.Lifecycle.Process}, {"statePath", payload.Lifecycle.StatePath}, {"pidPath", payload.Lifecycle.PIDPath}, {"staleState", payload.Lifecycle.StaleState}}
		if state := payload.Lifecycle.State; state != nil {
			lifecycleEntries = append(lifecycleEntries, [2]any{"pid", state.PID}, [2]any{"startedAt", state.StartedAt}, [2]any{"binaryPath", state.BinaryPath})
			if state.Supervisor != nil {
				lifecycleEntries = append(lifecycleEntries, [2]any{"supervisor", state.Supervisor.Source}, [2]any{"supervisorLabel", state.Supervisor.Label}, [2]any{"plistPath", state.Supervisor.PlistPath})
			}
			lifecycleEntries = append(lifecycleEntries, [2]any{"logMain", state.Logs.Main}, [2]any{"logStartupDir", state.Logs.StartupDir}, [2]any{"logStdout", state.Logs.Stdout}, [2]any{"logStderr", state.Logs.Stderr})
			if state.LastExit != nil {
				lifecycleEntries = append(lifecycleEntries, [2]any{"lastExitAt", state.LastExit.At}, [2]any{"lastExitReason", state.LastExit.Reason}, [2]any{"lastExitLog", state.LastExit.LogPath})
			}
			if state.LastError != "" {
				lifecycleEntries = append(lifecycleEntries, [2]any{"lastError", state.LastError})
			}
		}
		printSection(w, "Lifecycle", lifecycleEntries)
	}

	if !payload.APIReachable {
		return nil
	}

	selected := payload.Status
	if len(selected) == 0 {
		selected = payload.Health
	}
	if len(selected) == 0 {
		return nil
	}

	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return writeJSON(w, selected)
}

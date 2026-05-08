package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

const (
	daemonStateSchemaVersion = 1
	launchdLooperdLabel      = "com.nexu-io.looper.looperd"
)

type daemonLifecycleState struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Mode          config.DaemonMode         `json:"mode"`
	PID           int                       `json:"pid,omitempty"`
	StartedAt     *time.Time                `json:"startedAt,omitempty"`
	BinaryPath    string                    `json:"binaryPath,omitempty"`
	Supervisor    *daemonSupervisorState    `json:"supervisor,omitempty"`
	Logs          daemonLifecycleLogState   `json:"logs"`
	LastExit      *daemonLifecycleExitState `json:"lastExit,omitempty"`
	LastError     string                    `json:"lastError,omitempty"`
}

type daemonSupervisorState struct {
	Source                 string                     `json:"source"`
	Label                  string                     `json:"label,omitempty"`
	PlistPath              string                     `json:"plistPath,omitempty"`
	RestartPolicy          config.DaemonRestartPolicy `json:"restartPolicy"`
	RestartThrottleSeconds int                        `json:"restartThrottleSeconds"`
}

type daemonLifecycleLogState struct {
	Main       string `json:"main"`
	StartupDir string `json:"startupDir"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
}

type daemonLifecycleExitState struct {
	At       time.Time `json:"at"`
	ExitCode *int      `json:"exitCode,omitempty"`
	Signal   *string   `json:"signal,omitempty"`
	Reason   string    `json:"reason"`
	LogPath  string    `json:"logPath,omitempty"`
}

type daemonLifecycleStatus struct {
	State      *daemonLifecycleState `json:"state,omitempty"`
	StatePath  string                `json:"statePath"`
	PIDPath    string                `json:"pidPath"`
	Process    string                `json:"process"`
	StaleState bool                  `json:"staleState"`
}

type launchdPlistConfig struct {
	Label                  string
	ProgramArguments       []string
	WorkingDirectory       string
	Environment            map[string]string
	RunAtLoad              bool
	RestartPolicy          config.DaemonRestartPolicy
	RestartThrottleSeconds int
	StandardOutPath        string
	StandardErrorPath      string
}

func (r *commandRuntime) resolveDaemonStatePath() (string, error) {
	homeDir, err := r.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".looper", "looperd.state.json"), nil
}

func (r *commandRuntime) resolveLaunchdPlistPath(loaded config.LoadedFileConfig) (string, error) {
	if loaded.Config.Daemon.PlistPath != nil && strings.TrimSpace(*loaded.Config.Daemon.PlistPath) != "" {
		return r.resolveStablePath(*loaded.Config.Daemon.PlistPath)
	}
	homeDir, err := r.homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, "Library", "LaunchAgents", launchdLooperdLabel+".plist"), nil
}

func (r *commandRuntime) resolveStablePath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", nil
	}
	if filepath.IsAbs(trimmed) {
		return filepath.Clean(trimmed), nil
	}
	cwd, err := r.getwd()
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(cwd, trimmed)), nil
}

func (r *commandRuntime) resolveLaunchdPlistPathForState(loaded config.LoadedFileConfig, state *daemonLifecycleState) (string, error) {
	if state != nil && state.Supervisor != nil && strings.TrimSpace(state.Supervisor.PlistPath) != "" {
		return r.resolveStablePath(state.Supervisor.PlistPath)
	}
	return r.resolveLaunchdPlistPath(loaded)
}

func daemonLogState(loaded config.LoadedFileConfig) daemonLifecycleLogState {
	logDir := loaded.Config.Daemon.LogDir
	return daemonLifecycleLogState{
		Main:       filepath.Join(logDir, "looperd.log"),
		StartupDir: filepath.Join(logDir, "startup"),
		Stdout:     filepath.Join(logDir, "launchd", "looperd.stdout.log"),
		Stderr:     filepath.Join(logDir, "launchd", "looperd.stderr.log"),
	}
}

func (r *commandRuntime) readDaemonLifecycleState(path string) (*daemonLifecycleState, error) {
	raw, err := r.readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state daemonLifecycleState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("read daemon lifecycle state: %w", err)
	}
	if state.SchemaVersion != daemonStateSchemaVersion {
		return nil, fmt.Errorf("unsupported daemon lifecycle state schemaVersion %d at %s", state.SchemaVersion, path)
	}
	return &state, nil
}

func (r *commandRuntime) writeDaemonLifecycleState(path string, state daemonLifecycleState) error {
	state.SchemaVersion = daemonStateSchemaVersion
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := r.mkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if r.app.deps.WriteFile != nil {
		return r.writeFile(path, raw, 0o644)
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (r *commandRuntime) daemonLifecycleStatus(ctx context.Context, loaded config.LoadedFileConfig) (daemonLifecycleStatus, error) {
	statePath, err := r.resolveDaemonStatePath()
	if err != nil {
		return daemonLifecycleStatus{}, err
	}
	pidPath, err := r.resolveDaemonPIDFilePath()
	if err != nil {
		return daemonLifecycleStatus{}, err
	}
	state, err := r.readDaemonLifecycleState(statePath)
	if err != nil {
		return daemonLifecycleStatus{}, err
	}
	status := daemonLifecycleStatus{State: state, StatePath: statePath, PIDPath: pidPath, Process: "unknown"}
	if state != nil && state.Mode == config.DaemonModeLaunchd {
		if refreshed, err := r.refreshLaunchdLifecycleState(ctx, loaded, statePath, state); err != nil {
			state.LastError = err.Error()
		} else if refreshed != nil {
			state = refreshed
			status.State = refreshed
		}
	}
	pid := 0
	if state != nil {
		pid = state.PID
	}
	if pid == 0 {
		if filePID, ok := r.readPIDFile(pidPath); ok {
			pid = filePID
		}
	}
	if pid == 0 {
		status.Process = "not-running"
		return status, nil
	}
	if r.isProcessAlive(pid) {
		isLooperd, err := r.isLooperdProcess(ctx, pid)
		if err != nil {
			return daemonLifecycleStatus{}, err
		}
		if !isLooperd {
			status.Process = "not-looperd"
			status.StaleState = true
			return status, nil
		}
		status.Process = "running"
		return status, nil
	}
	status.Process = "exited"
	status.StaleState = true
	if state != nil && state.LastExit == nil {
		now := time.Now().UTC()
		state.LastExit = &daemonLifecycleExitState{At: now, Reason: fmt.Sprintf("process %d is no longer running; exit code unavailable (possibly SIGKILL, OOM, reboot, or supervisor stop)", pid), LogPath: state.Logs.Main}
		state.LastError = state.LastExit.Reason
		_ = r.writeDaemonLifecycleState(statePath, *state)
	}
	_ = loaded
	return status, nil
}

func (r *commandRuntime) refreshLaunchdLifecycleState(ctx context.Context, loaded config.LoadedFileConfig, statePath string, state *daemonLifecycleState) (*daemonLifecycleState, error) {
	if state == nil || r.platform() != "darwin" {
		return state, nil
	}
	launchctlPath, err := r.lookPath()("launchctl")
	if err != nil || strings.TrimSpace(launchctlPath) == "" {
		return state, fmt.Errorf("launchd lifecycle state exists, but launchctl is unavailable; install/repair macOS launchd tools to inspect supervised looperd")
	}
	label := launchdLooperdLabel
	if state.Supervisor != nil && strings.TrimSpace(state.Supervisor.Label) != "" {
		label = state.Supervisor.Label
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	pid := r.launchdPID(ctx, launchctlPath, domain, label)
	if pid <= 0 {
		if state.PID == 0 {
			return state, nil
		}
		updated := *state
		updated.PID = 0
		now := time.Now().UTC()
		updated.LastExit = &daemonLifecycleExitState{At: now, Reason: fmt.Sprintf("launchd service %s is not reporting a pid; clearing stale pid %d", label, state.PID), LogPath: state.Logs.Main}
		updated.LastError = updated.LastExit.Reason
		if updated.Logs.Main == "" {
			updated.Logs = daemonLogState(loaded)
		}
		if err := r.writeDaemonLifecycleState(statePath, updated); err != nil {
			return state, err
		}
		pidPath, err := r.resolveDaemonPIDFilePath()
		if err == nil {
			r.removePIDFile(pidPath)
		}
		return &updated, nil
	}
	if pid == state.PID {
		return state, nil
	}
	updated := *state
	updated.PID = pid
	updated.LastError = ""
	if updated.Logs.Main == "" {
		updated.Logs = daemonLogState(loaded)
	}
	if err := r.writeDaemonLifecycleState(statePath, updated); err != nil {
		return state, err
	}
	pidPath, err := r.resolveDaemonPIDFilePath()
	if err == nil {
		_ = r.mkdirAll(filepath.Dir(pidPath), 0o755)
		_ = r.writeFile(pidPath, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
	}
	return &updated, nil
}

func (r *commandRuntime) startLaunchdDaemon(ctx context.Context, out io.Writer, loaded config.LoadedFileConfig, binary *resolvedDaemonBinary, args []string, cwd string, env []string, client *DaemonAPIClient, apiURL string) error {
	if r.platform() != "darwin" {
		return fmt.Errorf("daemon.mode=launchd is only supported on macOS. Use daemon.mode=foreground for detached-only mode on this platform, or configure a platform supervisor when support is added")
	}
	launchctlPath, err := r.lookPath()("launchctl")
	if err != nil || strings.TrimSpace(launchctlPath) == "" {
		return fmt.Errorf("daemon.mode=launchd requires launchctl. Install/repair macOS launchd tools, or set daemon.mode=foreground for detached-only start")
	}
	plistPath, err := r.resolveLaunchdPlistPath(loaded)
	if err != nil {
		return err
	}
	logs := daemonLogState(loaded)
	if err := r.mkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	if err := r.mkdirAll(filepath.Dir(logs.Stdout), 0o755); err != nil {
		return fmt.Errorf("create launchd log directory: %w", err)
	}
	programArgs := append([]string{binary.Path}, args...)
	launchEnv := launchdEnvironment(env)
	for key, value := range loaded.Config.Daemon.Environment {
		launchEnv[key] = value
	}
	plist, err := generateLaunchdPlist(launchdPlistConfig{Label: launchdLooperdLabel, ProgramArguments: programArgs, WorkingDirectory: cwd, Environment: launchEnv, RunAtLoad: true, RestartPolicy: loaded.Config.Daemon.RestartPolicy, RestartThrottleSeconds: loaded.Config.Daemon.RestartThrottleSeconds, StandardOutPath: logs.Stdout, StandardErrorPath: logs.Stderr})
	if err != nil {
		return err
	}
	if err := r.writeFile(plistPath, plist, 0o644); err != nil {
		return fmt.Errorf("write launchd plist: %w", err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_, _ = r.runCommand(ctx, launchctlPath, []string{"bootout", domain, plistPath}, daemonCommandTimeout)
	result, err := r.runCommand(ctx, launchctlPath, []string{"bootstrap", domain, plistPath}, daemonCommandTimeout)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("launchctl bootstrap failed: %s", strings.TrimSpace(result.Stderr))
	}
	if err := r.waitForDaemonReadyByAPI(ctx, client, apiURL, r.daemonStartReadyTimeout(), daemonStartReadyPollPeriod); err != nil {
		_, _ = r.runCommand(ctx, launchctlPath, []string{"bootout", domain, plistPath}, daemonCommandTimeout)
		_ = r.removeFile(plistPath)
		statePath, stateErr := r.resolveDaemonStatePath()
		if stateErr == nil {
			now := time.Now().UTC()
			state := daemonLifecycleState{SchemaVersion: daemonStateSchemaVersion, Mode: config.DaemonModeLaunchd, StartedAt: &now, BinaryPath: binary.Path, Logs: logs, LastError: fmt.Sprintf("launchd daemon did not become ready at %s: %v", apiURL, err), Supervisor: &daemonSupervisorState{Source: "launchd", Label: launchdLooperdLabel, PlistPath: plistPath, RestartPolicy: loaded.Config.Daemon.RestartPolicy, RestartThrottleSeconds: loaded.Config.Daemon.RestartThrottleSeconds}}
			_ = r.writeDaemonLifecycleState(statePath, state)
		}
		return fmt.Errorf("launchd daemon did not become ready at %s: %w; stdout log: %s; stderr log: %s", apiURL, err, logs.Stdout, logs.Stderr)
	}
	pid := r.launchdPID(ctx, launchctlPath, domain, launchdLooperdLabel)
	now := time.Now().UTC()
	statePath, err := r.resolveDaemonStatePath()
	if err != nil {
		return err
	}
	state := daemonLifecycleState{SchemaVersion: daemonStateSchemaVersion, Mode: config.DaemonModeLaunchd, PID: pid, StartedAt: &now, BinaryPath: binary.Path, Logs: logs, Supervisor: &daemonSupervisorState{Source: "launchd", Label: launchdLooperdLabel, PlistPath: plistPath, RestartPolicy: loaded.Config.Daemon.RestartPolicy, RestartThrottleSeconds: loaded.Config.Daemon.RestartThrottleSeconds}}
	if err := r.writeDaemonLifecycleState(statePath, state); err != nil {
		return fmt.Errorf("write daemon lifecycle state: %w", err)
	}
	if pid > 0 {
		pidPath, _ := r.resolveDaemonPIDFilePath()
		_ = r.mkdirAll(filepath.Dir(pidPath), 0o755)
		_ = r.writeFile(pidPath, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
	}
	_, err = fmt.Fprintf(out, "Started looperd under launchd supervision (%s)\nSupervisor: launchd label %s\nRestart policy: %s (throttle %ds)\nLaunchAgent: %s\nLogs: %s, %s\nState: %s\n", binary.Path, launchdLooperdLabel, loaded.Config.Daemon.RestartPolicy, loaded.Config.Daemon.RestartThrottleSeconds, plistPath, logs.Stdout, logs.Stderr, statePath)
	return err
}

func (r *commandRuntime) stopLaunchdDaemon(ctx context.Context, out io.Writer, loaded config.LoadedFileConfig, state *daemonLifecycleState, startIfMissing bool) (bool, error) {
	if r.platform() != "darwin" {
		return false, fmt.Errorf("daemon.mode=launchd is only supported on macOS; cannot stop launchd supervision on this platform")
	}
	launchctlPath, err := r.lookPath()("launchctl")
	if err != nil || strings.TrimSpace(launchctlPath) == "" {
		return false, fmt.Errorf("daemon.mode=launchd requires launchctl to stop supervised looperd")
	}
	plistPath, err := r.resolveLaunchdPlistPathForState(loaded, state)
	if err != nil {
		return false, err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	result, err := r.runCommand(ctx, launchctlPath, []string{"bootout", domain, plistPath}, daemonCommandTimeout)
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 && !isLaunchdNotLoadedError(result.Stderr) {
		return false, fmt.Errorf("launchctl bootout failed: %s", strings.TrimSpace(result.Stderr))
	}
	statePath, _ := r.resolveDaemonStatePath()
	if state == nil {
		state, _ = r.readDaemonLifecycleState(statePath)
	}
	if state != nil {
		now := time.Now().UTC()
		state.LastExit = &daemonLifecycleExitState{At: now, Reason: "stopped by looper daemon stop", LogPath: state.Logs.Main}
		state.PID = 0
		_ = r.writeDaemonLifecycleState(statePath, *state)
	}
	pidPath, _ := r.resolveDaemonPIDFilePath()
	r.removePIDFile(pidPath)
	_ = r.removeFile(plistPath)
	if startIfMissing {
		_, err = fmt.Fprintln(out, "Stopped launchd-supervised looperd; starting daemon.")
	} else {
		_, err = fmt.Fprintf(out, "Stopped launchd-supervised looperd (%s)\n", launchdLooperdLabel)
	}
	return true, err
}

func isLaunchdNotLoadedError(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "service is not loaded") || strings.Contains(lower, "could not find") || strings.Contains(lower, "no such process") || strings.Contains(lower, "does not exist") || strings.Contains(lower, "not found")
}

func (r *commandRuntime) waitForDaemonReadyByAPI(ctx context.Context, client *DaemonAPIClient, apiURL string, timeout time.Duration, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastProbeErr error
	for time.Now().Before(deadline) {
		payload, err := r.getJSONWithClient(ctx, client, "/api/v1/status")
		if err != nil {
			lastProbeErr = err
		} else if isLooperdStatusPayload(payload) {
			return nil
		} else {
			lastProbeErr = fmt.Errorf("/api/v1/status at %s did not identify looperd", apiURL)
		}
		r.sleep(interval)
	}
	message := fmt.Sprintf("Timed out waiting for launchd-supervised looperd to become ready at %s", apiURL)
	if lastProbeErr != nil {
		message += fmt.Sprintf("; last readiness error: %v", lastProbeErr)
	}
	return fmt.Errorf(message)
}

func (r *commandRuntime) launchdPID(ctx context.Context, launchctlPath string, domain string, label string) int {
	result, err := r.runCommand(ctx, launchctlPath, []string{"print", domain + "/" + label}, daemonCommandTimeout)
	if err != nil || result.ExitCode != 0 {
		return 0
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid =") {
			pid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid =")))
			return pid
		}
	}
	return 0
}

func generateLaunchdPlist(cfg launchdPlistConfig) ([]byte, error) {
	if strings.TrimSpace(cfg.Label) == "" {
		return nil, fmt.Errorf("launchd label is required")
	}
	if len(cfg.ProgramArguments) == 0 || strings.TrimSpace(cfg.ProgramArguments[0]) == "" {
		return nil, fmt.Errorf("launchd ProgramArguments are required")
	}
	if cfg.RestartThrottleSeconds < 1 {
		return nil, fmt.Errorf("launchd ThrottleInterval must be positive")
	}
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writePlistString(&b, "Label", cfg.Label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range cfg.ProgramArguments {
		b.WriteString("\t\t<string>" + html.EscapeString(arg) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	writePlistString(&b, "WorkingDirectory", cfg.WorkingDirectory)
	if len(cfg.Environment) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		keys := make([]string, 0, len(cfg.Environment))
		for key := range cfg.Environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writePlistString(&b, key, cfg.Environment[key])
		}
		b.WriteString("\t</dict>\n")
	}
	writePlistBool(&b, "RunAtLoad", cfg.RunAtLoad)
	switch cfg.RestartPolicy {
	case config.DaemonRestartNever:
	case config.DaemonRestartOnFailure:
		b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	case config.DaemonRestartAlways:
		writePlistBool(&b, "KeepAlive", true)
	default:
		return nil, fmt.Errorf("unsupported restart policy %q", cfg.RestartPolicy)
	}
	b.WriteString(fmt.Sprintf("\t<key>ThrottleInterval</key>\n\t<integer>%d</integer>\n", cfg.RestartThrottleSeconds))
	writePlistString(&b, "StandardOutPath", cfg.StandardOutPath)
	writePlistString(&b, "StandardErrorPath", cfg.StandardErrorPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func writePlistString(b *bytes.Buffer, key string, value string) {
	b.WriteString("\t<key>" + html.EscapeString(key) + "</key>\n\t<string>" + html.EscapeString(value) + "</string>\n")
}

func writePlistBool(b *bytes.Buffer, key string, value bool) {
	b.WriteString("\t<key>" + html.EscapeString(key) + "</key>\n")
	if value {
		b.WriteString("\t<true/>\n")
	} else {
		b.WriteString("\t<false/>\n")
	}
}

func launchdEnvironment(env []string) map[string]string {
	allowed := map[string]struct{}{
		"HOME":          {},
		"PATH":          {},
		"SHELL":         {},
		"TMPDIR":        {},
		"LOOPER_CONFIG": {},
	}
	for name := range daemonSpawnPathEnvNames {
		allowed[name] = struct{}{}
	}
	result := make(map[string]string, len(allowed))
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		if _, keep := allowed[name]; keep || strings.HasPrefix(name, "LOOPER_") {
			result[name] = value
		}
	}
	return result
}

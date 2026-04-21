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
	"strings"
	"syscall"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/spf13/cobra"
)

const daemonCommandTimeout = 5 * time.Second

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
	Mode                config.DaemonMode `json:"mode"`
	ConfigPath          string            `json:"configPath"`
	LogDir              string            `json:"logDir"`
	APIReachable        bool              `json:"apiReachable"`
	DaemonVersion       *string           `json:"daemonVersion"`
	DaemonVersionSource *string           `json:"daemonVersionSource"`
	DaemonBinaryPath    *string           `json:"daemonBinaryPath"`
	Status              json.RawMessage   `json:"status"`
	Health              json.RawMessage   `json:"health"`
}

type daemonLogsOutput struct {
	LogPath string   `json:"logPath"`
	Lines   []string `json:"lines"`
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

	output := daemonStatusOutput{
		Mode:         loaded.Config.Daemon.Mode,
		ConfigPath:   loaded.Metadata.ConfigPath,
		LogDir:       loaded.Config.Daemon.LogDir,
		APIReachable: apiReachable,
		Status:       statusPayload,
		Health:       healthPayload,
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
	binary, err := r.resolveDaemonBinary(ctx)
	if err != nil {
		return err
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
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "looperd already appears to be running (pid %d)\n", existingPID); err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Phase 1 process management is minimal: use `looper daemon restart` or stop the process manually if needed.")
				return err
			}

			r.removePIDFile(pidFilePath)
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Daemon pid %d does not appear to be looperd; treating pid file as stale.\n", existingPID); err != nil {
				return err
			}
		} else {
			r.removePIDFile(pidFilePath)
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed stale daemon pid file for pid %d\n", existingPID); err != nil {
				return err
			}
		}
	}

	cwd, err := r.getwd()
	if err != nil {
		return fmt.Errorf("determine current working directory: %w", err)
	}

	pid, err := r.spawnDetached(binary.Path, ExtractConfigArgs(r.argv), cwd, os.Environ())
	if err != nil {
		return fmt.Errorf("Failed to start looperd: %w", err)
	}
	if pid <= 0 {
		return fmt.Errorf("Failed to start looperd: process did not report a pid")
	}

	r.sleep(100 * time.Millisecond)
	if !r.isProcessAlive(pid) {
		r.removePIDFile(pidFilePath)
		return fmt.Errorf("Failed to start looperd: process %d exited during startup", pid)
	}

	isLooperd, err := r.isLooperdProcess(ctx, pid)
	if err != nil {
		return err
	}
	if !isLooperd {
		r.removePIDFile(pidFilePath)
		return fmt.Errorf("Failed to start looperd: process %d exited during startup", pid)
	}

	if err := r.mkdirAll(filepath.Dir(pidFilePath), 0o755); err != nil {
		return fmt.Errorf("create daemon pid directory: %w", err)
	}
	if err := r.writeFile(pidFilePath, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		return fmt.Errorf("write daemon pid file: %w", err)
	}

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Started looperd (%s) with pid %d\n", binary.Path, pid); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "PID file: %s\n", pidFilePath); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Phase 1 process management is minimal and does not provide full background supervision.")
	return err
}

func (r *commandRuntime) daemonRestart(cmd *cobra.Command, args []string) error {
	_ = args

	pidFilePath, err := r.resolveDaemonPIDFilePath()
	if err != nil {
		return err
	}

	existingPID, ok := r.readPIDFile(pidFilePath)
	if !ok {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "No daemon pid file found; starting daemon."); err != nil {
			return err
		}
		return r.daemonStart(cmd, nil)
	}

	if !r.isProcessAlive(existingPID) {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Daemon pid %d is stale; starting daemon.\n", existingPID); err != nil {
			return err
		}
		r.removePIDFile(pidFilePath)
		return r.daemonStart(cmd, nil)
	}

	isLooperd, err := r.isLooperdProcess(cmd.Context(), existingPID)
	if err != nil {
		return err
	}
	if !isLooperd {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Daemon pid %d does not appear to be looperd; treating pid file as stale.\n", existingPID); err != nil {
			return err
		}
		r.removePIDFile(pidFilePath)
		return r.daemonStart(cmd, nil)
	}

	if err := r.killProcess(existingPID, int(syscall.SIGTERM)); err != nil {
		return fmt.Errorf("stop looperd pid %d: %w", existingPID, err)
	}
	if err := r.waitForProcessExit(existingPID, 2*time.Second, 100*time.Millisecond); err != nil {
		return err
	}
	r.removePIDFile(pidFilePath)
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Stopped looperd pid %d\n", existingPID); err != nil {
		return err
	}

	return r.daemonStart(cmd, nil)
}

func (r *commandRuntime) daemonLogs(cmd *cobra.Command, args []string) error {
	_ = args

	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}

	lineCountValue := strings.TrimSpace(getStringFlag(cmd, "lines"))
	lineCount := int64(50)
	if lineCountValue != "" {
		lineCount, err = parsePositiveInt(lineCountValue, "--lines")
		if err != nil {
			return err
		}
	}

	logPath := filepath.Join(loaded.Config.Daemon.LogDir, "looperd.log")
	raw, err := r.readFile(logPath)
	if err != nil {
		return err
	}

	lines := tailLines(strings.TrimRight(string(raw), "\n"), int(lineCount))
	output := daemonLogsOutput{LogPath: logPath, Lines: lines}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}

	if _, err := fmt.Fprintln(cmd.OutOrStdout(), logPath); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	return nil
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

	token := ""
	if loaded.Config.Server.AuthMode == config.AuthModeLocalToken && loaded.Config.Server.LocalToken != nil {
		token = strings.TrimSpace(*loaded.Config.Server.LocalToken)
	}

	return NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: baseURL, Token: token, HTTPClient: r.httpClient()})
}

func (r *commandRuntime) detectDaemonVersionState(ctx context.Context, statusPayload json.RawMessage) (*daemonVersionState, error) {
	if version := extractDaemonVersion(statusPayload); version != "" {
		return &daemonVersionState{Version: version, Source: "api"}, nil
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

func extractDaemonVersion(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}

	var decoded struct {
		Service struct {
			Version string `json:"version"`
		} `json:"service"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	return strings.TrimSpace(decoded.Service.Version)
}

type resolvedDaemonBinary struct {
	Path   string
	Source string
}

func (r *commandRuntime) resolveDaemonBinary(ctx context.Context) (*resolvedDaemonBinary, error) {
	managedPath, err := r.managedDaemonBinaryPath()
	if err != nil {
		return nil, err
	}

	candidates := []resolvedDaemonBinary{{Path: managedPath, Source: "installed"}, {Path: looperdBinaryName, Source: "path"}}
	for _, candidate := range candidates {
		version, err := r.runVersionCommand(ctx, candidate.Path)
		if err != nil {
			return nil, err
		}
		if version != "" {
			resolved := candidate
			return &resolved, nil
		}
	}

	return nil, fmt.Errorf("Cannot find looperd binary. Lookup order: ~/.looper/bin/looperd, then $PATH.")
}

func (r *commandRuntime) readManagedDaemonVersion(ctx context.Context) (*daemonVersionState, error) {
	binaryPath, err := r.managedDaemonBinaryPath()
	if err != nil {
		return nil, err
	}
	return r.readDaemonVersion(ctx, binaryPath)
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
	result, err := r.runCommand(ctx, command, []string{"--version"}, daemonCommandTimeout)
	if err != nil {
		return "", nil
	}
	if result.ExitCode != 0 {
		return "", nil
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

	cmd := exec.Command(command, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
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
	entries := [][2]any{{"mode", payload.Mode}, {"configPath", payload.ConfigPath}, {"logDir", payload.LogDir}, {"apiReachable", payload.APIReachable}, {"daemonVersion", payload.DaemonVersion}, {"daemonVersionSource", payload.DaemonVersionSource}, {"daemonBinaryPath", payload.DaemonBinaryPath}}
	printSection(w, "Daemon", entries)

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

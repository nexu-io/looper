package cliapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/version"
)

func allowVersionStatusProbeOnly(t *testing.T) *http.Client {
	t.Helper()
	return newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/v1/status" || req.URL.Path == "/api/v1/version" {
			return nil, os.ErrNotExist
		}
		t.Fatalf("unexpected auto-upgrade request to %q", req.URL.String())
		return nil, nil
	})
}

func missingDaemonVersionCommand(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
	_ = ctx
	_ = command
	_ = args
	_ = timeout
	return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
}

func TestResolveLooperTarget(t *testing.T) {
	t.Parallel()

	target, err := resolveLooperTarget("darwin", "arm64")
	if err != nil {
		t.Fatalf("resolveLooperTarget(darwin, arm64) error = %v", err)
	}
	if target != "darwin-arm64" {
		t.Fatalf("resolveLooperTarget(darwin, arm64) = %q, want %q", target, "darwin-arm64")
	}

	target, err = resolveLooperTarget("linux", "amd64")
	if err != nil {
		t.Fatalf("resolveLooperTarget(linux, amd64) error = %v", err)
	}
	if target != "linux-amd64" {
		t.Fatalf("resolveLooperTarget(linux, amd64) = %q, want %q", target, "linux-amd64")
	}

	for _, arch := range []string{"amd64", "x64"} {
		_, err = resolveLooperTarget("darwin", arch)
		if err == nil {
			t.Fatalf("resolveLooperTarget(darwin, %s) error = nil, want unsupported", arch)
		}
	}
}

func TestShouldRunAutoUpgradeCheck(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-autoUpgradeCheckInterval + time.Minute)
	old := now.Add(-autoUpgradeCheckInterval - time.Minute)

	for _, tc := range []struct {
		name  string
		state *autoUpgradeState
		want  bool
	}{
		{name: "nil state", state: nil, want: true},
		{name: "missing timestamp", state: &autoUpgradeState{}, want: true},
		{name: "recent timestamp", state: &autoUpgradeState{LastCheckedAt: &recent}, want: false},
		{name: "expired timestamp", state: &autoUpgradeState{LastCheckedAt: &old}, want: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldRunAutoUpgradeCheck(tc.state, now); got != tc.want {
				t.Fatalf("shouldRunAutoUpgradeCheck() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRootPreRunSkipsAutoUpgradeForUnsupportedInstallSource(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        t.TempDir(),
		ExecutablePath: "/opt/homebrew/Cellar/looper/0.2.1/bin/looper",
		CLIChannel:     cliInstallChannelStable,
		HTTPClient:     allowVersionStatusProbeOnly(t),
		RunCommand:     missingDaemonVersionCommand,
	})

	exitCode := app.Run(context.Background(), []string{"version"})
	if exitCode != 0 {
		t.Fatalf("Run([version]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([version]) stderr = %q, want empty string", stderr.String())
	}
	if got, want := stdout.String(), "CLI version: 0.0.0-dev\nlooperd server version: unavailable\n"; got != want {
		t.Fatalf("Run([version]) stdout = %q, want %q", got, want)
	}
}

func TestRootPreRunSkipsAutoUpgradeForBareRootHelp(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		CLIChannel:     cliInstallChannelStable,
		HTTPClient:     allowVersionStatusProbeOnly(t),
		RunCommand:     missingDaemonVersionCommand,
	})

	exitCode := app.Run(context.Background(), nil)
	if exitCode != 0 {
		t.Fatalf("Run([]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:\n  looper") {
		t.Fatalf("Run([]) stdout = %q, want root help output", stdout.String())
	}
}

func TestRootPreRunSkipsAutoUpgradeForHelpOnlySubcommand(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		CLIChannel:     cliInstallChannelStable,
		HTTPClient:     allowVersionStatusProbeOnly(t),
		RunCommand:     missingDaemonVersionCommand,
	})

	exitCode := app.Run(context.Background(), []string{"daemon"})
	if exitCode != 0 {
		t.Fatalf("Run([daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([daemon]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:\n  looper daemon") {
		t.Fatalf("Run([daemon]) stdout = %q, want daemon help output", stdout.String())
	}
}

func TestVersionNoAutoUpgradeFlagDisablesAutoUpgrade(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		CLIChannel:     cliInstallChannelStable,
		HTTPClient:     allowVersionStatusProbeOnly(t),
		RunCommand:     missingDaemonVersionCommand,
	})

	exitCode := app.Run(context.Background(), []string{"version", "--no-auto-upgrade", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([version --no-auto-upgrade]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([version --no-auto-upgrade]) stderr = %q, want empty string", stderr.String())
	}
}

func TestEnvDisablesAutoUpgrade(t *testing.T) {
	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	t.Setenv("LOOPER_AUTO_UPGRADE_ENABLED", "false")
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		CLIChannel:     cliInstallChannelStable,
		HTTPClient:     allowVersionStatusProbeOnly(t),
		RunCommand:     missingDaemonVersionCommand,
	})

	exitCode := app.Run(context.Background(), []string{"version", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([version --config]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([version --config]) stderr = %q, want empty string", stderr.String())
	}
}

func TestAutoUpgradeStartsBackgroundWorkerAndCachesInFlightState(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	statePath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.json")
	workerPID := os.Getpid()
	var spawnCalls int
	var spawnedArgs []string
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		CLIChannel:     cliInstallChannelStable,
		HTTPClient:     allowVersionStatusProbeOnly(t),
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = command
			_ = cwd
			_ = env
			spawnCalls++
			spawnedArgs = append([]string{}, args...)
			return workerPID, nil
		},
		Getwd: func() (string, error) {
			return homeDir, nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == "ps" {
				return commandExecutionResult{Stdout: filepath.Join(homeDir, ".looper", "bin", "looper") + " upgrade --background-auto\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	for i := 0; i < 2; i++ {
		stdout.Reset()
		stderr.Reset()
		exitCode := app.Run(context.Background(), []string{"version", "--config", configPath})
		if exitCode != 0 {
			t.Fatalf("run %d Run([version --config]) exit code = %d, want 0; stderr=%q", i+1, exitCode, stderr.String())
		}
	}
	if spawnCalls != 1 {
		t.Fatalf("spawnDetached calls = %d, want 1", spawnCalls)
	}
	if got, want := strings.Join(spawnedArgs, " "), "upgrade --background-auto --config "+configPath; got != want {
		t.Fatalf("spawned args = %q, want %q", got, want)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(auto-upgrade state) error = %v", err)
	}
	var state autoUpgradeState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("Unmarshal(auto-upgrade state) error = %v", err)
	}
	if state.InFlight == nil || state.InFlight.PID != workerPID {
		t.Fatalf("state.InFlight = %#v, want pid %d", state.InFlight, workerPID)
	}
	if state.LastCheckedAt != nil {
		t.Fatalf("state.LastCheckedAt = %v, want nil until background worker finishes", state.LastCheckedAt)
	}
}

func TestTryAcquireAutoUpgradeLockReturnsRemoveErrorForStaleLock(t *testing.T) {
	homeDir := t.TempDir()
	lockDir := filepath.Join(homeDir, ".looper")
	lockPath := filepath.Join(lockDir, "auto-upgrade.lock")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("invalid-pid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lockPath) error = %v", err)
	}
	staleTime := time.Now().Add(-autoUpgradeBusyRetryDelay - time.Second)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(lockPath) error = %v", err)
	}
	if err := os.Chmod(lockDir, 0o555); err != nil {
		t.Fatalf("Chmod(lockDir) error = %v", err)
	}
	defer func() {
		_ = os.Chmod(lockDir, 0o755)
	}()

	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir}), nil)
	unlock, acquired, err := runtime.tryAcquireAutoUpgradeLock(lockPath)
	if err == nil {
		if unlock != nil {
			unlock()
		}
		t.Fatal("tryAcquireAutoUpgradeLock() error = nil, want stale lock removal failure")
	}
	if acquired {
		t.Fatal("tryAcquireAutoUpgradeLock() acquired = true, want false")
	}
	if unlock != nil {
		t.Fatal("tryAcquireAutoUpgradeLock() unlock != nil, want nil")
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("Stat(lockPath) error = %v, want stale lock to remain", statErr)
	}
}

func TestTryAcquireAutoUpgradeLockBreaksLegacyStaleLockWhenPIDIsReused(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	lockPath := filepath.Join(homeDir, ".looper", "auto-upgrade.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("WriteFile(lockPath) error = %v", err)
	}
	staleTime := time.Now().Add(-autoUpgradeBusyRetryDelay - time.Second)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(lockPath) error = %v", err)
	}

	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir, ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper")}), nil)
	unlock, acquired, err := runtime.tryAcquireAutoUpgradeLock(lockPath)
	if err != nil {
		t.Fatalf("tryAcquireAutoUpgradeLock() error = %v", err)
	}
	if !acquired {
		t.Fatal("tryAcquireAutoUpgradeLock() acquired = false, want true")
	}
	defer unlock()

	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile(lockPath) error = %v", err)
	}
	lock, ok := parseAutoUpgradeLockState(raw)
	if !ok {
		t.Fatalf("parseAutoUpgradeLockState(lock file) = false, want true; raw=%q", string(raw))
	}
	if lock.PID != os.Getpid() {
		t.Fatalf("lock.PID = %d, want %d", lock.PID, os.Getpid())
	}
	if lock.Executable == "" {
		t.Fatal("lock.Executable = empty, want process metadata")
	}
	if lock.Command == "" {
		t.Fatal("lock.Command = empty, want process metadata")
	}
	if lock.ObservedAt == "" {
		t.Fatal("lock.ObservedAt = empty, want process metadata")
	}
}

func TestShouldBreakAutoUpgradeLockKeepsMatchingExecutableLockActive(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	lockPath := filepath.Join(homeDir, ".looper", "auto-upgrade.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	commandLine := "/usr/local/bin/looper upgrade --background-auto"
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(autoUpgradeLockState{PID: os.Getpid(), Executable: "looper", Command: commandLine, ObservedAt: observedAt})
	if err != nil {
		t.Fatalf("Marshal(lock state) error = %v", err)
	}
	if err := os.WriteFile(lockPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(lockPath) error = %v", err)
	}
	staleTime := time.Now().Add(-autoUpgradeBusyRetryDelay - time.Second)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(lockPath) error = %v", err)
	}

	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir, RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		_ = ctx
		_ = timeout
		if command != "ps" {
			t.Fatalf("RunCommand command = %q, want ps", command)
		}
		switch strings.Join(args, " ") {
		case fmt.Sprintf("-p %d -o command=", os.Getpid()):
			return commandExecutionResult{Stdout: commandLine + "\n", ExitCode: 0}, nil
		case fmt.Sprintf("-p %d -o etime=", os.Getpid()):
			return commandExecutionResult{Stdout: "0:00\n", ExitCode: 0}, nil
		default:
			t.Fatalf("RunCommand args = %q, want ps query for command or elapsed time", strings.Join(args, " "))
			return commandExecutionResult{}, nil
		}
	}}), nil)
	if runtime.shouldBreakAutoUpgradeLock(lockPath) {
		t.Fatal("shouldBreakAutoUpgradeLock() = true, want false")
	}
}

func TestShouldBreakAutoUpgradeLockKeepsForegroundUpgradeRunLockActive(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	lockPath := filepath.Join(homeDir, ".looper", "auto-upgrade.run.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	commandLine := "/usr/local/bin/looper upgrade"
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(autoUpgradeLockState{PID: os.Getpid(), Executable: "looper", Command: commandLine, ObservedAt: observedAt})
	if err != nil {
		t.Fatalf("Marshal(lock state) error = %v", err)
	}
	if err := os.WriteFile(lockPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(lockPath) error = %v", err)
	}

	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir, RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		_ = ctx
		_ = timeout
		if command != "ps" {
			t.Fatalf("RunCommand command = %q, want ps", command)
		}
		switch strings.Join(args, " ") {
		case fmt.Sprintf("-p %d -o command=", os.Getpid()):
			return commandExecutionResult{Stdout: commandLine + "\n", ExitCode: 0}, nil
		case fmt.Sprintf("-p %d -o etime=", os.Getpid()):
			return commandExecutionResult{Stdout: "0:00\n", ExitCode: 0}, nil
		default:
			t.Fatalf("RunCommand args = %q, want ps query for command or elapsed time", strings.Join(args, " "))
			return commandExecutionResult{}, nil
		}
	}}), nil)
	if runtime.shouldBreakAutoUpgradeLock(lockPath) {
		t.Fatal("shouldBreakAutoUpgradeLock() = true, want false")
	}
}

func TestShouldBreakAutoUpgradeLockKeepsStateLockForActiveLooperCommand(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	lockPath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	commandLine := "/usr/local/bin/looper version --config /tmp/config.json"
	observedAt := time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(autoUpgradeLockState{PID: os.Getpid(), Executable: "looper", Command: commandLine, ObservedAt: observedAt})
	if err != nil {
		t.Fatalf("Marshal(lock state) error = %v", err)
	}
	if err := os.WriteFile(lockPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(lockPath) error = %v", err)
	}

	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir, RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		_ = ctx
		_ = timeout
		if command != "ps" {
			t.Fatalf("RunCommand command = %q, want ps", command)
		}
		switch strings.Join(args, " ") {
		case fmt.Sprintf("-p %d -o command=", os.Getpid()):
			return commandExecutionResult{Stdout: commandLine + "\n", ExitCode: 0}, nil
		case fmt.Sprintf("-p %d -o etime=", os.Getpid()):
			return commandExecutionResult{Stdout: "0:00\n", ExitCode: 0}, nil
		default:
			t.Fatalf("RunCommand args = %q, want ps query for command or elapsed time", strings.Join(args, " "))
			return commandExecutionResult{}, nil
		}
	}}), nil)
	if runtime.shouldBreakAutoUpgradeLock(lockPath) {
		t.Fatal("shouldBreakAutoUpgradeLock() = true, want false")
	}
}

func TestShouldBreakAutoUpgradeLockBreaksWhenProcessStartTimeChanges(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	lockPath := filepath.Join(homeDir, ".looper", "auto-upgrade.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	raw, err := json.Marshal(autoUpgradeLockState{PID: os.Getpid(), Executable: "looper", Command: "/usr/local/bin/looper upgrade --background-auto", ObservedAt: time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatalf("Marshal(lock state) error = %v", err)
	}
	if err := os.WriteFile(lockPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(lockPath) error = %v", err)
	}

	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir, RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		_ = ctx
		_ = timeout
		if command != "ps" {
			t.Fatalf("RunCommand command = %q, want ps", command)
		}
		switch strings.Join(args, " ") {
		case fmt.Sprintf("-p %d -o command=", os.Getpid()):
			return commandExecutionResult{Stdout: "/usr/local/bin/looper upgrade --background-auto\n", ExitCode: 0}, nil
		case fmt.Sprintf("-p %d -o etime=", os.Getpid()):
			return commandExecutionResult{Stdout: "0:01\n", ExitCode: 0}, nil
		default:
			t.Fatalf("RunCommand args = %q, want ps query for command or elapsed time", strings.Join(args, " "))
			return commandExecutionResult{}, nil
		}
	}}), nil)
	if !runtime.shouldBreakAutoUpgradeLock(lockPath) {
		t.Fatal("shouldBreakAutoUpgradeLock() = false, want true")
	}
}

func TestShouldBreakAutoUpgradeLockBreaksLegacyPIDLockForDifferentProcess(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	lockPath := filepath.Join(homeDir, ".looper", "auto-upgrade.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("WriteFile(lockPath) error = %v", err)
	}

	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir, RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
		_ = ctx
		_ = timeout
		if command != "ps" {
			t.Fatalf("RunCommand command = %q, want ps", command)
		}
		switch strings.Join(args, " ") {
		case fmt.Sprintf("-p %d -o command=", os.Getpid()):
			return commandExecutionResult{Stdout: "/usr/bin/vim /tmp/note.txt\n", ExitCode: 0}, nil
		case fmt.Sprintf("-p %d -o etime=", os.Getpid()):
			return commandExecutionResult{Stdout: "0:10\n", ExitCode: 0}, nil
		default:
			t.Fatalf("RunCommand args = %q, want ps query for command or elapsed time", strings.Join(args, " "))
			return commandExecutionResult{}, nil
		}
	}}), nil)
	if !runtime.shouldBreakAutoUpgradeLock(lockPath) {
		t.Fatal("shouldBreakAutoUpgradeLock() = false, want true")
	}
}

func TestParsePSElapsedSeconds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw  string
		want int
	}{
		{raw: "0:07", want: 7},
		{raw: "12:34", want: 12*60 + 34},
		{raw: "1:02:03", want: 3723},
		{raw: "2-03:04:05", want: (((2*24)+3)*60+4)*60 + 5},
	} {
		got, err := parsePSElapsedSeconds(tc.raw)
		if err != nil {
			t.Fatalf("parsePSElapsedSeconds(%q) error = %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("parsePSElapsedSeconds(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestBackgroundAutoUpgradeCommandPersistsReadyRestartState(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	statePath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.json")
	execPath := filepath.Join(homeDir, ".looper", "bin", "looper")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(exec dir) error = %v", err)
	}
	if err := os.WriteFile(execPath, []byte("old looper"), 0o755); err != nil {
		t.Fatalf("WriteFile(execPath) error = %v", err)
	}
	if err := os.WriteFile(managedPath, []byte("old looperd"), 0o755); err != nil {
		t.Fatalf("WriteFile(managedPath) error = %v", err)
	}
	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir}), nil)
	if err := runtime.writeAutoUpgradeState(statePath, autoUpgradeState{InFlight: &autoUpgradeInFlightState{PID: 4321, StartedAt: timePtr(time.Now().UTC())}}); err != nil {
		t.Fatalf("writeAutoUpgradeState() error = %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cliBinary := []byte("new looper")
	cliChecksum := sha256.Sum256(cliBinary)
	daemonBinary := []byte("new looperd")
	daemonChecksum := sha256.Sum256(daemonBinary)
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: execPath,
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return jsonResponse(t, http.StatusOK, fmt.Sprintf(`{"ok":true,"requestId":"req_status","data":{"service":{"version":"0.2.1","binary":{"name":"looperd","path":%q}}}}`, managedPath)), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/latest",
				"https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looper-darwin-arm64","browser_download_url":"https://example.invalid/looper-darwin-arm64"},{"name":"looper-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looper-darwin-arm64.sha256"},{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looper-darwin-arm64":
				return binaryResponse(t, http.StatusOK, cliBinary), nil
			case "https://example.invalid/looper-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(cliChecksum[:])+"  looper-darwin-arm64\n"), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, daemonBinary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(daemonChecksum[:])+"  looperd-darwin-arm64\n"), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	if exitCode := app.Run(context.Background(), []string{"upgrade", "--background-auto", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([upgrade --background-auto]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(auto-upgrade state) error = %v", err)
	}
	var state autoUpgradeState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("Unmarshal(auto-upgrade state) error = %v", err)
	}
	if state.InFlight != nil {
		t.Fatalf("state.InFlight = %#v, want nil", state.InFlight)
	}
	if state.LastCheckedAt == nil {
		t.Fatal("state.LastCheckedAt = nil, want timestamp")
	}
	if state.Ready == nil {
		t.Fatal("state.Ready = nil, want ready notification")
	}
	if !state.Ready.CLIChanged || state.Ready.CLIVersion != "0.3.0" {
		t.Fatalf("state.Ready CLI = %#v, want changed version 0.3.0", state.Ready)
	}
	if !state.Ready.DaemonChanged || state.Ready.DaemonVersion != "0.3.0" {
		t.Fatalf("state.Ready daemon = %#v, want changed version 0.3.0", state.Ready)
	}
	if !state.Ready.DaemonRestartRequired || state.Ready.RunningDaemonVersion != "0.2.1" {
		t.Fatalf("state.Ready restart = %#v, want restart from 0.2.1", state.Ready)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty string", stdout.String())
	}
}

func TestBackgroundAutoUpgradeCommandSkipsDaemonInstallWithoutManagedBinary(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	statePath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.json")
	execPath := filepath.Join(homeDir, ".looper", "bin", "looper")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(exec dir) error = %v", err)
	}
	if err := os.WriteFile(execPath, []byte("old looper"), 0o755); err != nil {
		t.Fatalf("WriteFile(execPath) error = %v", err)
	}
	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir}), nil)
	if err := runtime.writeAutoUpgradeState(statePath, autoUpgradeState{InFlight: &autoUpgradeInFlightState{PID: 4321, StartedAt: timePtr(time.Now().UTC())}}); err != nil {
		t.Fatalf("writeAutoUpgradeState() error = %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cliBinary := []byte("new looper")
	cliChecksum := sha256.Sum256(cliBinary)
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: execPath,
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looper-darwin-arm64","browser_download_url":"https://example.invalid/looper-darwin-arm64"},{"name":"looper-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looper-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looper-darwin-arm64":
				return binaryResponse(t, http.StatusOK, cliBinary), nil
			case "https://example.invalid/looper-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(cliChecksum[:])+"  looper-darwin-arm64\n"), nil
			case "http://127.0.0.1:4321/api/v1/status":
				t.Fatalf("unexpected daemon status request %q", req.URL.String())
				return nil, nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
				t.Fatalf("unexpected daemon release fetch %q", req.URL.String())
				return nil, nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	if exitCode := app.Run(context.Background(), []string{"upgrade", "--background-auto", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([upgrade --background-auto]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(auto-upgrade state) error = %v", err)
	}
	var state autoUpgradeState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("Unmarshal(auto-upgrade state) error = %v", err)
	}
	if state.Ready == nil {
		t.Fatal("state.Ready = nil, want ready notification")
	}
	if !state.Ready.CLIChanged || state.Ready.CLIVersion != "0.3.0" {
		t.Fatalf("state.Ready CLI = %#v, want changed version 0.3.0", state.Ready)
	}
	if state.Ready.DaemonChanged {
		t.Fatalf("state.Ready.DaemonChanged = true, want false; ready=%#v", state.Ready)
	}
	if state.Ready.DaemonRestartRequired {
		t.Fatalf("state.Ready.DaemonRestartRequired = true, want false; ready=%#v", state.Ready)
	}
	if _, err := os.Stat(managedPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(managedPath) error = %v, want no managed daemon install", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty string", stdout.String())
	}
}

func TestAutoUpgradeReadyNoticePrintsRestartCommand(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	statePath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.json")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	if err := os.MkdirAll(filepath.Dir(managedPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(managed dir) error = %v", err)
	}
	if err := os.WriteFile(managedPath, []byte("managed looperd"), 0o755); err != nil {
		t.Fatalf("WriteFile(managedPath) error = %v", err)
	}
	now := time.Now().UTC()
	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir}), nil)
	if err := runtime.writeAutoUpgradeState(statePath, autoUpgradeState{LastCheckedAt: &now, Ready: &autoUpgradeReadyState{DaemonChanged: true, DaemonVersion: "0.3.0", DaemonRestartRequired: true, CompletedAt: now}}); err != nil {
		t.Fatalf("writeAutoUpgradeState() error = %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/version":
				return jsonResponse(t, http.StatusOK, fmt.Sprintf(`{"ok":true,"requestId":"req_version","data":{"version":"0.2.0","build":{"apiVersion":%q},"binary":{"name":"looperd","path":%q}}}`, version.Current().Metadata.APIVersion, managedPath)), nil
			case "http://127.0.0.1:4321/api/v1/status":
				return jsonResponse(t, http.StatusOK, fmt.Sprintf(`{"ok":true,"requestId":"req_status","data":{"service":{"version":"0.2.0","binary":{"name":"looperd","path":%q}}}}`, managedPath)), nil
			default:
				if req.URL.Path == "/api/v1/status" || req.URL.Path == "/api/v1/version" {
					return nil, os.ErrNotExist
				}
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.3.0\n", ExitCode: 0}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	if exitCode := app.Run(context.Background(), []string{"version", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([version --config]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Auto-upgrade ready: looperd 0.3.0 is installed") {
		t.Fatalf("stderr = %q, want daemon ready message", stderr.String())
	}
	if !strings.Contains(stderr.String(), "looper daemon restart") {
		t.Fatalf("stderr = %q, want restart command", stderr.String())
	}
}

func TestCorruptAutoUpgradeStateSkipsNetworkAndPreservesFile(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	statePath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(state dir) error = %v", err)
	}
	const corrupt = "{invalid json\n"
	if err := os.WriteFile(statePath, []byte(corrupt), 0o644); err != nil {
		t.Fatalf("WriteFile(statePath) error = %v", err)
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		ExecutablePath: filepath.Join(homeDir, ".looper", "bin", "looper"),
		CLIChannel:     cliInstallChannelStable,
		HTTPClient:     allowVersionStatusProbeOnly(t),
		RunCommand:     missingDaemonVersionCommand,
	})

	exitCode := app.Run(context.Background(), []string{"version", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([version --config]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Auto-upgrade skipped: read state") {
		t.Fatalf("stderr = %q, want corrupt state warning", stderr.String())
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(statePath) error = %v", err)
	}
	if string(raw) != corrupt {
		t.Fatalf("state file content = %q, want %q", string(raw), corrupt)
	}
}

func TestUpgradeCheckPrintsSummary(t *testing.T) {
	t.Parallel()

	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[]}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--check", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --check]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([upgrade --check]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Upgrade check") {
		t.Fatalf("stdout = %q, want Upgrade check section", stdout.String())
	}
	for _, want := range []string{"cliCurrent", "0.2.1", "cliLatest", "0.3.0", "installed-binary"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want to contain %q", stdout.String(), want)
		}
	}
}

func TestUpgradeCheckUsesManagedProvenanceFromStatusAPI(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return jsonResponse(t, http.StatusOK, `{"success":true,"data":{"service":{"version":"0.2.1","binary":{"path":"`+managedPath+`"}}}}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[]}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--check", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --check --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([upgrade --check --json]) stderr = %q, want empty string", stderr.String())
	}
	var decoded struct {
		Daemon struct {
			CurrentVersion string `json:"currentVersion"`
			Installed      bool   `json:"installed"`
			Source         string `json:"source"`
			BinaryPath     string `json:"binaryPath"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal stdout JSON: %v\nraw=%q", err, stdout.String())
	}
	if decoded.Daemon.CurrentVersion != "0.2.1" {
		t.Fatalf("daemon.currentVersion = %q, want 0.2.1", decoded.Daemon.CurrentVersion)
	}
	if !decoded.Daemon.Installed {
		t.Fatal("daemon.installed = false, want true")
	}
	if decoded.Daemon.Source != "installed-binary" {
		t.Fatalf("daemon.source = %q, want installed-binary", decoded.Daemon.Source)
	}
	if decoded.Daemon.BinaryPath != managedPath {
		t.Fatalf("daemon.binaryPath = %q, want %q", decoded.Daemon.BinaryPath, managedPath)
	}
}

func TestSelectUpgradeDaemonVersionStatePreservesAPIBinaryPath(t *testing.T) {
	t.Parallel()

	managedPath := "/tmp/.looper/bin/looperd"
	state := selectUpgradeDaemonVersionState(
		json.RawMessage(`{"service":{"version":"0.2.1","binary":{"path":"`+managedPath+`"}}}`),
		&upgradeDaemonVersionState{Version: "0.2.1", Source: "installed-binary", BinaryPath: stringPtr(managedPath)},
		nil,
	)
	if state == nil {
		t.Fatal("selectUpgradeDaemonVersionState() = nil, want state")
	}
	if state.Source != "installed-binary" {
		t.Fatalf("state.Source = %q, want installed-binary", state.Source)
	}
	if state.BinaryPath == nil || *state.BinaryPath != managedPath {
		t.Fatalf("state.BinaryPath = %v, want %q", state.BinaryPath, managedPath)
	}
}

func TestReplaceBinaryAtomicallyRestoresPreviousOnFinalRenameFailure(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "looper")
	original := []byte("original")
	if err := os.WriteFile(installPath, original, 0o755); err != nil {
		t.Fatalf("WriteFile(installPath) error = %v", err)
	}

	var renameCalls int
	err := replaceBinaryAtomicallyWithRename(installPath, []byte("new"), func(oldPath string, newPath string) error {
		renameCalls++
		if renameCalls == 2 {
			return fmt.Errorf("injected final rename failure")
		}
		return os.Rename(oldPath, newPath)
	})
	if err == nil {
		t.Fatal("replaceBinaryAtomicallyWithRename() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "replace looper binary") {
		t.Fatalf("error = %v, want replace context", err)
	}
	restored, readErr := os.ReadFile(installPath)
	if readErr != nil {
		t.Fatalf("ReadFile(installPath) error = %v", readErr)
	}
	if string(restored) != string(original) {
		t.Fatalf("restored binary = %q, want %q", string(restored), string(original))
	}
	if _, statErr := os.Stat(installPath + ".new"); !os.IsNotExist(statErr) {
		t.Fatalf("staged binary still exists or stat failed: %v", statErr)
	}
}

func TestUpgradeRejectsCombiningCheckAndDaemon(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--check", "--daemon"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade --check --daemon]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "--check, --cli, and --daemon cannot be combined") {
		t.Fatalf("stderr = %q, want combination error", stderr.String())
	}
}

func TestUpgradeWithoutFlagsContinuesWithDaemonWhenCLISelfUpgradeRefused(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{1, 2, 3, 4}
	checksumText := "9f64a747e1b97f131fabb6b447296c9b6f0201e79fb3c5356e6c77e89b6a806a  looperd-darwin-arm64\n"
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: "/opt/homebrew/Cellar/looper/0.2.1/bin/looper",
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "CLI self-upgrade skipped") {
		t.Fatalf("stdout = %q, want CLI refusal guidance", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Proceeding with daemon upgrade") {
		t.Fatalf("stdout = %q, want daemon continuation note", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Installed looperd 0.3.0") {
		t.Fatalf("stdout = %q, want daemon install message", stdout.String())
	}
}

func TestUpgradeWithoutFlagsDoesNotInstallDaemonWhenCLIUpgradeFails(t *testing.T) {
	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".looper", "worktrees"), 0o755); err != nil {
		t.Fatalf("create test worktree root: %v", err)
	}
	t.Setenv("HOME", homeDir)

	execPath := filepath.Join(homeDir, ".looper", "bin", "looper")
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(exec dir): %v", err)
	}
	if err := os.WriteFile(execPath, []byte("old-cli"), 0o755); err != nil {
		t.Fatalf("WriteFile(execPath): %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	cliBinary := []byte("corrupt-cli")
	cliChecksum := sha256.Sum256([]byte("different-cli"))
	daemonBinary := []byte("new-daemon-binary")
	daemonChecksum := sha256.Sum256(daemonBinary)
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: execPath,
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest",
				"https://api.github.com/repos/nexu-io/looper/releases/tags/v9.9.9":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v9.9.9","assets":[{"name":"looper-darwin-arm64","browser_download_url":"https://example.invalid/looper-darwin-arm64"},{"name":"looper-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looper-darwin-arm64.sha256"},{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looper-darwin-arm64":
				return binaryResponse(t, http.StatusOK, cliBinary), nil
			case "https://example.invalid/looper-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(cliChecksum[:])+"  looper-darwin-arm64\n"), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, daemonBinary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(daemonChecksum[:])+"  looperd-darwin-arm64\n"), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run([upgrade]) exit code = %d, want non-zero", exitCode)
	}
	if _, err := os.Stat(managedPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(%q) error = %v, want daemon install to be skipped", managedPath, err)
	}
	if !strings.Contains(stderr.String(), "Downloading looperd…") {
		t.Fatalf("stderr = %q, want daemon lane to have started download prep", stderr.String())
	}
	if strings.Contains(stdout.String(), "Installed looperd") {
		t.Fatalf("stdout = %q, did not expect daemon install output", stdout.String())
	}
}

func TestUpgradeWithoutFlagsWritesSingleJSONDocument(t *testing.T) {
	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, ".looper", "worktrees"), 0o755); err != nil {
		t.Fatalf("create test worktree root: %v", err)
	}
	t.Setenv("HOME", homeDir)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{1, 2, 3, 4}
	checksumText := "9f64a747e1b97f131fabb6b447296c9b6f0201e79fb3c5356e6c77e89b6a806a  looperd-darwin-arm64\n"
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")

	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homeDir,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: "/opt/homebrew/Cellar/looper/0.2.1/bin/looper",
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--json", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloaded looperd (4 B)") {
		t.Fatalf("Run([upgrade --json]) stderr = %q, want daemon download progress", stderr.String())
	}
	if strings.Contains(stdout.String(), "Proceeding with daemon upgrade") {
		t.Fatalf("stdout = %q, want JSON without human progress text", stdout.String())
	}

	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal stdout JSON: %v\nraw=%q", err, stdout.String())
	}
	if _, ok := decoded["cli"].(map[string]any); !ok {
		t.Fatalf("stdout JSON missing cli object: %#v", decoded)
	}
	if _, ok := decoded["daemon"].(map[string]any); !ok {
		t.Fatalf("stdout JSON missing daemon object: %#v", decoded)
	}
}

func TestUpgradeCLIRefusesHomebrewInstallWithGuidance(t *testing.T) {
	t.Parallel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		ExecutablePath: "/opt/homebrew/Cellar/looper/0.2.1/bin/looper",
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request before Homebrew refusal: %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--cli"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade --cli]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "brew upgrade looper") {
		t.Fatalf("stderr = %q, want brew guidance", stderr.String())
	}
}

func TestUpgradeCLIRefusesHomebrewSymlinkWithGuidance(t *testing.T) {
	t.Parallel()

	homebrewRoot := t.TempDir()
	targetPath := filepath.Join(homebrewRoot, "usr", "local", "Cellar", "looper", "0.2.1", "bin", "looper")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(target dir) error = %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("os.WriteFile(target) error = %v", err)
	}

	symlinkPath := filepath.Join(homebrewRoot, "usr", "local", "bin", "looper")
	if err := os.MkdirAll(filepath.Dir(symlinkPath), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(symlink dir) error = %v", err)
	}
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Skipf("os.Symlink() unavailable: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        homebrewRoot,
		ExecutablePath: symlinkPath,
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request before Homebrew refusal: %q", req.URL.String())
			return nil, nil
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--cli"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade --cli]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "brew upgrade looper") {
		t.Fatalf("stderr = %q, want brew guidance", stderr.String())
	}
}

func TestUpgradeCLIPreflightsInstallPathBeforeDownload(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	blockingPath := filepath.Join(root, ".looper")
	if err := os.WriteFile(blockingPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(blockingPath) error = %v", err)
	}
	execPath := filepath.Join(root, ".looper", "bin", "looper")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        t.TempDir(),
		ExecutablePath: execPath,
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looper-darwin-arm64","browser_download_url":"https://example.invalid/looper-darwin-arm64"},{"name":"looper-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looper-darwin-arm64.sha256"}]}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--cli"})
	if exitCode != 1 {
		t.Fatalf("Run([upgrade --cli]) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "install location is not writable") {
		t.Fatalf("stderr = %q, want writable guidance", stderr.String())
	}
	if strings.Contains(stderr.String(), "download") {
		t.Fatalf("stderr = %q, did not expect download failure", stderr.String())
	}
}

func TestUpgradeCLIPrintsDownloadProgressToStderr(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	execPath := filepath.Join(root, ".looper", "bin", "looper")
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(exec dir) error = %v", err)
	}
	if err := os.WriteFile(execPath, []byte("old looper"), 0o755); err != nil {
		t.Fatalf("WriteFile(execPath) error = %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte("new looper")
	checksum := sha256.Sum256(binary)
	checksumText := hex.EncodeToString(checksum[:]) + "  looper-darwin-arm64\n"
	app := New(Deps{
		Stdout:         stdout,
		Stderr:         stderr,
		HomeDir:        root,
		Platform:       "darwin",
		Arch:           "arm64",
		ExecutablePath: execPath,
		CLIChannel:     cliInstallChannelStable,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v999.0.0","assets":[{"name":"looper-darwin-arm64","browser_download_url":"https://example.invalid/looper-darwin-arm64"},{"name":"looper-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looper-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looper-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looper-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--cli"})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --cli]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloaded looper (10 B)") {
		t.Fatalf("stderr = %q, want CLI download progress", stderr.String())
	}
	if strings.Contains(stdout.String(), "Downloading looper") {
		t.Fatalf("stdout = %q, did not expect download progress", stdout.String())
	}
}

func TestDetectCLIInstallSourceTreatsInstallerSelectedUserBinAsRelease(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		t.Skipf("cannot resolve user home directory: %v", err)
	}
	userBin := filepath.Join(homeDir, "bin")
	t.Setenv("PATH", userBin)

	got := detectCLIInstallSourceForChannel(filepath.Join(userBin, "looper"), cliInstallChannelStable)
	if got != cliInstallSourceRelease {
		t.Fatalf("detectCLIInstallSourceForChannel(user PATH bin, stable) = %q, want %q", got, cliInstallSourceRelease)
	}
}

func TestDetectCLIInstallSourceTreatsGoBinAsDevBeforeUserBinRelease(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		t.Skipf("cannot resolve user home directory: %v", err)
	}
	goBin := filepath.Join(homeDir, "go", "bin")
	t.Setenv("PATH", goBin)

	got := detectCLIInstallSourceForChannel(filepath.Join(goBin, "looper"), cliInstallChannelStable)
	if got != cliInstallSourceDev {
		t.Fatalf("detectCLIInstallSourceForChannel(go PATH bin, stable) = %q, want %q", got, cliInstallSourceDev)
	}
}

func TestDetectCLIInstallSourceTreatsDevChannelAsDevRegardlessOfPath(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		t.Skipf("cannot resolve user home directory: %v", err)
	}
	// Paths that would normally be classified as release binaries.
	releasePaths := []string{
		filepath.Join(homeDir, ".local", "bin", "looper"),
		"/usr/local/bin/looper",
		filepath.Join(homeDir, ".looper", "bin", "looper"),
		"/opt/homebrew/Cellar/looper/1.0.0/bin/looper",
	}
	for _, p := range releasePaths {
		if got := detectCLIInstallSourceForChannel(p, "dev"); got != cliInstallSourceDev {
			t.Errorf("detectCLIInstallSourceForChannel(%q, dev) = %q, want %q", p, got, cliInstallSourceDev)
		}
	}
}

func TestDetectCLIInstallSourceTreatsUnknownChannelAsDev(t *testing.T) {
	// Any non-stable channel (rc, nightly, custom builds) must opt out of
	// auto-upgrade by default; auto-upgrade is gated on release-binary so
	// returning dev for unknown channels is the safe choice.
	for _, channel := range []string{"", "rc", "nightly", "unknown"} {
		if got := detectCLIInstallSourceForChannel("/usr/local/bin/looper", channel); got != cliInstallSourceDev {
			t.Errorf("detectCLIInstallSourceForChannel(release path, %q) = %q, want %q", channel, got, cliInstallSourceDev)
		}
	}
}

func TestUpgradeDaemonPrintsRestartHint(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{1, 2, 3, 4}
	checksumText := "9f64a747e1b97f131fabb6b447296c9b6f0201e79fb3c5356e6c77e89b6a806a  looperd-darwin-arm64\n"
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--daemon", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloaded looperd (4 B)") {
		t.Fatalf("Run([upgrade --daemon]) stderr = %q, want daemon download progress", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Upgraded looperd 0.2.1 → 0.3.0") {
		t.Fatalf("stdout = %q, want upgrade confirmation", stdout.String())
	}
	if !strings.Contains(stdout.String(), "looper daemon restart") {
		t.Fatalf("stdout = %q, want restart hint", stdout.String())
	}
}

func TestUpgradeDaemonSkipsCurrentManagedBinary(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")

	app := New(Deps{
		Stdout:  stdout,
		Stderr:  stderr,
		HomeDir: homeDir,
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.2.1","assets":[]}`), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.2.1\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--daemon", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run([upgrade --daemon]) stderr = %q, want empty string", stderr.String())
	}
	if !strings.Contains(stdout.String(), "looperd is already up to date (0.2.1)") {
		t.Fatalf("stdout = %q, want current-version message", stdout.String())
	}
	if !strings.Contains(stdout.String(), managedPath) {
		t.Fatalf("stdout = %q, want managed binary path", stdout.String())
	}
}

func TestUpgradeDaemonInstallsManagedBinaryWhenOnlyPathBinaryExists(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	binary := []byte{1, 2, 3, 4}
	checksumText := "9f64a747e1b97f131fabb6b447296c9b6f0201e79fb3c5356e6c77e89b6a806a  looperd-darwin-arm64\n"
	configPath := writeCLIConfig(t, "http://127.0.0.1:4321", "")

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "http://127.0.0.1:4321/api/v1/status":
				return nil, fmt.Errorf("daemon offline")
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.4.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.4.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.4.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64":
				return binaryResponse(t, http.StatusOK, binary), nil
			case "https://example.invalid/looperd-darwin-arm64.sha256":
				return textResponse(t, http.StatusOK, checksumText), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == filepath.Join(homeDir, ".looper", "bin", "looperd") && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
			}
			if command == looperdBinaryName && strings.Join(args, " ") == "--version" {
				return commandExecutionResult{Stdout: "0.4.0\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"upgrade", "--daemon", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run([upgrade --daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Downloaded looperd (4 B)") {
		t.Fatalf("Run([upgrade --daemon]) stderr = %q, want daemon download progress", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Installed managed looperd 0.4.0") {
		t.Fatalf("stdout = %q, want managed install message", stdout.String())
	}
	if !strings.Contains(stdout.String(), "previously using looperd") {
		t.Fatalf("stdout = %q, want PATH fallback note", stdout.String())
	}
}

func TestManagedDaemonInstallUpgradeLifecycleEndToEnd(t *testing.T) {
	homeDir := t.TempDir()
	managedPath := filepath.Join(homeDir, ".looper", "bin", "looperd")
	statePath := filepath.Join(homeDir, ".looper", "auto-upgrade.state.json")
	configPath := writeCLIConfig(t, "http://daemon.test", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	oldBinary := []byte("looperd-v0.2.0")
	newBinary := []byte("looperd-v0.3.0")
	oldChecksum := sha256.Sum256(oldBinary)
	newChecksum := sha256.Sum256(newBinary)

	type processState struct {
		version string
		alive   bool
	}
	var (
		mu             sync.Mutex
		nextPID        = 2000
		processes      = map[int]processState{}
		runningPID     int
		runningVersion string
		latestCalls    int
	)

	app := New(Deps{
		Stdout:   stdout,
		Stderr:   stderr,
		HomeDir:  homeDir,
		Platform: "darwin",
		Arch:     "arm64",
		HTTPClient: newTestHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case "https://api.github.com/repos/nexu-io/looper/releases/latest":
				latestCalls += 1
				if latestCalls == 1 {
					return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.2.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256"}]}`), nil
				}
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.3.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.3.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256"}]}`), nil
			case "https://api.github.com/repos/nexu-io/looper/releases/tags/v0.2.0":
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v0.2.0","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256"}]}`), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.2.0":
				return binaryResponse(t, http.StatusOK, oldBinary), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.2.0.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(oldChecksum[:])+"  looperd-darwin-arm64\n"), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.3.0":
				return binaryResponse(t, http.StatusOK, newBinary), nil
			case "https://example.invalid/looperd-darwin-arm64-v0.3.0.sha256":
				return textResponse(t, http.StatusOK, hex.EncodeToString(newChecksum[:])+"  looperd-darwin-arm64\n"), nil
			case "http://daemon.test/api/v1/status", "http://127.0.0.1:17310/api/v1/status":
				mu.Lock()
				defer mu.Unlock()
				if runningPID == 0 {
					return nil, fmt.Errorf("daemon offline")
				}
				return jsonResponse(t, http.StatusOK, fmt.Sprintf(`{"ok":true,"requestId":"req_status","data":{"service":{"version":%q,"binary":{"name":"looperd"}}}}`, runningVersion)), nil
			default:
				t.Fatalf("unexpected request URL %q", req.URL.String())
				return nil, nil
			}
		}),
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			_ = ctx
			_ = timeout
			if command == managedPath && strings.Join(args, " ") == "--version" {
				raw, err := os.ReadFile(managedPath)
				if err != nil {
					return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
				}
				switch string(raw) {
				case string(oldBinary):
					return commandExecutionResult{Stdout: "0.2.0\n", ExitCode: 0}, nil
				case string(newBinary):
					return commandExecutionResult{Stdout: "0.3.0\n", ExitCode: 0}, nil
				default:
					return commandExecutionResult{ExitCode: 1, Stderr: "unknown binary"}, nil
				}
			}
			if command == "ps" && len(args) == 4 && args[0] == "-p" && args[2] == "-o" && args[3] == "command=" {
				return commandExecutionResult{Stdout: managedPath + "\n", ExitCode: 0}, nil
			}
			return commandExecutionResult{ExitCode: 1, Stderr: "not found"}, nil
		},
		SpawnDetached: func(command string, args []string, cwd string, env []string) (int, error) {
			_ = args
			_ = cwd
			_ = env
			if command != managedPath {
				return 0, fmt.Errorf("unexpected command %q", command)
			}
			raw, err := os.ReadFile(managedPath)
			if err != nil {
				return 0, err
			}
			version := ""
			switch string(raw) {
			case string(oldBinary):
				version = "0.2.0"
			case string(newBinary):
				version = "0.3.0"
			default:
				return 0, fmt.Errorf("unknown binary bytes")
			}

			mu.Lock()
			defer mu.Unlock()
			nextPID += 1
			processes[nextPID] = processState{version: version, alive: true}
			runningPID = nextPID
			runningVersion = version
			return nextPID, nil
		},
		KillProcess: func(pid int, signal int) error {
			mu.Lock()
			defer mu.Unlock()
			proc, ok := processes[pid]
			if !ok || !proc.alive {
				return os.ErrProcessDone
			}
			if signal == 15 {
				proc.alive = false
				processes[pid] = proc
				if runningPID == pid {
					runningPID = 0
					runningVersion = ""
				}
			}
			return nil
		},
		Sleep: func(duration time.Duration) {
			_ = duration
		},
		Getwd: func() (string, error) {
			return t.TempDir(), nil
		},
	})

	if exitCode := app.Run(context.Background(), []string{"daemon", "install", "--force", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon install --force]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Installed looperd (darwin-arm64)") {
		t.Fatalf("stdout = %q, want install confirmation", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"daemon", "start", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon start]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"daemon", "status", "--json", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon status --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	assertJSONContains(t, stdout.String(), "apiReachable", true)
	assertJSONContains(t, stdout.String(), "daemonVersion", "0.2.0")
	assertJSONContains(t, stdout.String(), "daemonVersionSource", "api")

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"upgrade", "--daemon", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([upgrade --daemon]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Upgraded looperd 0.2.0 → 0.3.0") {
		t.Fatalf("stdout = %q, want upgrade confirmation", stdout.String())
	}
	upgradeCompletedAt := time.Now().UTC()
	runtime := newCommandRuntime(New(Deps{HomeDir: homeDir}), nil)
	if err := runtime.writeAutoUpgradeState(statePath, autoUpgradeState{Ready: &autoUpgradeReadyState{DaemonChanged: true, DaemonVersion: "0.3.0", DaemonRestartRequired: true, RunningDaemonVersion: "0.2.0", CompletedAt: upgradeCompletedAt}}); err != nil {
		t.Fatalf("writeAutoUpgradeState() error = %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"daemon", "restart", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon restart]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := app.Run(context.Background(), []string{"daemon", "status", "--json", "--config", configPath}); exitCode != 0 {
		t.Fatalf("Run([daemon status --json]) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	assertJSONContains(t, stdout.String(), "apiReachable", true)
	assertJSONContains(t, stdout.String(), "daemonVersion", "0.3.0")
	assertJSONContains(t, stdout.String(), "daemonVersionSource", "api")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(auto-upgrade state) error = %v", err)
	}
	var state autoUpgradeState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("Unmarshal(auto-upgrade state) error = %v", err)
	}
	if state.Ready != nil {
		t.Fatalf("state.Ready = %#v, want cleared after restart", state.Ready)
	}
}

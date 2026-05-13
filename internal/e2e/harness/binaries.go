package harness

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

const (
	envLooperPath    = "LOOPER_E2E_LOOPER_PATH"
	envLooperdPath   = "LOOPER_E2E_LOOPERD_PATH"
	envFakeAgentPath = "LOOPER_E2E_FAKE_AGENT_PATH"
	envFakeGHPath    = "LOOPER_E2E_FAKE_GH_PATH"
	envFakeOSAPath   = "LOOPER_E2E_FAKE_OSASCRIPT_PATH"
)

type BuiltBinaries struct {
	LooperPath        string
	LooperdPath       string
	FakeAgentPath     string
	FakeGHPath        string
	FakeOsascriptPath string
}

var (
	builtOnce sync.Once
	builtSet  BuiltBinaries
	builtErr  error
)

func RunTestMain(m *testing.M) int {
	bins, dir, err := buildAll()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build e2e binaries: %v\n", err)
		return 1
	}
	setBinaryEnv(bins)
	code := m.Run()
	_ = os.RemoveAll(dir)
	return code
}

func MustBinaries(tb testing.TB) BuiltBinaries {
	tb.Helper()
	if bins, ok := binariesFromEnv(); ok {
		return bins
	}
	builtOnce.Do(func() { builtSet, _, builtErr = buildAll() })
	if builtErr != nil {
		tb.Fatalf("build e2e binaries: %v", builtErr)
	}
	return builtSet
}

func binariesFromEnv() (BuiltBinaries, bool) {
	bins := BuiltBinaries{
		LooperPath:        os.Getenv(envLooperPath),
		LooperdPath:       os.Getenv(envLooperdPath),
		FakeAgentPath:     os.Getenv(envFakeAgentPath),
		FakeGHPath:        os.Getenv(envFakeGHPath),
		FakeOsascriptPath: os.Getenv(envFakeOSAPath),
	}
	if bins.LooperPath == "" || bins.LooperdPath == "" || bins.FakeAgentPath == "" || bins.FakeGHPath == "" || bins.FakeOsascriptPath == "" {
		return BuiltBinaries{}, false
	}
	return bins, true
}

func setBinaryEnv(bins BuiltBinaries) {
	_ = os.Setenv(envLooperPath, bins.LooperPath)
	_ = os.Setenv(envLooperdPath, bins.LooperdPath)
	_ = os.Setenv(envFakeAgentPath, bins.FakeAgentPath)
	_ = os.Setenv(envFakeGHPath, bins.FakeGHPath)
	_ = os.Setenv(envFakeOSAPath, bins.FakeOsascriptPath)
}

func buildAll() (BuiltBinaries, string, error) {
	repoRoot, err := repoRoot()
	if err != nil {
		return BuiltBinaries{}, "", err
	}
	outDir, err := os.MkdirTemp("", "looper-e2e-bin-*")
	if err != nil {
		return BuiltBinaries{}, "", err
	}
	bins := BuiltBinaries{
		LooperPath:        filepath.Join(outDir, executableName("looper")),
		LooperdPath:       filepath.Join(outDir, executableName("looperd")),
		FakeAgentPath:     filepath.Join(outDir, executableName("fake-agent")),
		FakeGHPath:        filepath.Join(outDir, executableName("fake-gh")),
		FakeOsascriptPath: filepath.Join(outDir, executableName("fake-osascript")),
	}
	targets := []struct{ out, pkg, name string }{
		{bins.LooperPath, "./cmd/looper", "looper"},
		{bins.LooperdPath, "./cmd/looperd", "looperd"},
		{bins.FakeAgentPath, "./internal/e2e/harness/cmd/fake-agent", "fake-agent"},
		{bins.FakeGHPath, "./internal/e2e/harness/cmd/fake-gh", "fake-gh"},
		{bins.FakeOsascriptPath, "./internal/e2e/harness/cmd/fake-osascript", "fake-osascript"},
	}
	for _, target := range targets {
		if err := goBuild(repoRoot, target.out, target.pkg); err != nil {
			_ = os.RemoveAll(outDir)
			return BuiltBinaries{}, "", fmt.Errorf("build %s: %w", target.name, err)
		}
	}
	return bins, outDir, nil
}

func goBuild(repoRoot string, out string, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return nil
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..")), nil
}

func executableName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

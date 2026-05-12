package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pkgapi "github.com/nexu-io/looper/pkg/api"
)

type DaemonProcess struct {
	cmd        *exec.Cmd
	home       TempHome
	configPath string
	stdoutPath string
	stderrPath string
	baseURL    string
	doneCh     chan struct{}
	mu         sync.RWMutex
	waitErr    error
}

func (d *DaemonProcess) BaseURL() string {
	if d == nil {
		return ""
	}
	return d.baseURL
}

func StartLooperd(tb testing.TB, bins BuiltBinaries, home TempHome, configPath string, extraEnv map[string]string, host string, port int) *DaemonProcess {
	tb.Helper()
	stdoutPath := filepath.Join(home.ArtifactsDir, "looperd.stdout.log")
	stderrPath := filepath.Join(home.ArtifactsDir, "looperd.stderr.log")
	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		tb.Fatalf("create looperd stdout log: %v", err)
	}
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		_ = stdoutFile.Close()
		tb.Fatalf("create looperd stderr log: %v", err)
	}
	cmd := exec.Command(bins.LooperdPath, "--config", configPath)
	cmd.Dir = home.WorkingDir
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.Env = append(os.Environ(), home.EnvSlice()...)
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if err := cmd.Start(); err != nil {
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
		tb.Fatalf("start looperd: %v", err)
	}
	proc := &DaemonProcess{cmd: cmd, home: home, configPath: configPath, stdoutPath: stdoutPath, stderrPath: stderrPath, baseURL: BaseURL(host, port), doneCh: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
		proc.mu.Lock()
		proc.waitErr = err
		proc.mu.Unlock()
		close(proc.doneCh)
	}()
	tb.Cleanup(func() {
		proc.Stop(context.Background())
		if tb.Failed() {
			proc.DumpArtifacts(tb)
		}
	})
	return proc
}

func (d *DaemonProcess) WaitForReady(ctx context.Context) (map[string]any, error) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	statusURL := d.baseURL + "/api/v1/status"
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for %s: %w", statusURL, ctx.Err())
		case <-d.doneCh:
			err := d.exitErr()
			if err == nil {
				return nil, errors.New("looperd exited before readiness")
			}
			return nil, fmt.Errorf("looperd exited before readiness: %w", err)
		default:
		}
		resp, err := client.Get(statusURL)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				var envelope pkgapi.Envelope[map[string]any]
				if decodeErr := json.Unmarshal(body, &envelope); decodeErr == nil && envelope.OK && envelope.Data != nil {
					return *envelope.Data, nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (d *DaemonProcess) Stop(ctx context.Context) {
	if d == nil || d.cmd == nil || d.cmd.Process == nil {
		return
	}
	_ = d.cmd.Process.Signal(os.Interrupt)
	select {
	case <-ctx.Done():
		_ = d.cmd.Process.Signal(syscall.SIGKILL)
		<-d.doneCh
	case <-time.After(5 * time.Second):
		_ = d.cmd.Process.Signal(syscall.SIGKILL)
		<-d.doneCh
	case <-d.doneCh:
	}
	d.cmd = nil
}

func (d *DaemonProcess) exitErr() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.waitErr
}

func (d *DaemonProcess) DumpArtifacts(tb testing.TB) {
	tb.Helper()
	for _, path := range []string{d.configPath, d.stdoutPath, d.stderrPath, d.home.LogDir, d.home.ArtifactsDir} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		dumpPath(tb, path)
	}
}

func dumpPath(tb testing.TB, path string) {
	tb.Helper()
	info, err := os.Stat(path)
	if err != nil {
		tb.Logf("artifact missing: %s (%v)", path, err)
		return
	}
	if info.IsDir() {
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			tb.Logf("artifact dir unreadable: %s (%v)", path, readErr)
			return
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		tb.Logf("artifact dir %s: %s", path, strings.Join(names, ", "))
		return
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		tb.Logf("artifact file unreadable: %s (%v)", path, readErr)
		return
	}
	tb.Logf("artifact file %s:\n%s", path, string(content))
}

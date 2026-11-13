package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

// Opt-in OS enforcement test; never invokes a model or a forge. Use a pinned
// nono v0.81.0 binary. The same test binary can run on macOS and Linux ARM64.
func TestNonoLive(t *testing.T) {
	binary := os.Getenv("LOOPER_TEST_NONO")
	if binary == "" {
		t.Skip("set LOOPER_TEST_NONO to a pinned nono executable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "ln-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	cwd, scratch, home := filepath.Join(root, "worktree"), filepath.Join(root, "scratch"), filepath.Join(root, "home")
	for _, dir := range []string{cwd, scratch, home} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	allowed, secret := filepath.Join(cwd, "source"), filepath.Join(root, "secret")
	for _, path := range []string{allowed, secret} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	profile := filepath.Join(root, "profile.json")
	raw, err := json.Marshal(map[string]any{
		"meta": map[string]any{"name": "looper-test"}, "groups": map[string]any{"include": []string{}, "exclude": []string{"system_read_macos", "system_write_macos", "system_write_linux"}},
		"workdir": map[string]any{"access": "read"}, "filesystem": map[string]any{"read_file": []string{exe}, "read": []string{"/bin", "/usr/bin", "/usr/lib", "/System/Library", "/etc", "/dev"}, "allow": []string{scratch}, "write_file": []string{"/dev/null"}},
		"network": map[string]any{"block": true}, "security": map[string]any{"capability_elevation": false}, "linux": map[string]any{"af_unix_mediation": "pathname"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	socket, other := filepath.Join(root, "allowed.sock"), filepath.Join(root, "other.sock")
	for _, path := range []string{socket, other} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}()
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	env := map[string]string{"HOME": home, "TMPDIR": scratch, "LOOPER_NONO_CHILD": "enforce", "LOOPER_FIXTURE_SECRET": secret, "LOOPER_FIXTURE_SCRATCH": scratch, "LOOPER_FIXTURE_SOCKET": socket, "LOOPER_FIXTURE_OTHER_SOCKET": other, "LOOPER_FIXTURE_TCP": tcp.Addr().String()}
	opts := withParamsOwner(ExecutorConfig{Vendor: "custom", Params: map[string]any{"command": exe, "args": []any{"-test.run=^TestNonoChild$"}}, Env: env}, "custom")
	opts.Nono = &config.ReviewerNonoConfig{Command: binary, Profile: profile}
	input := RunInput{WorkingDirectory: cwd, Prompt: "OS enforcement probe", Timeout: 15 * time.Second, GracefulShutdown: 100 * time.Millisecond, Env: map[string]string{forge.TrustedReviewSockEnv: socket}}
	t.Run("enforcement-and-streams", func(t *testing.T) {
		run, err := New(opts).Start(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := run.Wait(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "completed" || result.Summary != "enforced" || !strings.Contains(result.Stderr, "child-stderr") {
			t.Fatalf("sandbox result: %+v", result)
		}
	})
	t.Run("invalid-policy-fails-closed", func(t *testing.T) {
		bad := filepath.Join(root, "invalid.json")
		if err := os.WriteFile(bad, []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		options := opts
		options.Nono = &config.ReviewerNonoConfig{Command: binary, Profile: bad}
		run, err := New(options).Start(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := run.Wait(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "failed" || strings.Contains(result.Stdout, "enforced") {
			t.Fatalf("invalid policy result: %+v", result)
		}
	})
	for _, stop := range []string{"timeout", "cancel"} {
		t.Run(stop+"-drains-descendants", func(t *testing.T) {
			pidFile := filepath.Join(scratch, "child.pid")
			_ = os.Remove(pidFile)
			opts.Config.Env["LOOPER_NONO_CHILD"] = "sleep"
			input.Timeout = 15 * time.Second
			if stop == "timeout" {
				input.Timeout = 2 * time.Second
			}
			run, err := New(opts).Start(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if stop == "cancel" {
				ready := time.Now().Add(5 * time.Second)
				for time.Now().Before(ready) {
					if _, err := os.Stat(pidFile); err == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if err := run.Kill("test cancel"); err != nil {
					t.Fatal(err)
				}
			}
			result, err := run.Wait(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := "timeout"
			if stop == "cancel" {
				want = "killed"
			}
			if result.Status != want {
				t.Fatalf("%s result: %+v", stop, result)
			}
			pidRaw, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("descendant %d survived sandbox %s", pid, stop)
		})
	}
}

func TestNonoChild(t *testing.T) {
	mode := os.Getenv("LOOPER_NONO_CHILD")
	if mode == "" {
		t.Skip("subprocess only")
	}
	scratch := os.Getenv("LOOPER_FIXTURE_SCRATCH")
	if mode == "sleep" {
		child := exec.Command("/bin/sleep", "120")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scratch, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
		_ = child.Wait()
		return
	}
	if _, err := os.ReadFile("source"); err != nil {
		t.Fatalf("source read denied: %v", err)
	}
	if _, err := os.ReadFile(os.Getenv("LOOPER_FIXTURE_SECRET")); err == nil {
		t.Fatal("secret read allowed")
	}
	if err := os.WriteFile("source", []byte("changed"), 0600); err == nil {
		t.Fatal("source write allowed")
	}
	if err := os.WriteFile(filepath.Join(scratch, "output"), []byte("ok"), 0600); err != nil {
		t.Fatalf("scratch write denied: %v", err)
	}
	for _, tc := range []struct {
		network, address string
		allowed          bool
	}{
		{"unix", os.Getenv("LOOPER_FIXTURE_SOCKET"), true}, {"unix", os.Getenv("LOOPER_FIXTURE_OTHER_SOCKET"), false}, {"tcp", os.Getenv("LOOPER_FIXTURE_TCP"), false},
	} {
		conn, err := net.DialTimeout(tc.network, tc.address, time.Second)
		if conn != nil {
			conn.Close()
		}
		if (err == nil) != tc.allowed {
			t.Fatalf("%s allowed=%v: %v", tc.network, tc.allowed, err)
		}
	}
	fmt.Fprintln(os.Stderr, "child-stderr")
	fmt.Println(`__LOOPER_RESULT__={"summary":"enforced"}`)
}

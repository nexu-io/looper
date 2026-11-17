package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

func TestNonoWrapsNativeResumeAndFallback(t *testing.T) {
	dir := t.TempDir()
	profile, wrapper, agent, log := filepath.Join(dir, "profile.json"), filepath.Join(dir, "nono"), filepath.Join(dir, "agent"), filepath.Join(dir, "calls")
	files := map[string]string{
		profile: `{}`,
		wrapper: `#!/bin/sh
if env | grep -E '^(NONO_|LD_|DYLD_)' >/dev/null; then exit 42; fi
printf '%s\n' "$*" >> "$WRAP_LOG"
while [ "$1" != '--' ]; do shift; done
shift
exec "$@"
`,
		agent: `#!/bin/sh
case "$*" in *resume*) printf '%s\n' 'resume failed' >&2; exit 2;; esac
printf '%s\n' '__LOOPER_RESULT__={"summary":"sandboxed checkpoint"}'
`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	opts := withParamsOwner(ExecutorConfig{Vendor: config.AgentVendorCodex, Params: map[string]any{"command": agent}, NativeResumeEnabled: true,
		Env: map[string]string{"NONO_ALLOW": "/", "NONO_CAPABILITY_ELEVATION": "true", "WRAP_LOG": log}}, config.AgentVendorCodex)
	opts.Nono = &config.ReviewerNonoConfig{Command: wrapper, Profile: profile}
	e := New(opts)
	run, err := e.Start(context.Background(), RunInput{WorkingDirectory: dir, Prompt: "full prompt with spaces", NativeResumePrompt: "continue", NativeSessionID: "session", Timeout: 5 * time.Second, Env: map[string]string{"NONO_PROFILE": "untrusted"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := run.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Summary != "sandboxed checkpoint" {
		t.Fatalf("result: %+v", result)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "-- "+agent+" exec resume session continue") || !strings.Contains(lines[1], "-- "+agent+" exec full prompt with spaces") {
		t.Fatalf("wrapped attempts: %s", data)
	}
}

func TestNonoFailureNeverLaunchesAgent(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "resume"}[native], func(t *testing.T) {
			dir := t.TempDir()
			profile := filepath.Join(dir, "profile.json")
			marker := filepath.Join(dir, "escaped")
			agent := filepath.Join(dir, "agent")
			if err := os.WriteFile(agent, []byte("#!/bin/sh\nprintf ran > \"$ESCAPE_MARKER\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(profile, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			opts := withParamsOwner(ExecutorConfig{Vendor: config.AgentVendorCodex, Params: map[string]any{"command": agent}, Env: map[string]string{"ESCAPE_MARKER": marker}, NativeResumeEnabled: true}, config.AgentVendorCodex)
			opts.Nono = &config.ReviewerNonoConfig{Command: filepath.Join(dir, "missing-nono"), Profile: profile}
			input := RunInput{WorkingDirectory: dir, Prompt: "review"}
			if native {
				input.NativeSessionID = "session"
			}
			if _, err := New(opts).Start(context.Background(), input); err == nil {
				t.Fatal("missing nono accepted")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("agent ran outside sandbox: %v", err)
			}
		})
	}
}

func TestNonoRejectsMissingOrRelativePolicy(t *testing.T) {
	dir := t.TempDir()
	for _, profile := range []string{filepath.Join(dir, "missing.json"), "repo-policy.json", dir} {
		opts := withParamsOwner(ExecutorConfig{Vendor: "custom", Params: map[string]any{"command": "/bin/echo"}}, "custom")
		opts.Nono = &config.ReviewerNonoConfig{Command: "/bin/echo", Profile: profile}
		if _, err := New(opts).Start(context.Background(), RunInput{WorkingDirectory: dir, Prompt: "review"}); err == nil {
			t.Fatalf("accepted invalid profile %q", profile)
		}
	}
}

func TestNonoGrantsOnlyRunCapabilities(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(profile, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	e := New(ExecutorOptions{Nono: &config.ReviewerNonoConfig{Command: "/bin/echo", Profile: profile}})
	input := RunInput{WorkingDirectory: dir, Env: map[string]string{forge.TrustedReviewSockEnv: "/run/review.sock"}}
	hosting := &hostingExecution{dir: "/run/shims", env: map[string]string{forge.HostCLIEnv: "/opt/looper", forge.HostSockEnv: "/run/hosting.sock"}}
	cmd, err := e.agentCommand("codex", []string{"exec", "prompt with spaces"}, input, "review", ExecutorConfig{}, hosting)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, "\n")
	for _, grant := range []string{"--allow-unix-socket\n/run/review.sock", "--allow-unix-socket\n/run/hosting.sock", "--read\n/run/shims", "--read-file\n/opt/looper", "--\ncodex\nexec\nprompt with spaces"} {
		if !strings.Contains(args, grant) {
			t.Fatalf("missing %s in %s", grant, args)
		}
	}
	if strings.Contains(args, "--allow\n") || strings.Contains(args, "--allow-unix-socket-dir") {
		t.Fatalf("broad dynamic grant: %s", args)
	}
	plain, err := New(ExecutorOptions{}).agentCommand("/bin/echo", []string{"hello"}, input, "review", ExecutorConfig{}, nil)
	if err != nil || len(plain.Args) != 2 || plain.Args[1] != "hello" {
		t.Fatalf("disabled sandbox changed command: %v %v", plain, err)
	}
}

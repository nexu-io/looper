package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func piOwner() *config.AgentVendor {
	v := config.AgentVendorPi
	return &v
}

func TestResolvePiArgs(t *testing.T) {
	model := "sonnet"
	base := ExecutorConfig{Vendor: config.AgentVendorPi, Model: &model}
	workdir := "/tmp/looper-worktree"
	prompt := "generated prompt"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"default", nil, []string{"--model", model, "-p", prompt, "--approve"}},
		{"configured model", []string{"--model", "custom"}, []string{"--model", "custom", "-p", prompt, "--approve"}},
		{"equals model", []string{"--model=custom"}, []string{"--model=custom", "-p", prompt, "--approve"}},
		{"short print owns prompt", []string{"-p", "custom"}, []string{"--model", model, "-p", "custom", "--approve"}},
		{"long print owns prompt", []string{"--print", "custom"}, []string{"--model", model, "--print", "custom", "--approve"}},
		{"print equals owns prompt", []string{"--print=custom"}, []string{"--model", model, "--print=custom", "--approve"}},
		{"short approve", []string{"-a"}, []string{"--model", model, "-a", "-p", prompt}},
		{"long approve", []string{"--approve"}, []string{"--model", model, "--approve", "-p", prompt}},
		{"no approve", []string{"-na"}, []string{"--model", model, "-na", "-p", prompt}},
		{"long no approve", []string{"--no-approve"}, []string{"--model", model, "--no-approve", "-p", prompt}},
		// workdir is process Dir only; pi has no --cwd default.
		{"workdir ignored for argv", nil, []string{"--model", model, "-p", prompt, "--approve"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]string(nil), tt.args...)
			cfg := base
			cfg.Params = map[string]any{"args": tt.args}
			command, got := ResolveSpawn(cfg, workdir, prompt)
			if command != "pi" || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ResolveSpawn() = (%q, %#v), want (pi, %#v)", command, got, tt.want)
			}
			if !reflect.DeepEqual(tt.args, original) {
				t.Fatalf("configured args mutated: got %#v, want %#v", tt.args, original)
			}
		})
	}
}

func TestPiExecutionContractAndUnsupportedResume(t *testing.T) {
	workdir := t.TempDir()
	scriptPath := filepath.Join(t.TempDir(), "pi")
	observedPath := filepath.Join(t.TempDir(), "observed")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OBSERVED_PATH\"\nprintf 'env:%s\\n' \"$PWD\" >> \"$OBSERVED_PATH\"\nprintf 'dir:%s\\n' \"$(pwd)\" >> \"$OBSERVED_PATH\"\nprintf 'stderr line\\n' >&2\nprintf '__LOOPER_RESULT__={\"summary\":\"done\"}\\n'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	repos := storage.NewRepositories(openAgentCoordinator(t).DB())
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendorPi, NativeResumeEnabled: true, Params: map[string]any{"command": scriptPath}}, Repos: repos,
		ParamsOwnerVendor: piOwner(),
	})
	execution, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_pi", WorkingDirectory: workdir, Prompt: "fresh prompt", NativeResumePrompt: "resume prompt", NativeSessionID: "session-1", Timeout: 10 * time.Second, Env: map[string]string{"OBSERVED_PATH": observedPath}})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "completed" || result.Summary != "done" || !strings.Contains(result.Stdout, "__LOOPER_RESULT__") || !strings.Contains(result.Stderr, "stderr line") {
		t.Fatalf("result = %#v, want completed output capture and parsed marker", result)
	}
	observed, err := os.ReadFile(observedPath)
	if err != nil {
		t.Fatalf("read observed args: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(observed)), "\n")
	want := []string{"-p", "fresh prompt", "--approve", "env:" + workdir, "dir:" + workdir}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observed = %#v, want %#v", got, want)
	}
	if nativeResumeSupported(config.AgentVendorPi) || InteractiveTakeoverSupported(config.AgentVendorPi) {
		t.Fatal("Pi resume support must remain disabled")
	}
	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_pi")
	if err != nil || record == nil || record.NativeResumeMode == nil || *record.NativeResumeMode != "checkpoint_restart" || record.NativeResumeStatus == nil || *record.NativeResumeStatus != "unsupported" {
		t.Fatalf("native resume record = %#v, err = %v, want checkpoint_restart/unsupported", record, err)
	}
}

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

func agyOwner() *config.AgentVendor {
	v := config.AgentVendorAgy
	return &v
}

func TestResolveAgyArgs(t *testing.T) {
	model := "gemini-4-neon-high"
	base := ExecutorConfig{Vendor: config.AgentVendorAgy, Model: &model}
	workdir := "/tmp/looper-worktree"
	prompt := "generated prompt"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"default", nil, []string{"--model", model, "-p", prompt, "--dangerously-skip-permissions"}},
		{"configured model", []string{"--model", "custom"}, []string{"--model", "custom", "-p", prompt, "--dangerously-skip-permissions"}},
		{"equals model", []string{"--model=custom"}, []string{"--model=custom", "-p", prompt, "--dangerously-skip-permissions"}},
		{"short print owns prompt", []string{"-p", "custom"}, []string{"--model", model, "-p", "custom", "--dangerously-skip-permissions"}},
		{"long print owns prompt", []string{"--print", "custom"}, []string{"--model", model, "--print", "custom", "--dangerously-skip-permissions"}},
		{"print equals owns prompt", []string{"--print=custom"}, []string{"--model", model, "--print=custom", "--dangerously-skip-permissions"}},
		{"prompt alias owns prompt", []string{"--prompt", "custom"}, []string{"--model", model, "--prompt", "custom", "--dangerously-skip-permissions"}},
		{"prompt equals owns prompt", []string{"--prompt=custom"}, []string{"--model", model, "--prompt=custom", "--dangerously-skip-permissions"}},
		{"dangerously skip permissions", []string{"--dangerously-skip-permissions"}, []string{"--model", model, "--dangerously-skip-permissions", "-p", prompt}},
		{"mode", []string{"--mode", "auto"}, []string{"--model", model, "--mode", "auto", "-p", prompt}},
		{"mode equals", []string{"--mode=plan"}, []string{"--model", model, "--mode=plan", "-p", prompt}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]string(nil), tt.args...)
			cfg := base
			cfg.Params = map[string]any{"args": tt.args}
			command, got := ResolveSpawn(cfg, workdir, prompt)
			if command != "agy" || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ResolveSpawn() = (%q, %#v), want (agy, %#v)", command, got, tt.want)
			}
			if !reflect.DeepEqual(tt.args, original) {
				t.Fatalf("configured args mutated: got %#v, want %#v", tt.args, original)
			}
		})
	}
}

func TestAgyExecutionContractAndUnsupportedResume(t *testing.T) {
	workdir := t.TempDir()
	scriptPath := filepath.Join(t.TempDir(), "agy")
	observedPath := filepath.Join(t.TempDir(), "observed")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OBSERVED_PATH\"\nprintf 'env:%s\\n' \"$PWD\" >> \"$OBSERVED_PATH\"\nprintf 'dir:%s\\n' \"$(pwd)\" >> \"$OBSERVED_PATH\"\nprintf 'stderr line\\n' >&2\nprintf '__LOOPER_RESULT__={\"summary\":\"done\"}\\n'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	repos := storage.NewRepositories(openAgentCoordinator(t).DB())
	executor := New(ExecutorOptions{
		Config:            ExecutorConfig{Vendor: config.AgentVendorAgy, NativeResumeEnabled: true, Params: map[string]any{"command": scriptPath}},
		Repos:             repos,
		ParamsOwnerVendor: agyOwner(),
	})
	execution, err := executor.Start(context.Background(), RunInput{
		ExecutionID:        "agent_agy",
		WorkingDirectory:   workdir,
		Prompt:             "fresh prompt",
		NativeResumePrompt: "resume prompt",
		NativeSessionID:    "session-1",
		Timeout:            10 * time.Second,
		Env:                map[string]string{"OBSERVED_PATH": observedPath},
	})
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
	want := []string{"-p", "fresh prompt", "--dangerously-skip-permissions", "env:" + workdir, "dir:" + workdir}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observed = %#v, want %#v", got, want)
	}
	if nativeResumeSupported(config.AgentVendorAgy) || InteractiveTakeoverSupported(config.AgentVendorAgy) {
		t.Fatal("agy resume support must remain disabled")
	}
	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_agy")
	if err != nil || record == nil || record.NativeResumeMode == nil || *record.NativeResumeMode != "checkpoint_restart" || record.NativeResumeStatus == nil || *record.NativeResumeStatus != "unsupported" {
		t.Fatalf("native resume record = %#v, err = %v, want checkpoint_restart/unsupported", record, err)
	}
}

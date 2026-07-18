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

func TestResolveGrokArgs(t *testing.T) {
	model := "grok-4"
	base := ExecutorConfig{Vendor: config.AgentVendorGrokBuild, Model: &model}
	workdir := "/tmp/looper-worktree"
	prompt := "generated prompt"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"default", nil, []string{"--model", model, "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off", "--no-auto-update"}},
		{"configured model", []string{"-m", "custom"}, []string{"-m", "custom", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off"}},
		{"long model", []string{"--model", "custom"}, []string{"--model", "custom", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off"}},
		{"equals model", []string{"--model=custom"}, []string{"--model=custom", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off"}},
		{"short prompt", []string{"-p", "custom"}, []string{"--model", model, "-p", "custom", "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off"}},
		{"long prompt", []string{"--single", "custom"}, []string{"--model", model, "--single", "custom", "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off"}},
		{"prompt forms", []string{"--single=custom"}, []string{"--model", model, "--single=custom", "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off"}},
		{"cwd output sandbox", []string{"--cwd", "/operator", "--output-format", "json", "--sandbox", "none"}, []string{"--model", model, "--cwd", "/operator", "--output-format", "json", "--sandbox", "none", "-p", prompt, "--always-approve"}},
		{"cwd output sandbox equals", []string{"--cwd=/operator", "--output-format=json", "--sandbox=none"}, []string{"--model", model, "--cwd=/operator", "--output-format=json", "--sandbox=none", "-p", prompt, "--always-approve"}},
		{"configured workspace sandbox", []string{"--sandbox", "workspace"}, []string{"--model", model, "--sandbox", "workspace", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--always-approve"}},
		{"permission mode", []string{"--permission-mode", "ask"}, []string{"--model", model, "--permission-mode", "ask", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--sandbox", "off"}},
		{"permission mode equals", []string{"--permission-mode=ask"}, []string{"--model", model, "--permission-mode=ask", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--sandbox", "off"}},
		{"always approve", []string{"--always-approve"}, []string{"--model", model, "--always-approve", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--sandbox", "off"}},
		{"approval aliases", []string{"--yolo"}, []string{"--model", model, "--yolo", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--sandbox", "off"}},
		{"dangerous approval alias", []string{"--dangerously-skip-permissions"}, []string{"--model", model, "--dangerously-skip-permissions", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--sandbox", "off"}},
		{"prompt sources retain generated prompt", []string{"--prompt-file", "task.txt", "--prompt-json"}, []string{"--model", model, "--prompt-file", "task.txt", "--prompt-json", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off"}},
		{"configured no auto update", []string{"--no-auto-update"}, []string{"--model", model, "--no-auto-update", "-p", prompt, "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]string(nil), tt.args...)
			want := append([]string(nil), tt.want...)
			if !hasAnyFlag(want, []string{"--no-auto-update"}) {
				want = append(want, "--no-auto-update")
			}
			cfg := base
			cfg.Params = map[string]any{"args": tt.args}
			command, got := ResolveSpawn(cfg, workdir, prompt)
			if command != "grok" || !reflect.DeepEqual(got, want) {
				t.Fatalf("ResolveSpawn() = (%q, %#v), want (grok, %#v)", command, got, want)
			}
			if !reflect.DeepEqual(tt.args, original) {
				t.Fatalf("configured args mutated: got %#v, want %#v", tt.args, original)
			}
		})
	}
}

func TestGrokBuildExecutionContractAndUnsupportedResume(t *testing.T) {
	workdir := t.TempDir()
	scriptPath := filepath.Join(t.TempDir(), "grok")
	observedPath := filepath.Join(t.TempDir(), "observed")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OBSERVED_PATH\"\nprintf 'env:%s\\n' \"$PWD\" >> \"$OBSERVED_PATH\"\nprintf 'dir:%s\\n' \"$(pwd)\" >> \"$OBSERVED_PATH\"\nprintf 'stderr line\\n' >&2\nprintf '__LOOPER_RESULT__={\"summary\":\"done\"}\\n'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	repos := storage.NewRepositories(openAgentCoordinator(t).DB())
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendorGrokBuild, NativeResumeEnabled: true, Params: map[string]any{"command": scriptPath}}, Repos: repos,
		ParamsOwnerVendor: grokBuildOwner(),
	})
	execution, err := executor.Start(context.Background(), RunInput{ExecutionID: "agent_grok", WorkingDirectory: workdir, Prompt: "fresh prompt", NativeResumePrompt: "resume prompt", NativeSessionID: "session-1", Timeout: 10 * time.Second, Env: map[string]string{"OBSERVED_PATH": observedPath}})
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
	want := []string{"-p", "fresh prompt", "--cwd", workdir, "--output-format", "plain", "--always-approve", "--sandbox", "off", "--no-auto-update", "env:" + workdir, "dir:" + workdir}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observed = %#v, want %#v", got, want)
	}
	if nativeResumeSupported(config.AgentVendorGrokBuild) || InteractiveTakeoverSupported(config.AgentVendorGrokBuild) {
		t.Fatal("Grok Build resume support must remain disabled")
	}
	record, err := repos.AgentExecutions.GetByID(context.Background(), "agent_grok")
	if err != nil || record == nil || record.NativeResumeMode == nil || *record.NativeResumeMode != "checkpoint_restart" || record.NativeResumeStatus == nil || *record.NativeResumeStatus != "unsupported" {
		t.Fatalf("native resume record = %#v, err = %v, want checkpoint_restart/unsupported", record, err)
	}
}

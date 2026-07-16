package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	looperdapi "github.com/nexu-io/looper/internal/api"
	"github.com/nexu-io/looper/internal/config"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
)

func TestRuntimeConfigMetadataMarksHotOverridesReadOnly(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	loaded := config.LoadedFileConfig{
		Config: cfg,
		Metadata: config.LoadFileMetadata{
			ConfigPath:        "/tmp/config.toml",
			ConfigFilePresent: true,
			FieldSources: map[string]config.ValueSource{
				"agent.vendor":                           config.ValueSourceConfigFile,
				"agent.params.apiKey":                    config.ValueSourceConfigFile,
				"agent.params.nested.token":              config.ValueSourceConfigFile,
				"agent.env.OPENAI_API_KEY":               config.ValueSourceConfigFile,
				"daemon.environment.SECRET":              config.ValueSourceConfigFile,
				"server.localToken":                      config.ValueSourceConfigFile,
				"agent.timeouts.workerSeconds":           config.ValueSourceConfigFile,
				"roles.planner.triggers.planeAssigneeId": config.ValueSourceConfigFile,
				"roles.worker.triggers.planeAssigneeId":  config.ValueSourceConfigFile,
				"scheduler.maxConcurrentRuns":            config.ValueSourceEnv,
				"scheduler.pollIntervalSeconds":          config.ValueSourceDefault,
			},
		},
	}
	rt := looperdruntime.New(looperdruntime.Options{Config: cfg, InitialConfig: loaded})
	rtStatus := rt.ConfigReloadStatus()
	rtStatus.RejectedPaths = []string{"agent.params.apiKey", "agent.params.nested.token", "daemon.environment.SECRET", "server.localToken"}
	rtStatus.LastError = "configuration changes require a daemon restart: agent.params.apiKey, agent.params.nested.token, daemon.environment.SECRET, server.localToken"

	metadata := runtimeConfigMetadataFromStatus(rtStatus)
	if metadata.ConfigPath != "/tmp/config.toml" || metadata.Format != "toml" || !metadata.FilePresent {
		t.Fatalf("metadata source = %#v", metadata)
	}
	if field := metadata.Fields["agent.vendor"]; !field.Editable || field.ApplyMode != "hot" || field.Source != "config-file" {
		t.Fatalf("agent.vendor metadata = %#v, want editable hot config-file", field)
	}
	if field := metadata.Fields["scheduler.maxConcurrentRuns"]; field.Editable || field.ApplyMode != "hot" || field.Source != "env" {
		t.Fatalf("env-owned scheduler metadata = %#v, want read-only hot env", field)
	}
	if field := metadata.Fields["scheduler.pollIntervalSeconds"]; field.Editable || field.ApplyMode != "restart" {
		t.Fatalf("poll interval metadata = %#v, want read-only restart", field)
	}
	if field := metadata.Fields["roles.planner.triggers.planeAssigneeId"]; field.Editable || field.ApplyMode != "restart" {
		t.Fatalf("planner Plane assignee metadata = %#v, want read-only restart", field)
	}
	if field := metadata.Fields["roles.worker.triggers.planeAssigneeId"]; !field.Editable || field.ApplyMode != "hot" {
		t.Fatalf("worker Plane assignee metadata = %#v, want editable hot", field)
	}
	if field := metadata.Fields["agent.env.OPENAI_API_KEY"]; !field.Editable {
		t.Fatalf("write-only agent env metadata = %#v, want editable key", field)
	}
	for _, path := range []string{"agent.params.apiKey", "agent.params.nested.token", "daemon.environment.SECRET", "server.localToken"} {
		if _, exists := metadata.Fields[path]; exists {
			t.Fatalf("metadata exposed file-only path %q: %#v", path, metadata.Fields)
		}
		if metadata.LastError != nil && strings.Contains(*metadata.LastError, path) {
			t.Fatalf("metadata error exposed file-only path %q: %s", path, *metadata.LastError)
		}
	}
	if _, exists := metadata.Fields["agent.timeouts.workerSeconds"]; exists {
		t.Fatalf("metadata exposed deprecated compatibility alias: %#v", metadata.Fields["agent.timeouts.workerSeconds"])
	}
	if got, want := metadata.RejectedPaths, []string{"agent.params", "daemon.environment", "server.localToken"}; !slices.Equal(got, want) {
		t.Fatalf("metadata rejected paths = %#v, want collapsed %#v", got, want)
	}
}

func TestRuntimeConfigMetadataHidesUnstructuredDecodeErrors(t *testing.T) {
	const secret = "SUPER_SECRET_VALUE"
	metadata := runtimeConfigMetadataFromStatus(looperdruntime.ConfigReloadStatus{
		LastError: "yaml: invalid map key containing " + secret,
	})
	if metadata.LastError == nil {
		t.Fatal("metadata.LastError = nil, want safe diagnostic")
	}
	if strings.Contains(*metadata.LastError, secret) || strings.Contains(*metadata.LastError, "invalid map key") {
		t.Fatalf("metadata.LastError = %q, exposed unstructured decode details", *metadata.LastError)
	}
}

func TestPatchRuntimeConfigMapsValidationAndStartupOnlyErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"notifications":{"osascript":{"enabled":false}},"scheduler":{"maxConcurrentRuns":2}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	loadAt := func(selected string) (config.LoadedFileConfig, error) {
		return config.LoadFile(config.LoadFileOptions{
			ConfigPathOverride: selected,
			LookupEnv:          func(string) (string, bool) { return "", false },
		})
	}
	initial, err := loadAt(path)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	rt := looperdruntime.New(looperdruntime.Options{
		Config:        initial.Config,
		ConfigPath:    path,
		InitialConfig: initial,
		ReloadConfig:  func() (config.LoadedFileConfig, error) { return loadAt(path) },
		LoadConfigAt:  loadAt,
	})

	tests := []struct {
		name      string
		path      string
		value     json.RawMessage
		wantKind  looperdapi.ConfigRequestErrorKind
		wantIssue string
	}{
		{name: "invalid", path: "scheduler.maxConcurrentRuns", value: json.RawMessage("0"), wantKind: looperdapi.ConfigRequestErrorKindValidation, wantIssue: "invalid_value"},
		{name: "startup only", path: "server.port", value: json.RawMessage("19000"), wantKind: looperdapi.ConfigRequestErrorKindUnsupported, wantIssue: "field_not_editable"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := patchRuntimeConfig(context.Background(), rt, looperdapi.ConfigPatchRequest{Set: map[string]json.RawMessage{testCase.path: testCase.value}})
			var requestError looperdapi.ConfigRequestError
			if !errors.As(err, &requestError) {
				t.Fatalf("error = %T %v, want ConfigRequestError", err, err)
			}
			if requestError.Kind != testCase.wantKind || len(requestError.Issues) == 0 || requestError.Issues[0].Code != testCase.wantIssue {
				t.Fatalf("request error = %#v", requestError)
			}
		})
	}
}

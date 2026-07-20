package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestPatchConfigPersistsValidatedFieldsAndPublishesImmediately(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte("# operator comment\nx-extension:\n  keep: true\nnotifications:\n  osascript:\n    enabled: false\nscheduler:\n  maxConcurrentRuns: 2\ndefaults:\n  allowAutoPush: false\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	rt := newConfigPatchRuntime(t, path, func(string) (string, bool) { return "", false })

	err := rt.PatchConfig(context.Background(), ConfigPatch{Revision: testConfigRevision(t, path), Set: map[string]json.RawMessage{
		"scheduler.maxConcurrentRuns": json.RawMessage("4"),
		"defaults.allowAutoPush":      json.RawMessage("true"),
		"agent.env.OPENAI_API_KEY":    json.RawMessage(`"new-secret"`),
	}})
	if err != nil {
		t.Fatalf("PatchConfig() error = %v", err)
	}

	got := rt.Config()
	if got.Scheduler.MaxConcurrentRuns != 4 || !got.Defaults.AllowAutoPush {
		t.Fatalf("Config() did not publish patch: scheduler=%d allowAutoPush=%v", got.Scheduler.MaxConcurrentRuns, got.Defaults.AllowAutoPush)
	}
	if got.Agent.Env["OPENAI_API_KEY"] != "new-secret" {
		t.Fatalf("Config().Agent.Env was not updated")
	}
	snapshot, status := rt.ConfigSnapshot()
	publishedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Scheduler.MaxConcurrentRuns != 4 || status.Revision != config.ConfigFileRevision(publishedRaw, true) {
		t.Fatalf("ConfigSnapshot() = maxConcurrent %d, revision %q; want published values and file revision", snapshot.Scheduler.MaxConcurrentRuns, status.Revision)
	}
	partial, present, err := config.ReadPartialConfigFile(path)
	if err != nil || !present {
		t.Fatalf("ReadPartialConfigFile() = present %v, err %v", present, err)
	}
	if partial.Scheduler == nil || partial.Scheduler.MaxConcurrentRuns == nil || *partial.Scheduler.MaxConcurrentRuns != 4 {
		t.Fatalf("persisted scheduler = %#v", partial.Scheduler)
	}
	if partial.Agent == nil || partial.Agent.Env["OPENAI_API_KEY"] != "new-secret" {
		t.Fatalf("persisted agent env keys = %#v", partial.Agent)
	}
	if partial.Defaults == nil || partial.Defaults.AllowAutoPush == nil || !*partial.Defaults.AllowAutoPush {
		t.Fatalf("persisted defaults = %#v", partial.Defaults)
	}
	persistedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(persistedRaw, []byte("x-extension:")) || !bytes.Contains(persistedRaw, []byte("keep: true")) {
		t.Fatalf("dashboard patch dropped unknown top-level extension:\n%s", persistedRaw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", info.Mode().Perm())
	}

	if err := rt.PatchConfig(context.Background(), ConfigPatch{Revision: testConfigRevision(t, path), Unset: []string{"agent.env.OPENAI_API_KEY"}}); err != nil {
		t.Fatalf("PatchConfig(unset env) error = %v", err)
	}
	if _, exists := rt.Config().Agent.Env["OPENAI_API_KEY"]; exists {
		t.Fatal("Config().Agent.Env retained removed write-only value")
	}
}

func TestPatchConfigRejectsStaleRevisionAndInheritedAuthority(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{"notifications":{"osascript":{"enabled":false}},"agent":{"env":{"OLD":"value"}}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newConfigPatchRuntime(t, path, func(string) (string, bool) { return "", false })
	staleRevision := config.ConfigFileRevision(original, true)
	external := []byte(`{"notifications":{"osascript":{"enabled":false}},"agent":{"env":{"OLD":"external"}}}`)
	if err := os.WriteFile(path, external, 0o600); err != nil {
		t.Fatal(err)
	}
	err := rt.PatchConfig(context.Background(), ConfigPatch{Revision: staleRevision, Set: map[string]json.RawMessage{"agent.model": json.RawMessage(`"gpt-5"`)}})
	var patchErr *ConfigPatchError
	if !errors.As(err, &patchErr) || patchErr.Kind != "conflict" {
		t.Fatalf("stale PatchConfig() error = %#v, want conflict", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, external) {
		t.Fatalf("stale patch overwrote external edit: %s", got)
	}

	rt.loadedConfig.Metadata.FieldSources["agent.env"] = config.ValueSourceEnv
	err = rt.PatchConfig(context.Background(), ConfigPatch{Revision: config.ConfigFileRevision(external, true), Set: map[string]json.RawMessage{"agent.env.NEW": json.RawMessage(`"secret"`)}})
	if !errors.As(err, &patchErr) || patchErr.Kind != "unsupported" {
		t.Fatalf("inherited-authority PatchConfig() error = %#v, want unsupported", err)
	}
}

func TestConfigSnapshotRevisionStaysBoundToPublishedGeneration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	initial := []byte(`{"notifications":{"osascript":{"enabled":false}},"scheduler":{"maxConcurrentRuns":2}}`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newConfigPatchRuntime(t, path, func(string) (string, bool) { return "", false })

	external := []byte(`{"notifications":{"osascript":{"enabled":false}},"scheduler":{"maxConcurrentRuns":7}}`)
	if err := os.WriteFile(path, external, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, status := rt.ConfigSnapshot()
	if snapshot.Scheduler.MaxConcurrentRuns != 2 {
		t.Fatalf("ConfigSnapshot().MaxConcurrentRuns = %d, want last-published 2", snapshot.Scheduler.MaxConcurrentRuns)
	}
	if got, want := status.Revision, config.ConfigFileRevision(initial, true); got != want {
		t.Fatalf("ConfigSnapshot().Revision = %q, want published generation %q", got, want)
	}

	err := rt.PatchConfig(context.Background(), ConfigPatch{
		Revision: status.Revision,
		Set:      map[string]json.RawMessage{"scheduler.maxConcurrentRuns": json.RawMessage("5")},
	})
	var patchErr *ConfigPatchError
	if !errors.As(err, &patchErr) || patchErr.Kind != "conflict" {
		t.Fatalf("PatchConfig() error = %#v, want conflict with unseen external generation", err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(got, external) {
		t.Fatalf("conflicting patch changed unseen external edit: got %q, err %v", got, readErr)
	}

	if err := rt.ReloadConfig(context.Background()); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	snapshot, status = rt.ConfigSnapshot()
	if snapshot.Scheduler.MaxConcurrentRuns != 7 || status.Revision != config.ConfigFileRevision(external, true) {
		t.Fatalf("published external generation = max %d revision %q", snapshot.Scheduler.MaxConcurrentRuns, status.Revision)
	}
}

func TestPatchConfigCanonicalFieldRetiresShadowingLegacyAliases(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{
  "notifications":{"osascript":{"enabled":false}},
  "agent":{"timeouts":{"workerSeconds":600}},
  "defaults":{"allowAutoApprove":true},
  "reviewer":{"reviewEvents":{"clean":"APPROVE","blocking":"REQUEST_CHANGES"}}
}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newConfigPatchRuntime(t, path, func(string) (string, bool) { return "", false })
	err := rt.PatchConfig(context.Background(), ConfigPatch{
		Revision: testConfigRevision(t, path),
		Set: map[string]json.RawMessage{
			"agent.timeouts.workerMaxRuntimeSeconds":     json.RawMessage("700"),
			"roles.reviewer.behavior.reviewEvents.clean": json.RawMessage(`"COMMENT"`),
		},
	})
	if err != nil {
		t.Fatalf("PatchConfig() error = %v", err)
	}
	got := rt.Config()
	if got.Agent.Timeouts.WorkerSeconds != 700 || got.Agent.Timeouts.WorkerMaxRuntimeSeconds != 700 {
		t.Fatalf("worker timeout aliases = legacy %d canonical %d, want 700", got.Agent.Timeouts.WorkerSeconds, got.Agent.Timeouts.WorkerMaxRuntimeSeconds)
	}
	if got.Roles.Reviewer.Behavior.ReviewEvents.Clean != config.ReviewerReviewEventComment {
		t.Fatalf("reviewer clean event = %q, want COMMENT", got.Roles.Reviewer.Behavior.ReviewEvents.Clean)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(persisted, &document); err != nil {
		t.Fatalf("decode persisted config: %v\n%s", err, persisted)
	}
	timeouts := document["agent"].(map[string]any)["timeouts"].(map[string]any)
	if _, exists := timeouts["workerSeconds"]; exists {
		t.Fatalf("persisted config retained workerSeconds alias:\n%s", persisted)
	}
	defaults := document["defaults"].(map[string]any)
	if _, exists := defaults["allowAutoApprove"]; exists {
		t.Fatalf("persisted config retained allowAutoApprove alias:\n%s", persisted)
	}
	legacyEvents := document["reviewer"].(map[string]any)["reviewEvents"].(map[string]any)
	if _, exists := legacyEvents["clean"]; exists {
		t.Fatalf("persisted config retained reviewer.reviewEvents.clean alias:\n%s", persisted)
	}
	if !bytes.Contains(persisted, []byte(`"blocking": "REQUEST_CHANGES"`)) {
		t.Fatalf("persisted config dropped unrelated legacy field:\n%s", persisted)
	}

	if err := rt.PatchConfig(context.Background(), ConfigPatch{
		Revision: testConfigRevision(t, path),
		Unset:    []string{"roles.reviewer.behavior.reviewEvents.clean"},
	}); err != nil {
		t.Fatalf("PatchConfig(unset canonical) error = %v", err)
	}
	if got := rt.Config().Roles.Reviewer.Behavior.ReviewEvents.Clean; got != config.ReviewerReviewEventApprove {
		t.Fatalf("clean event after canonical unset = %q, want default APPROVE", got)
	}
}

func TestPatchConfigVendorCompanionModelMessageIsNotRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"agent":{"vendor":"codex","model":"gpt-5"},"scheduler":{"maxConcurrentRuns":2}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	rt := newConfigPatchRuntime(t, path, nil)
	err := rt.PatchConfig(context.Background(), ConfigPatch{
		Revision: testConfigRevision(t, path),
		Set:      map[string]json.RawMessage{"agent.vendor": json.RawMessage(`"claude-code"`)},
	})
	var patchErr *ConfigPatchError
	if !errors.As(err, &patchErr) {
		t.Fatalf("PatchConfig() error = %v, want ConfigPatchError", err)
	}
	if patchErr.Kind != "validation" {
		t.Fatalf("PatchConfig() kind = %q, want validation (companion, not restart)", patchErr.Kind)
	}
	if !strings.Contains(patchErr.Message, "companion fields") || strings.Contains(patchErr.Message, "daemon restart") {
		t.Fatalf("PatchConfig() message = %q, want companion-field guidance without restart claim", patchErr.Message)
	}
	if got, want := patchErr.Paths, []string{"agent.model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PatchConfig() paths = %#v, want %#v", got, want)
	}
}

func TestPatchConfigRejectsShadowedUnsupportedAndInvalidFieldsWithoutWriting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"notifications":{"osascript":{"enabled":false}},"defaults":{"allowAutoPush":true},"scheduler":{"maxConcurrentRuns":2}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	lookup := func(key string) (string, bool) {
		if key == "LOOPER_ALLOW_AUTO_PUSH" {
			return "false", true
		}
		return "", false
	}
	rt := newConfigPatchRuntime(t, path, lookup)

	tests := []struct {
		name  string
		patch ConfigPatch
		kind  string
	}{
		{name: "env shadowed", patch: ConfigPatch{Set: map[string]json.RawMessage{"defaults.allowAutoPush": json.RawMessage("true")}}, kind: "unsupported"},
		{name: "restart bound", patch: ConfigPatch{Set: map[string]json.RawMessage{"server.port": json.RawMessage("18000")}}, kind: "unsupported"},
		{name: "invalid value", patch: ConfigPatch{Set: map[string]json.RawMessage{"scheduler.maxConcurrentRuns": json.RawMessage("0")}}, kind: "validation"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(before) error = %v", err)
			}
			testCase.patch.Revision = config.ConfigFileRevision(before, true)
			err = rt.PatchConfig(context.Background(), testCase.patch)
			var patchErr *ConfigPatchError
			if !errors.As(err, &patchErr) || patchErr.Kind != testCase.kind {
				t.Fatalf("PatchConfig() error = %#v, want kind %q", err, testCase.kind)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(after) error = %v", err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("rejected patch changed file:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func testConfigRevision(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(revision) error = %v", err)
	}
	return config.ConfigFileRevision(raw, true)
}

func newConfigPatchRuntime(t *testing.T, path string, lookup config.EnvLookupFunc) *Runtime {
	t.Helper()
	loadAt := func(selected string) (config.LoadedFileConfig, error) {
		return config.LoadFile(config.LoadFileOptions{ConfigPathOverride: selected, LookupEnv: lookup})
	}
	initial, err := loadAt(path)
	if err != nil {
		t.Fatalf("LoadFile(initial) error = %v", err)
	}
	return New(Options{
		Config:        initial.Config,
		ConfigPath:    path,
		InitialConfig: initial,
		ReloadConfig: func() (config.LoadedFileConfig, error) {
			return loadAt(path)
		},
		LoadConfigAt: loadAt,
	})
}

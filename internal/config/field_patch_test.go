package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestValidateFieldPatchOperationsScalesAcrossIndependentMapKeys(t *testing.T) {
	t.Parallel()

	set := make(map[string]json.RawMessage, 20_000)
	for index := 0; index < 20_000; index++ {
		set[fmt.Sprintf("agent.env.KEY_%05d", index)] = json.RawMessage(`"value"`)
	}
	operations, err := validateFieldPatchOperations(set, nil)
	if err != nil {
		t.Fatalf("validateFieldPatchOperations() error = %v", err)
	}
	if len(operations) != len(set) {
		t.Fatalf("operations = %d, want %d", len(operations), len(set))
	}
}

func TestApplyFieldPatchPreservesUnrelatedConfig(t *testing.T) {
	t.Parallel()

	host := "127.0.0.1"
	port := 17310
	allowPush := true
	labels := []string{"old"}
	base := PartialConfig{
		Server:   &PartialServerConfig{Host: &host, Port: &port},
		Defaults: &PartialDefaultsConfig{AllowAutoPush: &allowPush},
		Agent: &PartialAgentConfig{
			Env:    map[string]string{"TOKEN": "old", "REMOVE": "gone"},
			Params: map[string]any{"nested": map[string]any{"value": "old"}},
		},
		Roles: &PartialRoleConfigs{Planner: &PartialPlannerRoleConfig{Triggers: &PartialIssueRoleTriggersConfig{Labels: &labels}}},
	}

	patched, err := ApplyFieldPatch(base, map[string]json.RawMessage{
		"defaults.allowAutoPush":        json.RawMessage(`false`),
		"agent.env.TOKEN":               json.RawMessage(`"new"`),
		"agent.params.nested.value":     json.RawMessage(`"changed"`),
		"roles.planner.triggers.labels": json.RawMessage(`["one","two"]`),
	}, []string{"server.host", "agent.env.REMOVE"})
	if err != nil {
		t.Fatalf("ApplyFieldPatch() error = %v", err)
	}

	if patched.Server == nil || patched.Server.Host != nil || patched.Server.Port == nil || *patched.Server.Port != port {
		t.Fatalf("patched.Server = %#v, want only host unset", patched.Server)
	}
	if patched.Defaults == nil || patched.Defaults.AllowAutoPush == nil || *patched.Defaults.AllowAutoPush {
		t.Fatalf("patched.Defaults = %#v, want allowAutoPush false", patched.Defaults)
	}
	if patched.Agent == nil || patched.Agent.Env["TOKEN"] != "new" {
		t.Fatalf("patched.Agent.Env = %#v, want TOKEN replaced", patched.Agent)
	}
	if _, exists := patched.Agent.Env["REMOVE"]; exists {
		t.Fatalf("patched.Agent.Env = %#v, want REMOVE deleted", patched.Agent.Env)
	}
	nested := patched.Agent.Params["nested"].(map[string]any)
	if nested["value"] != "changed" {
		t.Fatalf("patched.Agent.Params = %#v, want nested patch", patched.Agent.Params)
	}
	if patched.Roles == nil || patched.Roles.Planner == nil || patched.Roles.Planner.Triggers == nil || patched.Roles.Planner.Triggers.Labels == nil || !reflect.DeepEqual(*patched.Roles.Planner.Triggers.Labels, []string{"one", "two"}) {
		t.Fatalf("patched roles = %#v, want replaced labels", patched.Roles)
	}

	if base.Server.Host == nil || *base.Server.Host != host || base.Agent.Env["TOKEN"] != "old" || base.Agent.Params["nested"].(map[string]any)["value"] != "old" || !reflect.DeepEqual(*base.Roles.Planner.Triggers.Labels, []string{"old"}) {
		t.Fatalf("ApplyFieldPatch() mutated base: %#v", base)
	}
}

func TestApplyFieldPatchCreatesAgentEnvEntry(t *testing.T) {
	t.Parallel()

	patched, err := ApplyFieldPatch(PartialConfig{}, map[string]json.RawMessage{
		"agent.env.NEW_TOKEN": json.RawMessage(`"secret"`),
	}, nil)
	if err != nil {
		t.Fatalf("ApplyFieldPatch() error = %v", err)
	}
	if patched.Agent == nil || patched.Agent.Env["NEW_TOKEN"] != "secret" {
		t.Fatalf("patched.Agent = %#v, want new environment entry", patched.Agent)
	}
}

func TestIsFieldLevelConfigPathRejectsCompositeOperations(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"agent.model", "agent.env.TOKEN", "agent.timeouts.workerSeconds", "roles.planner.triggers.labels"} {
		if !IsFieldLevelConfigPath(path) {
			t.Fatalf("IsFieldLevelConfigPath(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"agent", "agent.env", "agent.timeouts", "roles", "roles.planner", "roles.planner.triggers"} {
		if IsFieldLevelConfigPath(path) {
			t.Fatalf("IsFieldLevelConfigPath(%q) = true, want false", path)
		}
	}
}

func TestApplyFieldPatchRejectsInvalidOperations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		base    PartialConfig
		set     map[string]json.RawMessage
		unset   []string
		message string
	}{
		{name: "empty path", set: map[string]json.RawMessage{"": json.RawMessage(`true`)}, message: "path must not be empty"},
		{name: "empty segment", set: map[string]json.RawMessage{"agent..model": json.RawMessage(`"x"`)}, message: "empty segment"},
		{name: "unknown field", set: map[string]json.RawMessage{"agent.unknown": json.RawMessage(`true`)}, message: "unknown config field"},
		{name: "wrong case", set: map[string]json.RawMessage{"Agent.model": json.RawMessage(`"x"`)}, message: "unknown config field"},
		{name: "inside list", set: map[string]json.RawMessage{"notifications.osascript.soundForLevels.0": json.RawMessage(`"failure"`)}, message: "complete list"},
		{name: "inside scalar map value", set: map[string]json.RawMessage{"agent.env.TOKEN.extra": json.RawMessage(`"x"`)}, message: "scalar config field"},
		{name: "invalid json", set: map[string]json.RawMessage{"agent.model": json.RawMessage(`{"bad"`)}, message: "invalid JSON value"},
		{name: "trailing json", set: map[string]json.RawMessage{"agent.model": json.RawMessage(`"one" "two"`)}, message: "one JSON value"},
		{name: "wrong value type", set: map[string]json.RawMessage{"defaults.allowAutoPush": json.RawMessage(`"yes"`)}, message: "cannot unmarshal string"},
		{name: "duplicate unset", unset: []string{"agent.model", "agent.model"}, message: "duplicate unset path"},
		{name: "set and unset", set: map[string]json.RawMessage{"agent.model": json.RawMessage(`"x"`)}, unset: []string{"agent.model"}, message: "both set and unset"},
		{name: "parent child set", set: map[string]json.RawMessage{"agent.env": json.RawMessage(`{}`), "agent.env.TOKEN": json.RawMessage(`"x"`)}, message: "conflicting patch paths"},
		{name: "parent unset child set", set: map[string]json.RawMessage{"roles.planner.autoDiscovery": json.RawMessage(`true`)}, unset: []string{"roles.planner"}, message: "conflicting patch paths"},
		{
			name:    "existing dynamic scalar cannot be descended",
			base:    PartialConfig{Agent: &PartialAgentConfig{Params: map[string]any{"value": "scalar"}}},
			set:     map[string]json.RawMessage{"agent.params.value.child": json.RawMessage(`true`)},
			message: "non-object value",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ApplyFieldPatch(test.base, test.set, test.unset)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("ApplyFieldPatch() error = %v, want message containing %q", err, test.message)
			}
		})
	}
}

func TestApplyFieldPatchValidUnsetMissingFieldIsNoop(t *testing.T) {
	t.Parallel()

	patched, err := ApplyFieldPatch(PartialConfig{}, nil, []string{"agent.model"})
	if err != nil {
		t.Fatalf("ApplyFieldPatch() error = %v", err)
	}
	if !reflect.DeepEqual(patched, PartialConfig{}) {
		t.Fatalf("ApplyFieldPatch() = %#v, want empty config", patched)
	}
}

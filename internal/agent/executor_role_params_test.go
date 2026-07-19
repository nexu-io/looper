package agent

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestParamsForRoleVendorStripsCrossVendorIdentity(t *testing.T) {
	t.Parallel()

	global := config.AgentVendorCodex
	roleModel := "role-model"
	params := map[string]any{
		"command": "/opt/codex-wrapper",
		"args":    []any{"exec", "--model", "params-model", "--sandbox", "workspace-write"},
		"keep":    "yes",
	}

	// Same vendor + resolved model: keep command wrapper and non-model args;
	// strip model flags so role/profile model can win. Clone — do not mutate
	// the global params map.
	same := ParamsForRoleVendor(params, &global, config.AgentVendorCodex, &roleModel)
	if same == nil || same["command"] != "/opt/codex-wrapper" {
		t.Fatalf("same-vendor params = %#v, want original command", same)
	}
	sameJoined := strings.Join(stringArgs(same["args"]), " ")
	if strings.Contains(sameJoined, "params-model") || strings.Contains(sameJoined, "--model") {
		t.Fatalf("same-vendor args = %q, want model flags stripped when role model set", sameJoined)
	}
	if !strings.Contains(sameJoined, "exec") || !strings.Contains(sameJoined, "--sandbox") {
		t.Fatalf("same-vendor args = %q, want non-model args preserved", sameJoined)
	}
	same["probe"] = true
	if _, ok := params["probe"]; ok {
		t.Fatal("same-vendor must clone params; mutated shared map")
	}
	// Original map must retain model flags.
	origJoined := strings.Join(stringArgs(params["args"]), " ")
	if !strings.Contains(origJoined, "params-model") {
		t.Fatal("ParamsForRoleVendor mutated source params.args model flags")
	}

	// Same vendor + no resolved model: preserve params.args model flags.
	keepModel := ParamsForRoleVendor(params, &global, config.AgentVendorCodex, nil)
	if keepModel == nil || keepModel["command"] != "/opt/codex-wrapper" {
		t.Fatalf("no-model same-vendor params = %#v, want original command", keepModel)
	}
	keepJoined := strings.Join(stringArgs(keepModel["args"]), " ")
	if !strings.Contains(keepJoined, "params-model") || !strings.Contains(keepJoined, "--model") {
		t.Fatalf("no-model same-vendor args = %q, want params model preserved", keepJoined)
	}
	keepModel["probe2"] = true
	if _, ok := params["probe2"]; ok {
		t.Fatal("no-model same-vendor must clone params; mutated shared map")
	}

	// Empty resolved model string is explicit suppress → strip params model so
	// the vendor default wins (do not treat the same as unset/nil).
	emptyModel := ""
	emptyStrip := ParamsForRoleVendor(params, &global, config.AgentVendorCodex, &emptyModel)
	emptyJoined := strings.Join(stringArgs(emptyStrip["args"]), " ")
	if strings.Contains(emptyJoined, "params-model") || strings.Contains(emptyJoined, "--model") {
		t.Fatalf("empty-model same-vendor args = %q, want model flags stripped for suppress", emptyJoined)
	}
	if !strings.Contains(emptyJoined, "exec") || !strings.Contains(emptyJoined, "--sandbox") {
		t.Fatalf("empty-model same-vendor args = %q, want non-model args preserved", emptyJoined)
	}

	// Diverged role vendor: drop command + all args (vendor-shaped); keep other keys.
	diverged := ParamsForRoleVendor(params, &global, config.AgentVendorClaudeCode, &roleModel)
	if _, ok := diverged["command"]; ok {
		t.Fatalf("diverged role still has command: %#v", diverged)
	}
	if _, ok := diverged["args"]; ok {
		t.Fatalf("diverged role still has args: %#v", diverged)
	}
	if diverged["keep"] != "yes" {
		t.Fatalf("diverged role dropped non-identity param: %#v", diverged)
	}
	// Original map must not be mutated.
	if params["command"] != "/opt/codex-wrapper" {
		t.Fatal("ParamsForRoleVendor mutated source params.command")
	}

	// No global vendor: params have no home vendor; strip vendor-owned for any role.
	noGlobal := ParamsForRoleVendor(params, nil, config.AgentVendorClaudeCode, nil)
	if _, ok := noGlobal["command"]; ok {
		t.Fatalf("nil global vendor still has command: %#v", noGlobal)
	}
	if _, ok := noGlobal["args"]; ok {
		t.Fatalf("nil global vendor still has args: %#v", noGlobal)
	}

	// Nil params stay nil.
	if ParamsForRoleVendor(nil, &global, config.AgentVendorClaudeCode, nil) != nil {
		t.Fatal("nil params should stay nil")
	}

	// Role with stripped params must resolve to the role vendor binary, not the wrapper.
	cfg := ExecutorConfig{
		Vendor: config.AgentVendorClaudeCode,
		Params: ParamsForRoleVendor(params, &global, config.AgentVendorClaudeCode, nil),
	}
	if cmd := resolveCommand(cfg); cmd != "claude" {
		t.Fatalf("resolveCommand(diverged role) = %q, want claude", cmd)
	}
}

func TestParamsForRoleVendorSameVendorRoleModelWinsOverParamsModelFlag(t *testing.T) {
	t.Parallel()

	global := config.AgentVendorCodex
	roleModel := "role-model"
	params := map[string]any{
		"args": []any{"exec", "--model", "params-model", "--sandbox", "workspace-write"},
	}
	cfg := ExecutorConfig{
		Vendor: config.AgentVendorCodex,
		Model:  &roleModel,
		Params: ParamsForRoleVendor(params, &global, config.AgentVendorCodex, &roleModel),
	}
	_, args := ResolveSpawn(cfg, "/tmp/wt", "hello")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "params-model") {
		t.Fatalf("args = %q, want params model flag stripped so role model wins", joined)
	}
	if !strings.Contains(joined, "role-model") {
		t.Fatalf("args = %q, want role model", joined)
	}
	if !strings.Contains(joined, "--sandbox") {
		t.Fatalf("args = %q, want same-vendor non-model args preserved", joined)
	}
}

func TestParamsForRoleVendorSameVendorPreservesParamsModelWhenNoResolvedModel(t *testing.T) {
	t.Parallel()

	global := config.AgentVendorCodex
	params := map[string]any{
		"args": []any{"exec", "--model", "params-model", "--sandbox", "workspace-write"},
	}
	cfg := ExecutorConfig{
		Vendor: config.AgentVendorCodex,
		Model:  nil,
		Params: ParamsForRoleVendor(params, &global, config.AgentVendorCodex, nil),
	}
	_, args := ResolveSpawn(cfg, "/tmp/wt", "hello")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "params-model") {
		t.Fatalf("args = %q, want params model preserved when no resolved model", joined)
	}
	if !strings.Contains(joined, "--sandbox") {
		t.Fatalf("args = %q, want same-vendor non-model args preserved", joined)
	}
}

func TestParamsForRoleVendorSameVendorEmptyModelSuppressesParamsModelFlag(t *testing.T) {
	t.Parallel()

	global := config.AgentVendorCodex
	emptyModel := ""
	params := map[string]any{
		"args": []any{"exec", "--model", "params-model", "--sandbox", "workspace-write"},
	}
	cfg := ExecutorConfig{
		Vendor: config.AgentVendorCodex,
		Model:  &emptyModel,
		Params: ParamsForRoleVendor(params, &global, config.AgentVendorCodex, &emptyModel),
	}
	_, args := ResolveSpawn(cfg, "/tmp/wt", "hello")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "params-model") {
		t.Fatalf("args = %q, want params model stripped so vendor default wins", joined)
	}
	if strings.Contains(joined, "--model") || strings.Contains(joined, "-m") {
		t.Fatalf("args = %q, want no model flags after empty-model suppress", joined)
	}
	if !strings.Contains(joined, "--sandbox") {
		t.Fatalf("args = %q, want same-vendor non-model args preserved", joined)
	}
}

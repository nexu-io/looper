package agent

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestEffectiveConfigUsesSnapshotOverrides(t *testing.T) {
	t.Parallel()

	configModel := "config-model"
	snapshotModel := "snapshot-model"
	owner := config.AgentVendorClaudeCode
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{
			Vendor: config.AgentVendorClaudeCode,
			Model:  &configModel,
			Params: map[string]any{"args": []any{"--print"}},
			Env:    map[string]string{"KEEP": "1"},
		},
		ParamsOwnerVendor: &owner,
	})

	// No snapshot: keep executor config.
	got := executor.effectiveConfig(RunInput{})
	if got.Vendor != config.AgentVendorClaudeCode || got.Model == nil || *got.Model != configModel {
		t.Fatalf("effectiveConfig(no snapshot) = %#v", got)
	}

	// Snapshot overrides vendor/model for spawn only; env stays from config.
	got = executor.effectiveConfig(RunInput{
		UseSnapshot:    true,
		SnapshotVendor: string(config.AgentVendorCodex),
		SnapshotModel:  &snapshotModel,
	})
	if got.Vendor != config.AgentVendorCodex {
		t.Fatalf("Vendor = %q, want codex", got.Vendor)
	}
	if got.Model == nil || *got.Model != snapshotModel {
		t.Fatalf("Model = %v, want %q", got.Model, snapshotModel)
	}
	if got.Env["KEEP"] != "1" {
		t.Fatalf("Env not preserved: %#v", got.Env)
	}

	// Snapshot with nil model clears model (no model flag).
	got = executor.effectiveConfig(RunInput{
		UseSnapshot:    true,
		SnapshotVendor: string(config.AgentVendorOpenCode),
		SnapshotModel:  nil,
	})
	if got.Vendor != config.AgentVendorOpenCode || got.Model != nil {
		t.Fatalf("effectiveConfig(nil model) = %#v", got)
	}

	// ResolveSpawn path uses override vendor/model.
	command, args := ResolveSpawn(got, "/tmp/wt", "hello")
	if command != "opencode" {
		t.Fatalf("command = %q, want opencode", command)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--model") {
		t.Fatalf("args = %q, want no --model when snapshot model is nil", joined)
	}
}

func TestEffectiveConfigStripsIdentityParamsUnderSnapshot(t *testing.T) {
	t.Parallel()

	configModel := "config-model"
	snapshotModel := "frozen-model"
	owner := config.AgentVendorClaudeCode
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{
			Vendor: config.AgentVendorClaudeCode,
			Model:  &configModel,
			Params: map[string]any{
				"command": "custom-agent-bin",
				"args":    []any{"--model", "params-model", "--print", "-m", "also-params", "--other"},
			},
		},
		ParamsOwnerVendor: &owner,
	})

	// Without snapshot, same-vendor owner: command/args params still apply;
	// resolved model strips params model flags.
	got := executor.effectiveConfig(RunInput{})
	if cmd := resolveCommand(got); cmd != "custom-agent-bin" {
		t.Fatalf("resolveCommand(no snapshot) = %q, want custom-agent-bin", cmd)
	}
	command, args := ResolveSpawn(got, "/tmp/wt", "hello")
	if command != "custom-agent-bin" {
		t.Fatalf("command(no snapshot) = %q, want custom-agent-bin", command)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "params-model") {
		t.Fatalf("args(no snapshot) = %q, want params model stripped when role model set", joined)
	}

	// Snapshot vendor differs from owner: strip command + all vendor-owned args.
	got = executor.effectiveConfig(RunInput{
		UseSnapshot:    true,
		SnapshotVendor: string(config.AgentVendorCodex),
		SnapshotModel:  &snapshotModel,
	})
	if _, ok := got.Params["command"]; ok {
		t.Fatalf("params still has command when snapshot vendor diverges: %#v", got.Params)
	}
	if _, ok := got.Params["args"]; ok {
		t.Fatalf("params still has args when snapshot vendor diverges: %#v", got.Params)
	}
	if cmd := resolveCommand(got); cmd != "codex" {
		t.Fatalf("resolveCommand(diverged snapshot) = %q, want codex", cmd)
	}
	command, args = ResolveSpawn(got, "/tmp/wt", "hello")
	if command != "codex" {
		t.Fatalf("command(diverged snapshot) = %q, want codex", command)
	}
	joined = strings.Join(args, " ")
	if strings.Contains(joined, "params-model") || strings.Contains(joined, "also-params") || strings.Contains(joined, "--other") {
		t.Fatalf("args(snapshot) = %q, want clean vendor defaults (no foreign params.args)", joined)
	}
	if !strings.Contains(joined, "frozen-model") {
		t.Fatalf("args(snapshot) = %q, want frozen model", joined)
	}

	// Same vendor as owner: keep params.command wrapper, still strip model flags.
	got = executor.effectiveConfig(RunInput{
		UseSnapshot:    true,
		SnapshotVendor: string(config.AgentVendorClaudeCode),
		SnapshotModel:  &snapshotModel,
	})
	if cmd := resolveCommand(got); cmd != "custom-agent-bin" {
		t.Fatalf("resolveCommand(same-vendor snapshot) = %q, want custom-agent-bin wrapper", cmd)
	}
	command, args = ResolveSpawn(got, "/tmp/wt", "hello")
	if command != "custom-agent-bin" {
		t.Fatalf("command(same-vendor snapshot) = %q, want custom-agent-bin", command)
	}
	joined = strings.Join(args, " ")
	if strings.Contains(joined, "params-model") || strings.Contains(joined, "also-params") {
		t.Fatalf("args(same-vendor snapshot) = %q, want model flags stripped", joined)
	}
	if !strings.Contains(joined, "frozen-model") {
		t.Fatalf("args(same-vendor snapshot) = %q, want frozen model", joined)
	}

	// Original executor params must not be mutated.
	if _, ok := executor.config.Params["command"]; !ok {
		t.Fatal("executor config params.command was mutated")
	}
}

func TestEffectiveConfigNilOwnerStripsVendorOwnedParams(t *testing.T) {
	t.Parallel()

	// Global agent.vendor unset (nil owner) but role resolves to Claude; leftover
	// Codex wrappers in agent.params must not launch under the role vendor.
	roleModel := "role-claude"
	params := map[string]any{
		"command": "/opt/codex-wrapper",
		"args":    []any{"exec", "--model", "params-model", "--sandbox", "workspace-write"},
		"keep":    "yes",
	}
	executor := New(ExecutorOptions{Config: ExecutorConfig{
		Vendor: config.AgentVendorClaudeCode,
		Model:  &roleModel,
		Params: params,
	}})

	got := executor.effectiveConfig(RunInput{})
	if _, ok := got.Params["command"]; ok {
		t.Fatalf("nil owner still has command: %#v", got.Params)
	}
	if _, ok := got.Params["args"]; ok {
		t.Fatalf("nil owner still has args: %#v", got.Params)
	}
	if got.Params["keep"] != "yes" {
		t.Fatalf("nil owner dropped non-identity param: %#v", got.Params)
	}
	if cmd := resolveCommand(got); cmd != "claude" {
		t.Fatalf("resolveCommand(nil owner) = %q, want claude", cmd)
	}

	// Snapshot path with nil owner also strips vendor-owned wrappers.
	snapshotModel := "frozen-claude"
	got = executor.effectiveConfig(RunInput{
		UseSnapshot:    true,
		SnapshotVendor: string(config.AgentVendorClaudeCode),
		SnapshotModel:  &snapshotModel,
	})
	if _, ok := got.Params["command"]; ok {
		t.Fatalf("nil owner snapshot still has command: %#v", got.Params)
	}
	if cmd := resolveCommand(got); cmd != "claude" {
		t.Fatalf("resolveCommand(nil owner snapshot) = %q, want claude", cmd)
	}
	if params["command"] != "/opt/codex-wrapper" {
		t.Fatal("effectiveConfig mutated shared global params.command")
	}
}

func TestEffectiveConfigKeepsGlobalParamsWhenSnapshotMatchesOwner(t *testing.T) {
	t.Parallel()

	// Global Codex owns params.command; the live role handler is Claude after a
	// hot switch. Sticky retry still restores Codex from agent_snapshot_json.
	global := config.AgentVendorCodex
	roleModel := "role-claude-model"
	snapshotModel := "frozen-codex-model"
	params := map[string]any{
		"command": "/opt/codex-wrapper",
		"args":    []any{"exec", "--model", "params-model", "--sandbox", "workspace-write"},
		"keep":    "yes",
	}
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{
			Vendor: config.AgentVendorClaudeCode,
			Model:  &roleModel,
			Params: params,
		},
		ParamsOwnerVendor: &global,
	})

	// Live role claim (no snapshot): strip Codex wrappers so Claude launches.
	got := executor.effectiveConfig(RunInput{})
	if _, ok := got.Params["command"]; ok {
		t.Fatalf("live diverged role still has command: %#v", got.Params)
	}
	if _, ok := got.Params["args"]; ok {
		t.Fatalf("live diverged role still has args: %#v", got.Params)
	}
	if got.Params["keep"] != "yes" {
		t.Fatalf("live diverged role dropped non-identity param: %#v", got.Params)
	}
	if cmd := resolveCommand(got); cmd != "claude" {
		t.Fatalf("resolveCommand(live role) = %q, want claude", cmd)
	}

	// Sticky snapshot restores Codex (params owner): keep wrapper + non-model args.
	got = executor.effectiveConfig(RunInput{
		UseSnapshot:    true,
		SnapshotVendor: string(config.AgentVendorCodex),
		SnapshotModel:  &snapshotModel,
	})
	if got.Params["command"] != "/opt/codex-wrapper" {
		t.Fatalf("snapshot matching owner lost command: %#v", got.Params)
	}
	if cmd := resolveCommand(got); cmd != "/opt/codex-wrapper" {
		t.Fatalf("resolveCommand(snapshot owner) = %q, want /opt/codex-wrapper", cmd)
	}
	command, args := ResolveSpawn(got, "/tmp/wt", "hello")
	if command != "/opt/codex-wrapper" {
		t.Fatalf("command(snapshot owner) = %q, want /opt/codex-wrapper", command)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "params-model") || strings.Contains(joined, "--model params-model") {
		t.Fatalf("args(snapshot owner) = %q, want params model flags stripped", joined)
	}
	if !strings.Contains(joined, "frozen-codex-model") {
		t.Fatalf("args(snapshot owner) = %q, want frozen snapshot model", joined)
	}
	if !strings.Contains(joined, "exec") || !strings.Contains(joined, "--sandbox") {
		t.Fatalf("args(snapshot owner) = %q, want non-model args preserved", joined)
	}

	// Snapshot to a third vendor still strips owner wrappers.
	got = executor.effectiveConfig(RunInput{
		UseSnapshot:    true,
		SnapshotVendor: string(config.AgentVendorOpenCode),
		SnapshotModel:  &snapshotModel,
	})
	if _, ok := got.Params["command"]; ok {
		t.Fatalf("third-vendor snapshot still has command: %#v", got.Params)
	}
	if cmd := resolveCommand(got); cmd != "opencode" {
		t.Fatalf("resolveCommand(third-vendor snapshot) = %q, want opencode", cmd)
	}

	// Shared global params map must not be mutated.
	if params["command"] != "/opt/codex-wrapper" {
		t.Fatal("effectiveConfig mutated shared global params.command")
	}
}

func TestStripModelFlags(t *testing.T) {
	t.Parallel()

	got := stripModelFlags([]string{"--model", "x", "-m", "y", "--model=z", "-m=w", "-mMODEL", "--keep", "v"})
	want := []string{"--keep", "v"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stripModelFlags = %v, want %v", got, want)
	}
}

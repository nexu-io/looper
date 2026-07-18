package agent

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func codexCfg() ExecutorConfig  { v := config.AgentVendorCodex; return ExecutorConfig{Vendor: v} }
func claudeCfg() ExecutorConfig { v := config.AgentVendorClaudeCode; return ExecutorConfig{Vendor: v} }
func openCfg() ExecutorConfig   { v := config.AgentVendorOpenCode; return ExecutorConfig{Vendor: v} }

func TestInteractiveTakeoverSupported(t *testing.T) {
	if !InteractiveTakeoverSupported(config.AgentVendorCodex) {
		t.Fatalf("codex should be takeover-supported (verified)")
	}
	if !InteractiveTakeoverSupported(config.AgentVendorClaudeCode) {
		t.Fatalf("claude should be takeover-supported (verified)")
	}
	// opencode/cursor: resume-preserving unverified for interactive takeover → gated off.
	if InteractiveTakeoverSupported(config.AgentVendorOpenCode) {
		t.Fatalf("opencode takeover must stay gated until verified")
	}
}

func TestInteractiveResumeCommandLine(t *testing.T) {
	sid := "019f2d12-279e-7a73-90fd-144e516028dc"
	wt := "/Users/x/.looper/worktrees/repo-abc"

	got, ok := InteractiveResumeCommandLine(codexCfg(), wt, sid)
	if !ok || got != "cd "+wt+" && codex resume "+sid {
		t.Fatalf("codex resume line = %q (ok=%v)", got, ok)
	}

	got, ok = InteractiveResumeCommandLine(claudeCfg(), wt, sid)
	if !ok || got != "cd "+wt+" && claude --resume "+sid {
		t.Fatalf("claude resume line = %q (ok=%v)", got, ok)
	}

	// No worktree → bare resume, still valid.
	got, ok = InteractiveResumeCommandLine(codexCfg(), "", sid)
	if !ok || got != "codex resume "+sid {
		t.Fatalf("codex resume (no wt) = %q (ok=%v)", got, ok)
	}

	// Gated vendor / missing session → not offered.
	if _, ok := InteractiveResumeCommandLine(openCfg(), wt, sid); ok {
		t.Fatalf("opencode takeover must not render a command")
	}
	if _, ok := InteractiveResumeCommandLine(codexCfg(), wt, "  "); ok {
		t.Fatalf("empty session id must not render a command")
	}
}

// Cross-vendor role takeover must not reuse global agent.params.command owned by
// a different vendor (e.g. Codex wrapper handed to a Claude role session).
func TestInteractiveResumeCommandLineFiltersCrossVendorParams(t *testing.T) {
	sid := "019f2d12-279e-7a73-90fd-144e516028dc"
	wt := "/Users/x/.looper/worktrees/repo-abc"
	global := config.AgentVendorCodex
	params := map[string]any{
		"command": "/opt/codex-wrapper",
		"args":    []string{"--sandbox", "workspace-write"},
	}

	// Same-vendor takeover keeps the global wrapper command.
	same := ParamsForRoleVendor(params, &global, config.AgentVendorCodex, nil)
	got, ok := InteractiveResumeCommandLine(ExecutorConfig{Vendor: config.AgentVendorCodex, Params: same}, wt, sid)
	wantSame := "cd " + wt + " && /opt/codex-wrapper resume " + sid
	if !ok || got != wantSame {
		t.Fatalf("same-vendor resume = %q (ok=%v), want %q", got, ok, wantSame)
	}

	// Diverged role vendor strips command/args so resume uses the native binary.
	filtered := ParamsForRoleVendor(params, &global, config.AgentVendorClaudeCode, nil)
	got, ok = InteractiveResumeCommandLine(ExecutorConfig{Vendor: config.AgentVendorClaudeCode, Params: filtered}, wt, sid)
	wantClaude := "cd " + wt + " && claude --resume " + sid
	if !ok || got != wantClaude {
		t.Fatalf("cross-vendor resume = %q (ok=%v), want %q", got, ok, wantClaude)
	}
	if _, hasCmd := filtered["command"]; hasCmd {
		t.Fatalf("filtered params still contain command: %#v", filtered)
	}
}

func TestShellSingleQuote(t *testing.T) {
	// UUIDs and plain paths pass through untouched.
	if got := shellSingleQuote("019f2d12-279e-7a73"); got != "019f2d12-279e-7a73" {
		t.Fatalf("uuid quoted unexpectedly: %q", got)
	}
	if got := shellSingleQuote("/a/b/c"); got != "/a/b/c" {
		t.Fatalf("plain path quoted unexpectedly: %q", got)
	}
	// Spaces / quotes get single-quoted safely.
	if got := shellSingleQuote("/a b/c"); got != "'/a b/c'" {
		t.Fatalf("spaced path = %q", got)
	}
	if got := shellSingleQuote("a'b"); !strings.Contains(got, `'\''`) {
		t.Fatalf("embedded quote not escaped: %q", got)
	}
}

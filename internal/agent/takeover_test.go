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

package agent

import "testing"

func TestCodexJSONLTranslator(t *testing.T) {
	tr := newCodexJSONLTranslator()
	lines := []string{
		`{"type":"thread.started","thread_id":"th_abc123"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"type":"command_execution","id":"c1","command":"cat README.md"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","id":"c1","command":"cat README.md","output":"# Repo","exit_code":0}}`,
		`{"type":"item.started","item":{"type":"command_execution","id":"c2","command":"npm test"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","id":"c2","output":"fail","exit_code":1}}`,
		`not json — should be ignored`,
		`{"type":"unknown.event"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Done: added LICENSE."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10}}`,
	}
	for _, l := range lines {
		tr.ingestLine(l)
	}

	if tr.threadID != "th_abc123" {
		t.Fatalf("threadID = %q", tr.threadID)
	}
	if !tr.terminal {
		t.Fatalf("expected terminal after turn.completed")
	}
	if tr.finalText != "Done: added LICENSE." {
		t.Fatalf("finalText = %q", tr.finalText)
	}
	if len(tr.tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %+v", len(tr.tools), tr.tools)
	}
	if tr.tools[0].Status != "done" || tr.tools[1].Status != "error" {
		t.Fatalf("statuses = %q, %q", tr.tools[0].Status, tr.tools[1].Status)
	}
	// c2's completion omitted the command; it should be preserved from item.started.
	if tr.tools[1].Command != "npm test" {
		t.Fatalf("c2 command not preserved: %q", tr.tools[1].Command)
	}

	lines2 := tr.recentToolLines(5)
	if len(lines2) != 2 || lines2[0] != "✅ cat README.md" || lines2[1] != "❌ npm test" {
		t.Fatalf("recentToolLines = %v", lines2)
	}
}

func TestResolveCodexArgsJSONFlag(t *testing.T) {
	base := ExecutorConfig{Vendor: "codex"}
	// off by default → no --json
	got := resolveCodexArgs(base, []string{"-c", "model=gpt-5.4"}, "do it")
	if containsArg(got, "--json") {
		t.Fatalf("--json should be absent by default: %v", got)
	}
	// on → --json present, after exec, prompt still last
	on := base
	on.LiveToolEvents = true
	got = resolveCodexArgs(on, []string{"-c", "model=gpt-5.4"}, "do it")
	if got[0] != "exec" || !containsArg(got, "--json") || got[len(got)-1] != "do it" {
		t.Fatalf("expected exec … --json … prompt: %v", got)
	}
}

func TestCleanShellWrapper(t *testing.T) {
	cases := map[string]string{
		"/bin/zsh -lc 'gh api repos/x'":   "gh api repos/x",
		`bash -c "npm test"`:              "npm test",
		"gh pr create --title x":          "gh pr create --title x",
		"/usr/bin/sh -lc 'cat README.md'": "cat README.md",
	}
	for in, want := range cases {
		if got := cleanShellWrapper(in); got != want {
			t.Fatalf("cleanShellWrapper(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestExtractCodexThreadID(t *testing.T) {
	blob := `{"type":"thread.started","thread_id":"019f2d12-279e-7a73"}
{"type":"item.started","item":{"type":"command_execution","id":"c1","command":"ls"}}`
	if got := extractCodexThreadID(blob); got != "019f2d12-279e-7a73" {
		t.Fatalf("extractCodexThreadID = %q; want the thread id", got)
	}
	if got := extractCodexThreadID(`{"type":"item.started"}`); got != "" {
		t.Fatalf("extractCodexThreadID(no thread.started) = %q; want empty", got)
	}
	if got := extractCodexThreadID("not json\n"); got != "" {
		t.Fatalf("extractCodexThreadID(garbage) = %q; want empty", got)
	}
}

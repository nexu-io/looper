package reviewer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/reviewer/reviewskills"
)

// Exercise the shipped, opt-in method through the real resolver and prompt builder.
// This proves wiring and fail-loud behavior, not model review quality.
func TestMaintainabilitySkillSelectionAndPrompt(t *testing.T) {
	t.Parallel()
	bundle, err := reviewskills.MaterializeBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	worktree := t.TempDir()
	dir := filepath.Join(worktree, ".agents", "skills", "maintainability-review")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../../skills/maintainability-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, dir); err != nil {
		t.Fatal(err)
	}
	input := reviewskills.ResolveInput{
		Mode: config.ReviewerSkillsModeExtend, Worktree: worktree,
		UserHome: t.TempDir(), BuiltinDir: bundle.Dir,
	}
	defaults, err := reviewskills.Resolve(input)
	if err != nil || len(defaults.Entries) != 1 || defaults.Entries[0].Name != "looper-review" {
		t.Fatalf("unconfigured method must stay disabled: %+v, %v", defaults, err)
	}
	input.Required = []string{"maintainability-review"}
	resolved, err := reviewskills.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Entries) != 2 || resolved.Entries[0].Name != "looper-review" || resolved.Entries[1].Name != "maintainability-review" || !resolved.Entries[1].Required {
		t.Fatalf("want builtin plus required maintainability method: %+v", resolved)
	}
	index := reviewskills.FormatIndex(resolved.Entries)
	prompt, _ := buildReviewPromptWithInstructions("", config.Config{}, "acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, false, "", config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper", false, false, "", index)
	for _, entry := range resolved.Entries {
		if !filepath.IsAbs(entry.Path) || !strings.Contains(prompt, entry.Path) {
			t.Fatalf("prompt missing absolute method path %q", entry.Path)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewskills.Resolve(input); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("required missing method must fail: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: wrong-name\ndescription: invalid installation\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewskills.Resolve(input); err == nil || !strings.Contains(err.Error(), "name mismatch") {
		t.Fatalf("invalid required method must fail: %v", err)
	}
}

package reviewskills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestResolveThreeLayerLookupAndProjectOverridesUser(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	userHome := t.TempDir()
	builtinDir := t.TempDir()
	writeSkillMD(t, filepath.Join(builtinDir, "SKILL.md"), "looper-review", "builtin method")
	writeNamedSkill(t, filepath.Join(builtinDir, "only-builtin"), "only-builtin", "from builtin")
	writeNamedSkill(t, filepath.Join(userHome, ".agents", "skills", "shared"), "shared", "from user")
	writeNamedSkill(t, filepath.Join(userHome, ".agents", "skills", "only-user"), "only-user", "from user")
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "shared"), "shared", "from project")
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "only-project"), "only-project", "from project")

	result, err := Resolve(ResolveInput{
		Mode:       config.ReviewerSkillsModeReplace,
		Required:   []string{"only-project", "shared", "only-user", "only-builtin"},
		Worktree:   worktree,
		UserHome:   userHome,
		BuiltinDir: builtinDir,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Entries) != 4 {
		t.Fatalf("entries = %#v, want 4", result.Entries)
	}
	assertSkill(t, result.Entries[0], "only-project", "project")
	assertSkill(t, result.Entries[1], "shared", "project")
	assertSkill(t, result.Entries[2], "only-user", "user")
	assertSkill(t, result.Entries[3], "only-builtin", "builtin")
	if !strings.Contains(result.Entries[1].Path, filepath.Join(worktree, ".agents", "skills", "shared")) {
		t.Fatalf("shared path = %q, want project overlay", result.Entries[1].Path)
	}
}

func TestResolveSameLevelConflict(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "alpha"), "dup", "a")
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "beta"), "dup", "b")

	_, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"dup"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "multiple files") {
		t.Fatalf("Resolve() error = %v, want same-level conflict", err)
	}
}

func TestResolveBoundsSkillFrontmatter(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	root := filepath.Join(worktree, ".agents", "skills")
	writeNamedSkill(t, filepath.Join(root, "large-body"), "large-body", "small metadata")
	f, err := os.OpenFile(filepath.Join(root, "large-body", "SKILL.md"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("body text\n", 128<<10)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	writeNamedSkill(t, filepath.Join(root, "second"), "second", "another method")
	writeNamedSkill(t, filepath.Join(root, "oversized"), "oversized", strings.Repeat("x", 128<<10))

	result, err := Resolve(ResolveInput{Mode: config.ReviewerSkillsModeReplace, Required: []string{"large-body", "second"}, Worktree: worktree})
	if err != nil || len(result.Entries) != 2 {
		t.Fatalf("large bodies and unrelated oversized metadata must not block selected methods: %#v, %v", result, err)
	}
	for _, ref := range []string{"oversized", "./.agents/skills/oversized/SKILL.md"} {
		_, err := Resolve(ResolveInput{Mode: config.ReviewerSkillsModeReplace, Required: []string{ref}, Worktree: worktree})
		if err == nil || !strings.Contains(err.Error(), "frontmatter") {
			t.Fatalf("Resolve(%q) error = %v, want oversized metadata rejected", ref, err)
		}
	}
}

func TestReplacementIndexOnlyNamesResolvedMethods(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "team-review"), "team-review", "team method")
	result, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"team-review"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	index := FormatIndex(result.Entries)
	if strings.Contains(index, "looper-review") {
		t.Fatalf("replacement index instructs the agent to use an unavailable builtin:\n%s", index)
	}
	if !strings.Contains(index, result.Entries[0].Path) || !strings.Contains(index, "complete the full review") {
		t.Fatalf("replacement index lost the custom method or full-review contract:\n%s", index)
	}
}

func TestResolveSymlinkAliasDedupes(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	realDir := filepath.Join(worktree, ".agents", "skills", "real")
	writeNamedSkill(t, realDir, "aliased", "real skill")
	aliasDir := filepath.Join(worktree, ".agents", "skills", "alias")
	if err := os.MkdirAll(filepath.Dir(aliasDir), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	result, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"aliased"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Name != "aliased" {
		t.Fatalf("entries = %#v, want one aliased skill", result.Entries)
	}
}

func TestResolveNameAndPathMix(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "named"), "named", "by name")
	pathDir := filepath.Join(worktree, "custom skills", "path review")
	writeNamedSkill(t, pathDir, "path-review", "by path")

	result, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"named", "./custom skills/path review"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("entries = %#v, want name and path", result.Entries)
	}
	assertSkill(t, result.Entries[0], "named", "project")
	assertSkill(t, result.Entries[1], "path-review", "path")
	if !strings.Contains(result.Entries[1].Path, "custom skills") {
		t.Fatalf("path entry = %q, want spaces preserved", result.Entries[1].Path)
	}
}

func TestResolveCorruptHighPriorityDoesNotFallback(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	userHome := t.TempDir()
	writeNamedSkill(t, filepath.Join(userHome, ".agents", "skills", "security-review"), "security-review", "user copy")
	corrupt := filepath.Join(worktree, ".agents", "skills", "security-review")
	if err := os.MkdirAll(corrupt, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, "SKILL.md"), []byte("not a skill\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"security-review"},
		Worktree: worktree,
		UserHome: userHome,
	})
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("Resolve() error = %v, want unreadable high-priority skill", err)
	}
}

func TestResolveExtendBuiltinNotShadowed(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	builtinDir := t.TempDir()
	writeSkillMD(t, filepath.Join(builtinDir, "SKILL.md"), "looper-review", "builtin method")
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "looper-review"), "looper-review", "overlay")
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "extra"), "extra", "project extra")

	result, err := Resolve(ResolveInput{
		Mode:       config.ReviewerSkillsModeExtend,
		Required:   []string{"looper-review", "extra"},
		Worktree:   worktree,
		UserHome:   t.TempDir(),
		BuiltinDir: builtinDir,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("entries = %#v, want builtin plus extra", result.Entries)
	}
	assertSkill(t, result.Entries[0], "looper-review", "builtin")
	if !strings.Contains(result.Entries[0].Path, builtinDir) {
		t.Fatalf("builtin path = %q, want bundle dir", result.Entries[0].Path)
	}
	assertSkill(t, result.Entries[1], "extra", "project")
}

func TestResolveRelativePathWithSpaces(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	writeNamedSkill(t, filepath.Join(worktree, "my skills", "custom review"), "custom-review", "spaced")

	result, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"./my skills/custom review/SKILL.md"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Name != "custom-review" || result.Entries[0].Source != "path" {
		t.Fatalf("entries = %#v", result.Entries)
	}
}

func TestResolveOptionalMissingAndRequiredMissing(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "present"), "present", "ok")

	result, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"present"},
		Optional: []string{"missing-optional"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Unavailable) != 1 || result.Unavailable[0] != "missing-optional" {
		t.Fatalf("unavailable = %#v", result.Unavailable)
	}

	_, err = Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"missing-required"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), `reviewer skill "missing-required" not found`) {
		t.Fatalf("Resolve() error = %v, want required missing", err)
	}
}

func TestResolveRequiredAndOptionalSamePathDedupes(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	writeNamedSkill(t, filepath.Join(worktree, ".agents", "skills", "shared"), "shared", "once")

	result, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"shared"},
		Optional: []string{"shared"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Entries) != 1 || !result.Entries[0].Required {
		t.Fatalf("entries = %#v, want required once", result.Entries)
	}
}

func TestResolveQuotedYAMLNameMatchesConfiguredName(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	dir := filepath.Join(worktree, ".agents", "skills", "security-review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	content := "---\nname: \"security-review\"\ndescription: quoted yaml name\n---\n\n# security-review\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := Resolve(ResolveInput{
		Mode:     config.ReviewerSkillsModeReplace,
		Required: []string{"security-review"},
		Worktree: worktree,
		UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Name != "security-review" {
		t.Fatalf("entries = %#v, want quoted YAML name match", result.Entries)
	}
}

func writeNamedSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	writeSkillMD(t, filepath.Join(dir, "SKILL.md"), name, description)
}

func writeSkillMD(t *testing.T, path, name, description string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func assertSkill(t *testing.T, entry Entry, name, source string) {
	t.Helper()
	if entry.Name != name || entry.Source != source || !entry.Required {
		t.Fatalf("entry = %+v, want name=%s source=%s required", entry, name, source)
	}
	if !filepath.IsAbs(entry.Path) {
		t.Fatalf("entry path is not absolute: %q", entry.Path)
	}
}

package reviewskills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeBuiltinReadableFromUnrelatedCwd(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cwd := t.TempDir()
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(orig)
	})

	bundle, err := MaterializeBuiltin()
	if err != nil {
		t.Fatalf("MaterializeBuiltin: %v", err)
	}
	t.Cleanup(func() {
		_ = bundle.Close()
	})
	if !filepath.IsAbs(bundle.Dir) {
		t.Fatalf("bundle dir is not absolute: %q", bundle.Dir)
	}
	if strings.Contains(bundle.Dir, "internal/reviewer/reviewskills") {
		t.Fatalf("bundle dir %q was written into the source tree", bundle.Dir)
	}
	if !strings.Contains(filepath.Base(bundle.Dir), "looper-review-skills-") {
		t.Fatalf("bundle dir %q does not use looper-review-skills-* prefix", bundle.Dir)
	}
	if len(bundle.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(bundle.Entries))
	}
	entry := bundle.Entries[0]
	if entry.Name != "looper-review" || entry.Source != "builtin" || !entry.Required {
		t.Fatalf("entry = %+v", entry)
	}
	if !filepath.IsAbs(entry.Path) {
		t.Fatalf("entry path is not absolute: %q", entry.Path)
	}
	skill, err := os.ReadFile(entry.Path)
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if !strings.Contains(string(skill), "name: looper-review") {
		t.Fatalf("SKILL.md missing name field:\n%s", skill)
	}
	for _, rel := range []string{
		"references/comment-quality.md",
		"references/implementation-rubric.md",
		"references/spec-rubric.md",
	} {
		data, err := os.ReadFile(filepath.Join(bundle.Dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			t.Fatalf("%s is empty", rel)
		}
	}
}

func TestBundleCloseRemovesDirectory(t *testing.T) {
	bundle, err := MaterializeBuiltin()
	if err != nil {
		t.Fatalf("MaterializeBuiltin: %v", err)
	}
	dir := bundle.Dir
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stat materialized dir: %v", err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Close left directory %q: %v", dir, err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestFormatIndexUsesAbsoluteMaterializedPath(t *testing.T) {
	bundle, err := MaterializeBuiltin()
	if err != nil {
		t.Fatalf("MaterializeBuiltin: %v", err)
	}
	t.Cleanup(func() {
		_ = bundle.Close()
	})
	index := FormatIndex(bundle.Entries)
	for _, want := range []string{
		"Review method skills:",
		"Every listed skill MUST be read from the given absolute paths before reviewing.",
		"still complete the base review using looper-review",
		"name: looper-review",
		"source: builtin",
		"required: true",
		"path: " + bundle.Entries[0].Path,
	} {
		if !strings.Contains(index, want) {
			t.Fatalf("index missing %q:\n%s", want, index)
		}
	}
	if strings.Contains(index, "<run-local builtin skill path>") {
		t.Fatalf("runtime index used preview placeholder:\n%s", index)
	}
}

func TestPreviewIndexPlaceholderHasNoFilesystemPath(t *testing.T) {
	preview := PreviewIndexPlaceholder()
	for _, want := range []string{
		"Review method skills:",
		"looper-review",
		"builtin",
		"<run-local builtin skill path>",
		builtinSkillDescription,
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, preview)
		}
	}
	for _, forbidden := range []string{"/tmp/", "looper-review-skills-", `C:\`} {
		if strings.Contains(preview, forbidden) {
			t.Fatalf("preview contains fabricated path fragment %q:\n%s", forbidden, preview)
		}
	}
}

func TestFormatIndexPathsChangeAcrossBundles(t *testing.T) {
	first, err := MaterializeBuiltin()
	if err != nil {
		t.Fatalf("first MaterializeBuiltin: %v", err)
	}
	firstPath := first.Entries[0].Path
	firstIndex := FormatIndex(first.Entries)
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Fatalf("old skill path still exists after Close: %v", err)
	}

	second, err := MaterializeBuiltin()
	if err != nil {
		t.Fatalf("second MaterializeBuiltin: %v", err)
	}
	t.Cleanup(func() {
		_ = second.Close()
	})
	secondPath := second.Entries[0].Path
	secondIndex := FormatIndex(second.Entries)
	if firstPath == secondPath {
		t.Fatalf("rematerialize reused path %q", firstPath)
	}
	if strings.Contains(secondIndex, firstPath) {
		t.Fatalf("new index still mentions old path %q:\n%s", firstPath, secondIndex)
	}
	if !strings.Contains(secondIndex, secondPath) {
		t.Fatalf("new index missing current path %q:\n%s", secondPath, secondIndex)
	}
	if strings.Contains(firstIndex, secondPath) {
		t.Fatalf("old index unexpectedly contains new path")
	}
	if reminder := ResumeReminder(second.Entries); !strings.Contains(reminder, secondPath) || strings.Contains(reminder, firstPath) {
		t.Fatalf("resume reminder = %q, want current path %q", reminder, secondPath)
	}
}

func TestParseFrontmatterDecodesQuotedYAMLName(t *testing.T) {
	t.Parallel()

	name, description, err := parseFrontmatter("---\nname: \"security-review\" # quoted\ndescription: 'team method'\n---\n\n# body\n")
	if err != nil {
		t.Fatalf("parseFrontmatter() error = %v", err)
	}
	if name != "security-review" || description != "team method" {
		t.Fatalf("parseFrontmatter() = name %q description %q", name, description)
	}
}

func TestFormatIndexAndResumeReminderRequireReadingResolvedOptionalSkills(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{Name: "looper-review", Path: "/tmp/looper-review/SKILL.md", Source: "builtin", Required: true, Description: "base"},
		{Name: "perf-review", Path: "/tmp/perf-review/SKILL.md", Source: "project", Required: false, Description: "optional present"},
	}
	index := FormatIndex(entries)
	if !strings.Contains(index, "Every listed skill MUST be read") {
		t.Fatalf("index missing read-all instruction:\n%s", index)
	}
	if !strings.Contains(index, "required: false") || !strings.Contains(index, "perf-review") {
		t.Fatalf("index missing optional entry:\n%s", index)
	}
	reminder := ResumeReminder(entries)
	if !strings.Contains(reminder, "/tmp/perf-review/SKILL.md") {
		t.Fatalf("resume reminder omitted resolved optional path: %q", reminder)
	}
}

func TestBuiltinSkillFilesContainMigratedMethodText(t *testing.T) {
	bundle, err := MaterializeBuiltin()
	if err != nil {
		t.Fatalf("MaterializeBuiltin: %v", err)
	}
	t.Cleanup(func() {
		_ = bundle.Close()
	})
	joined := readMaterializedTree(t, bundle.Dir)
	for _, want := range []string{
		"Every comment MUST include",
		"Every finding MUST include",
		"Bad comment example",
		"Good spec/docs comment example",
		"Write substantially more detail",
		"Do not repeat the overall body/summary as a comment",
		"fixture-matrix tests",
		"Implementation review rubric",
		"Spec/docs review rubric",
		"mark a finding as BLOCKING only when",
		"group repeated patterns into systemic comments with representative examples only when they share a root cause",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("materialized skills missing %q", want)
		}
	}
}

func readMaterializedTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b.Write(data)
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return b.String()
}

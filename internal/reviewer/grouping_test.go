package reviewer

import (
	"reflect"
	"testing"
)

func TestParseNameStatusIncludesDeletionsAndRenames(t *testing.T) {
	t.Parallel()
	files := parseNameStatus("M\tinternal/reviewer/runner.go\nD\tinternal/old.go\nR100\tinternal/a.go\tinternal/b.go\n")
	got := map[string]string{}
	for _, file := range files {
		got[file.Path] = file.Status
	}
	if got["internal/reviewer/runner.go"] != "M" {
		t.Fatalf("modified = %#v", files)
	}
	if got["internal/old.go"] != "D" {
		t.Fatalf("deletion missing: %#v", files)
	}
	if got["internal/b.go"] == "" || got["internal/a.go"] != "D" {
		t.Fatalf("rename sides = %#v", files)
	}
}

func TestGroupChangedFilesAssignsEveryPath(t *testing.T) {
	t.Parallel()
	files := []changedFile{
		{Path: "internal/reviewer/runner.go"},
		{Path: "internal/reviewer/runner_test.go"},
		{Path: "internal/config/types.go"},
		{Path: "docs/configuration.md"},
		{Path: "internal/old.go", Status: "D"},
	}
	groups := groupChangedFiles(files)
	seen := map[string]string{}
	for _, group := range groups {
		for _, path := range group.Paths {
			if prev, ok := seen[path]; ok {
				t.Fatalf("path %s in %s and %s", path, prev, group.ID)
			}
			seen[path] = group.ID
		}
	}
	for _, file := range files {
		if _, ok := seen[file.Path]; !ok {
			t.Fatalf("unassigned %s in %#v", file.Path, groups)
		}
	}
	if seen["internal/reviewer/runner.go"] != seen["internal/reviewer/runner_test.go"] {
		t.Fatalf("package split: %#v", groups)
	}
	if seen["docs/configuration.md"] != "other" {
		t.Fatalf("docs group = %q", seen["docs/configuration.md"])
	}
}

func TestMergeGroupedFindingsDedupeRootCause(t *testing.T) {
	t.Parallel()
	a := []reviewerCommentOnlyFindingResult{{Title: "Bug", Path: "a.go", Body: "one"}}
	b := []reviewerCommentOnlyFindingResult{{Title: "Bug", Path: "a.go", Body: "dup"}, {Title: "Other", Path: "b.go", Body: "two"}}
	got := mergeGroupedFindings(a, b)
	if len(got) != 2 {
		t.Fatalf("merged = %#v", got)
	}
}

func TestOtherPathsListsRemainder(t *testing.T) {
	t.Parallel()
	all := []changedFile{{Path: "a.go"}, {Path: "b.go"}, {Path: "c.md"}}
	got := otherPaths(fileGroup{Paths: []string{"a.go"}}, all)
	if !reflect.DeepEqual(got, []string{"b.go", "c.md"}) {
		t.Fatalf("other = %#v", got)
	}
}

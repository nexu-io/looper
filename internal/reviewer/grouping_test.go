package reviewer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestParseNameStatusIncludesDeletionsAndRenames(t *testing.T) {
	t.Parallel()
	files := parseNameStatus("M\x00internal/reviewer/runner.go\x00D\x00internal/old.go\x00R100\x00internal/a.go\x00internal/b.go\x00")
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

func TestParseNameStatusKeepsLiteralSpecialPaths(t *testing.T) {
	t.Parallel()
	files := parseNameStatus("M\x00dir/\tcafe\303\251.go\x00A\x00weird\\303\\251.go\x00")
	got := map[string]string{}
	for _, file := range files {
		got[file.Path] = file.Status
	}
	if got["dir/\tcafe\303\251.go"] != "M" {
		t.Fatalf("tab/latin1 path = %#v", files)
	}
	if got[`weird\303\251.go`] != "A" {
		t.Fatalf("literal escaped path = %#v", files)
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

func TestMergeGroupedFindingsPreservesDistinctFindings(t *testing.T) {
	t.Parallel()
	a := []reviewerCommentOnlyFindingResult{{Title: "Bug", Path: "a.go", Line: 10, Body: "one"}}
	b := []reviewerCommentOnlyFindingResult{
		{Title: "Bug", Path: "a.go", Line: 20, Body: "two"},
		{Title: "Bug", Files: []string{"b.go"}, Body: "files"},
	}
	got := mergeGroupedFindings(a, b)
	if len(got) != 3 {
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

func TestGroupingGitPathUsesConfiguredExecutable(t *testing.T) {
	t.Parallel()
	path := "/opt/custom/git"
	cfg := config.Config{}
	cfg.Tools.GitPath = &path
	got := groupingGitPath(&Runner{projectRoleConfig: &cfg})
	if got != path {
		t.Fatalf("groupingGitPath() = %q, want %q", got, path)
	}
}

func TestGroupedFindingPromptForbidsWorktreeMutation(t *testing.T) {
	t.Parallel()
	prompt := groupedFindingPrompt(fileGroup{ID: "go:pkg", Paths: []string{"a.go"}}, []changedFile{{Path: "a.go"}, {Path: "b.md"}}, "base", "head")
	if !strings.Contains(prompt, "Do not checkout another revision") {
		t.Fatalf("prompt missing read-only contract: %s", prompt)
	}
	if !strings.Contains(prompt, "must not publish") && !strings.Contains(prompt, "Do not publish") {
		t.Fatalf("prompt missing publish prohibition: %s", prompt)
	}
}

func TestApplyRelatedFileGroupsFailsOnChangedPathEnumeration(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing-git")
	cfg := config.Config{}
	cfg.Roles.Reviewer.Behavior.RelatedFileGroups = config.ReviewerRelatedFileGroupsConfig{Enabled: true, MinChangedFiles: 1}
	cfg.Tools.GitPath = &missing
	runner := &Runner{projectRoleConfig: &cfg}
	_, err := runner.applyRelatedFileGroups(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "p"}}, reviewerCheckpoint{Snapshot: &checkpointSnapshot{BaseSHA: "aaa", HeadSHA: "bbb"}}, t.TempDir(), "skills")
	if err == nil || !strings.Contains(err.Error(), "enumerate changed paths") {
		t.Fatalf("applyRelatedFileGroups() error = %v", err)
	}
}

func TestApplyRelatedFileGroupsLeavesSmallDiffUngrouped(t *testing.T) {
	t.Parallel()
	repo, base, head := groupingTestRepoWithRename(t)
	cfg := config.Config{}
	cfg.Roles.Reviewer.Behavior.RelatedFileGroups = config.ReviewerRelatedFileGroupsConfig{Enabled: true, MinChangedFiles: 24}
	runner := &Runner{projectRoleConfig: &cfg}
	got, err := runner.applyRelatedFileGroups(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "p"}}, reviewerCheckpoint{Snapshot: &checkpointSnapshot{BaseSHA: base, HeadSHA: head}}, repo, "skills")
	if err != nil {
		t.Fatalf("applyRelatedFileGroups() error = %v", err)
	}
	if got != "skills" {
		t.Fatalf("skillIndex = %q, want unchanged", got)
	}
}

func TestListChangedPathsParsesNULNameStatus(t *testing.T) {
	t.Parallel()
	repo, base, head := groupingTestRepoWithRename(t)
	files, err := listChangedPaths(context.Background(), "git", repo, base, head)
	if err != nil {
		t.Fatalf("listChangedPaths() error = %v", err)
	}
	got := map[string]string{}
	for _, file := range files {
		got[file.Path] = file.Status
	}
	if got["new name.go"] == "" || got["old name.go"] != "D" {
		t.Fatalf("rename paths = %#v", files)
	}
}

func TestRunGroupedFindingAgentsRequiresCompletedStatus(t *testing.T) {
	t.Parallel()
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[]}`}}}
	runner := &Runner{agentExecutor: agent}
	_, err := runner.runGroupedFindingAgents(context.Background(), stepInput{}, t.TempDir(), []fileGroup{{ID: "go:pkg", Paths: []string{"a.go"}}}, []changedFile{{Path: "a.go"}}, "base", "head")
	if err == nil || !strings.Contains(err.Error(), "agent failed") {
		t.Fatalf("runGroupedFindingAgents() error = %v", err)
	}
}

func TestRunGroupedFindingAgentsRequiresCommentOnlyMarker(t *testing.T) {
	t.Parallel()
	repo, _, head := groupingTestRepoWithRename(t)
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Stdout: "reviewed without a marker"}}}
	runner := &Runner{agentExecutor: agent, projectRoleConfig: &config.Config{}}
	_, err := runner.runGroupedFindingAgents(context.Background(), stepInput{}, repo, []fileGroup{{ID: "go:pkg", Paths: []string{"new name.go"}}}, []changedFile{{Path: "new name.go"}}, "base", head)
	if err == nil || !strings.Contains(err.Error(), "completion marker is required") {
		t.Fatalf("runGroupedFindingAgents() error = %v", err)
	}
}

func TestRunGroupedFindingAgentsPassesRunSnapshot(t *testing.T) {
	t.Parallel()
	repo, _, head := groupingTestRepoWithRename(t)
	model := "gpt-5"
	snapshot, err := config.MarshalAgentSnapshot(config.AgentSnapshot{Vendor: "codex", Model: &model})
	if err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[]}`}}}
	runner := &Runner{agentExecutor: agent, projectRoleConfig: &config.Config{}}
	if _, err := runner.runGroupedFindingAgents(context.Background(), stepInput{Run: storage.RunRecord{AgentSnapshotJSON: &snapshot}}, repo, []fileGroup{{ID: "go:pkg", Paths: []string{"new name.go"}}}, []changedFile{{Path: "new name.go"}}, "base", head); err != nil {
		t.Fatalf("runGroupedFindingAgents() error = %v", err)
	}
	if len(agent.starts) != 1 || !agent.starts[0].UseSnapshot || agent.starts[0].SnapshotVendor != "codex" {
		t.Fatalf("starts = %#v", agent.starts)
	}
	if agent.starts[0].SnapshotModel == nil || *agent.starts[0].SnapshotModel != model {
		t.Fatalf("snapshot model = %#v", agent.starts[0].SnapshotModel)
	}
}

func TestAssertGroupedWorktreeUnchangedDetectsDirtyTree(t *testing.T) {
	t.Parallel()
	repo, _, head := groupingTestRepoWithRename(t)
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := (&Runner{}).assertGroupedWorktreeUnchanged(context.Background(), repo, head)
	if err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("assertGroupedWorktreeUnchanged() error = %v", err)
	}
}

func groupingTestRepoWithRename(t *testing.T) (repo, base, head string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t",
			"GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "old name.go"), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "base")
	base = run("rev-parse", "HEAD")
	run("mv", "old name.go", "new name.go")
	run("commit", "-qm", "rename")
	head = run("rev-parse", "HEAD")
	return repo, base, head
}

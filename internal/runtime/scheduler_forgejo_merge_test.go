package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgejoHasMergeConflictsFetchesExactObjectsWithoutChangingCaller(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, remote, caller := filepath.Join(root, "source"), filepath.Join(root, "remote.git"), filepath.Join(root, "caller")
	forgejoMergeTestMustMkdirAll(t, source)
	forgejoMergeTestRunGit(t, source, "init", "-b", "main")
	forgejoMergeTestConfigureRepo(t, source)
	forgejoMergeTestWriteFile(t, filepath.Join(source, "README.md"), "original\n")
	forgejoMergeTestRunGit(t, source, "add", ".")
	forgejoMergeTestRunGit(t, source, "commit", "-m", "initial")
	initial := strings.TrimSpace(forgejoMergeTestRunGit(t, source, "rev-parse", "HEAD"))
	forgejoMergeTestRunGit(t, root, "clone", "--bare", source, remote)
	forgejoMergeTestRunGit(t, root, "clone", remote, caller)
	forgejoMergeTestRunGit(t, source, "remote", "add", "origin", remote)
	for _, branch := range []string{"feature", "clean"} {
		forgejoMergeTestRunGit(t, source, "checkout", "-b", branch, initial)
		path := "README.md"
		if branch == "clean" {
			path = "new.txt"
		}
		forgejoMergeTestWriteFile(t, filepath.Join(source, path), branch+" change\n")
		forgejoMergeTestRunGit(t, source, "add", ".")
		forgejoMergeTestRunGit(t, source, "commit", "-m", branch)
		forgejoMergeTestRunGit(t, source, "push", "origin", branch)
	}
	head := strings.TrimSpace(forgejoMergeTestRunGit(t, source, "rev-parse", "feature"))
	clean := strings.TrimSpace(forgejoMergeTestRunGit(t, source, "rev-parse", "clean"))
	forgejoMergeTestRunGit(t, source, "checkout", "main")
	forgejoMergeTestWriteFile(t, filepath.Join(source, "README.md"), "main change\n")
	forgejoMergeTestRunGit(t, source, "add", ".")
	forgejoMergeTestRunGit(t, source, "commit", "-m", "base advances")
	forgejoMergeTestRunGit(t, source, "push", "origin", "main")
	base := strings.TrimSpace(forgejoMergeTestRunGit(t, source, "rev-parse", "HEAD"))

	// Deliberately dirty both the index and working tree, and retain FETCH_HEAD.
	forgejoMergeTestWriteFile(t, filepath.Join(caller, "README.md"), "staged local work\n")
	forgejoMergeTestRunGit(t, caller, "add", "README.md")
	forgejoMergeTestWriteFile(t, filepath.Join(caller, "README.md"), "unstaged local work\n")
	forgejoMergeTestWriteFile(t, filepath.Join(caller, "untracked.txt"), "keep me\n")
	forgejoMergeTestWriteFile(t, filepath.Join(caller, ".git", "FETCH_HEAD"), "caller fetch marker\n")
	before := mergeConflictCallerSnapshot(t, caller)
	for _, tc := range []struct {
		name, head string
		want       bool
	}{{"clean", clean, false}, {"conflict", head, true}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := forgejoHasMergeConflicts(context.Background(), "git", caller, base, tc.head)
			if err != nil || got != tc.want {
				t.Fatalf("HasMergeConflicts = %v, %v; want %v", got, err, tc.want)
			}
			if after := mergeConflictCallerSnapshot(t, caller); after != before {
				t.Fatalf("caller changed:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
	// Once the exact objects are cached, remote unavailability must not matter.
	forgejoMergeTestRunGit(t, caller, "remote", "set-url", "origin", filepath.Join(root, "unavailable.git"))
	if got, err := forgejoHasMergeConflicts(context.Background(), "git", caller, base, head); err != nil || !got {
		t.Fatalf("cached merge check = %v, %v; must not fetch existing objects", got, err)
	}
	if _, err := os.Stat(filepath.Join(caller, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("MERGE_HEAD exists after read-only check: %v", err)
	}
	if got, err := forgejoHasMergeConflicts(context.Background(), "git", caller, base, strings.Repeat("1", 40)); err == nil || got {
		t.Fatalf("missing commit with failed fetch = %v, %v; want error, not conflict", got, err)
	}
}

func TestForgejoHasMergeConflictsDoesNotTurnGitFailuresIntoConflicts(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct{ name, script string }{
		{"inspect failure", "#!/bin/sh\necho 'cannot read repository' >&2\nexit 128\n"},
		{"unsupported merge-tree", "#!/bin/sh\nif [ \"$1\" = cat-file ]; then while read object; do echo \"$object commit\"; done; exit 0; fi\necho 'unknown option write-tree' >&2\nexit 129\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitPath := forgejoMergeTestWriteFakeGit(t, tc.script)
			got, err := forgejoHasMergeConflicts(context.Background(), gitPath, t.TempDir(), sha, sha)
			if err == nil || got {
				t.Fatalf("HasMergeConflicts = %v, %v; want Git error", got, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := forgejoHasMergeConflicts(ctx, "git", t.TempDir(), sha, sha); err == nil || got {
		t.Fatalf("cancelled check = %v, %v; want error", got, err)
	}
}

func mergeConflictCallerSnapshot(t *testing.T, cwd string) string {
	t.Helper()
	return strings.Join([]string{
		forgejoMergeTestRunGit(t, cwd, "symbolic-ref", "HEAD"), forgejoMergeTestRunGit(t, cwd, "rev-parse", "HEAD"),
		forgejoMergeTestRunGit(t, cwd, "for-each-ref", "--format=%(refname) %(objectname)"),
		forgejoMergeTestRunGit(t, cwd, "ls-files", "--stage"), forgejoMergeTestRunGit(t, cwd, "status", "--porcelain=v1"),
		forgejoMergeTestReadFile(t, filepath.Join(cwd, "README.md")), forgejoMergeTestReadFile(t, filepath.Join(cwd, "untracked.txt")),
		forgejoMergeTestReadFile(t, filepath.Join(cwd, ".git", "FETCH_HEAD")),
	}, "\n")
}

func forgejoMergeTestRunGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func forgejoMergeTestConfigureRepo(t *testing.T, cwd string) {
	t.Helper()
	forgejoMergeTestRunGit(t, cwd, "config", "user.name", "Looper Test")
	forgejoMergeTestRunGit(t, cwd, "config", "user.email", "test@example.com")
	forgejoMergeTestRunGit(t, cwd, "config", "commit.gpgsign", "false")
}

func forgejoMergeTestMustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func forgejoMergeTestWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func forgejoMergeTestReadFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func forgejoMergeTestWriteFakeGit(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

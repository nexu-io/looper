package harness

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type RepoSnapshot struct {
	Head             string
	StatusPorcelain  string
	IndexTree        string
	CurrentBranch    string
	WorktreeListText string
}

type SeededRepo struct {
	Path          string
	DefaultBranch string
	InitialCommit string
}

func CreateSeededRepo(tb testing.TB, gitPath string) SeededRepo {
	tb.Helper()
	repoPath := tb.TempDir()
	runGit(tb, gitPath, repoPath, "init", "-b", "main")
	runGit(tb, gitPath, repoPath, "config", "user.name", "Looper E2E")
	runGit(tb, gitPath, repoPath, "config", "user.email", "looper-e2e@example.com")
	runGit(tb, gitPath, repoPath, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("# looper e2e\n"), 0o644); err != nil {
		tb.Fatalf("write seed file: %v", err)
	}
	runGit(tb, gitPath, repoPath, "add", "README.md")
	runGit(tb, gitPath, repoPath, "commit", "-m", "initial commit")
	return SeededRepo{Path: repoPath, DefaultBranch: "main", InitialCommit: strings.TrimSpace(runGit(tb, gitPath, repoPath, "rev-parse", "HEAD"))}
}

func CreateBareOrigin(tb testing.TB, gitPath string, repoPath string) string {
	tb.Helper()
	originPath := filepath.Join(tb.TempDir(), "origin.git")
	runGit(tb, gitPath, repoPath, "clone", "--bare", repoPath, originPath)
	runGit(tb, gitPath, repoPath, "remote", "add", "origin", originPath)
	runGit(tb, gitPath, repoPath, "push", "-u", "origin", "main")
	return originPath
}

func CreateBranchCommitAndPush(tb testing.TB, gitPath string, repoPath string, branch string, filePath string, content string) string {
	tb.Helper()
	runGit(tb, gitPath, repoPath, "checkout", "-b", branch)
	fullPath := filepath.Join(repoPath, filePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		tb.Fatalf("mkdir branch file dir: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		tb.Fatalf("write branch file: %v", err)
	}
	runGit(tb, gitPath, repoPath, "add", filePath)
	runGit(tb, gitPath, repoPath, "commit", "-m", "seed feature branch")
	sha := strings.TrimSpace(runGit(tb, gitPath, repoPath, "rev-parse", "HEAD"))
	runGit(tb, gitPath, repoPath, "push", "-u", "origin", branch)
	runGit(tb, gitPath, repoPath, "checkout", "main")
	return sha
}

func SnapshotRepo(tb testing.TB, gitPath string, repoPath string) RepoSnapshot {
	tb.Helper()
	return RepoSnapshot{
		Head:             strings.TrimSpace(runGit(tb, gitPath, repoPath, "rev-parse", "HEAD")),
		StatusPorcelain:  runGit(tb, gitPath, repoPath, "status", "--porcelain=v1", "--untracked-files=all"),
		IndexTree:        strings.TrimSpace(runGit(tb, gitPath, repoPath, "write-tree")),
		CurrentBranch:    strings.TrimSpace(runGit(tb, gitPath, repoPath, "branch", "--show-current")),
		WorktreeListText: runGit(tb, gitPath, repoPath, "worktree", "list", "--porcelain"),
	}
}

func runGit(tb testing.TB, gitPath string, cwd string, args ...string) string {
	tb.Helper()
	output, err := runCommand(gitPath, cwd, nil, args...)
	if err != nil {
		tb.Fatalf("git %v in %s: %v", args, cwd, err)
	}
	return output
}

func runCommand(command string, cwd string, env []string, args ...string) (string, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = cwd
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

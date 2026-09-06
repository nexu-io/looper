package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops/failureclass"
)

func forgejoPullRequestHasConflicts(ctx context.Context, cfg *config.Config, repo, cwd string, pr forge.PullRequest) (bool, error) {
	if pr.Mergeable == nil || *pr.Mergeable || !strings.EqualFold(pr.State, "open") {
		return false, nil
	}
	// Forgejo's mergeable=false also includes CHECKING, server-side ERROR and
	// WIP. Only an actual merge of the exact base/head commits can establish a
	// conflict; the check never checks out or merges into the caller's worktree.
	gitPath, cwd, err := forgejoGitLocation(cfg, repo, cwd)
	if err != nil {
		return false, err
	}
	conflicted, err := forgejoHasMergeConflicts(ctx, gitPath, cwd, pr.Base.SHA, pr.Head.SHA)
	// Preserve remote fetch failures and keep transient local launch pressure retryable.
	return conflicted, withForgejoConflictBoundary(err)
}

func withForgejoConflictBoundary(err error) error {
	boundary := failureclass.BoundaryGitLocal
	if shell.IsTransientStartFailure(err) {
		boundary = failureclass.BoundaryGitRemote
	}
	return failureclass.WithBoundary(err, boundary)
}

// forgejoHasMergeConflicts checks exact commits without checking out branches or
// touching the caller's index, worktree, refs, or merge state. Missing commits
// are fetched into the object database; merge-tree may also write tree objects.
// It requires Git's merge-tree --write-tree support (Git 2.38 or later).
func forgejoHasMergeConflicts(ctx context.Context, gitPath, repoPath, baseSHA, headSHA string) (bool, error) {
	commits := []string{strings.TrimSpace(baseSHA), strings.TrimSpace(headSHA)}
	if err := ensureForgejoCommits(ctx, gitPath, repoPath, commits); err != nil {
		return false, err
	}
	result, err := shell.Run(ctx, shell.Options{Command: gitPath, CWD: repoPath, Args: []string{"merge-tree", "--write-tree", commits[0], commits[1]}})
	if err == nil {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && result.ExitCode == 1 {
		return true, nil
	}
	return false, fmt.Errorf("check merge conflicts: %w", err)
}

func ensureForgejoCommits(ctx context.Context, gitPath, repoPath string, commits []string) error {
	for _, sha := range commits {
		if len(sha) != 40 && len(sha) != 64 {
			return fmt.Errorf("Forgejo commit inspection requires full base and head commit IDs")
		}
		if _, err := hex.DecodeString(sha); err != nil {
			return fmt.Errorf("Forgejo commit inspection requires hexadecimal commit IDs: %w", err)
		}
	}
	missing, err := missingForgejoCommits(ctx, gitPath, repoPath, commits)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		// Empty refmap and no FETCH_HEAD write keep the caller's refs unchanged.
		args := append([]string{"fetch", "--no-write-fetch-head", "--no-tags", "--refmap=", "origin"}, missing...)
		if _, err := shell.Run(ctx, shell.Options{Command: gitPath, CWD: repoPath, Args: args}); err != nil {
			boundary := failureclass.BoundaryGitRemote
			if shell.IsStartFailure(err) && !shell.IsTransientStartFailure(err) {
				boundary = failureclass.BoundaryGitLocal
			}
			return failureclass.WithBoundary(fmt.Errorf("fetch commits for Forgejo commit inspection: %w", err), boundary)
		}
		missing, err = missingForgejoCommits(ctx, gitPath, repoPath, commits)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("Forgejo commit inspection commits still unavailable after fetch: %s", strings.Join(missing, ", "))
		}
	}
	return nil
}

func missingForgejoCommits(ctx context.Context, gitPath, repoPath string, commits []string) ([]string, error) {
	// Batch-check reports a missing object on stdout with exit zero. Ordinary
	// git failures (bad CWD, permissions, unavailable binary) remain errors and
	// do not incorrectly trigger a fetch or become a conflict verdict.
	result, err := shell.Run(ctx, shell.Options{
		Command: gitPath, CWD: repoPath,
		Args:  []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"},
		Stdin: strings.Join(commits, "\n") + "\n",
		Env:   map[string]string{"GIT_NO_LAZY_FETCH": "1"},
	})
	if err != nil {
		return nil, fmt.Errorf("inspect commits for merge conflict check: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != len(commits) {
		return nil, fmt.Errorf("inspect commits for merge conflict check: unexpected cat-file output")
	}
	var missing []string
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.EqualFold(fields[0], commits[i]) {
			return nil, fmt.Errorf("inspect commits for merge conflict check: unexpected cat-file output")
		}
		switch fields[1] {
		case "commit":
		case "missing":
			if len(missing) == 0 || missing[0] != commits[i] {
				missing = append(missing, commits[i])
			}
		default:
			return nil, fmt.Errorf("merge conflict check object %s is %s, want commit", commits[i], fields[1])
		}
	}
	return missing, nil
}

func forgejoGitLocation(cfg *config.Config, repo, cwd string) (gitPath, location string, err error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" && cfg != nil {
		for _, project := range cfg.Projects {
			if strings.EqualFold(strings.TrimSpace(project.Repo), strings.TrimSpace(repo)) {
				if cwd != "" {
					return "", "", failureclass.WithBoundary(fmt.Errorf("merge conflict check for %s requires an unambiguous project path", repo), failureclass.BoundaryGitLocal)
				}
				cwd = strings.TrimSpace(project.RepoPath)
			}
		}
	}
	if cwd == "" {
		return "", "", failureclass.WithBoundary(fmt.Errorf("merge conflict check for %s requires project repo path", repo), failureclass.BoundaryGitLocal)
	}
	gitPath = ""
	if cfg != nil {
		gitPath = derefString(cfg.Tools.GitPath)
	}
	if strings.TrimSpace(gitPath) == "" {
		gitPath = "git"
	}
	return gitPath, cwd, nil
}

package worktreecleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

type GitGateway interface {
	IsWorktreeClean(context.Context, string) (bool, error)
	CleanupWorktree(context.Context, gitinfra.CleanupWorktreeInput) error
}

type Options struct {
	Config config.Config
	Repos  *storage.Repositories
	Git    GitGateway
	DryRun bool
	Now    func() time.Time
}

type Summary struct {
	Inspected int `json:"inspected"`
	Eligible  int `json:"eligible"`
	Cleaned   int `json:"cleaned"`
	Skipped   int `json:"skipped"`
	Errors    int `json:"errors"`
}

type Candidate struct {
	ID           string `json:"id"`
	ProjectID    string `json:"projectId"`
	RepoPath     string `json:"repoPath"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
	Error        string `json:"error,omitempty"`
}

type Result struct {
	DryRun     bool        `json:"dryRun"`
	Summary    Summary     `json:"summary"`
	Candidates []Candidate `json:"candidates"`
}

func Run(ctx context.Context, options Options) (Result, error) {
	if options.Repos == nil || options.Repos.Worktrees == nil {
		return Result{}, fmt.Errorf("repositories are required")
	}
	if options.Git == nil {
		return Result{}, fmt.Errorf("git gateway is required")
	}

	plan, err := (&Service{
		Repos:  options.Repos,
		Config: options.Config.Daemon.WorktreeCleanup,
		Now:    options.Now,
	}).Plan(ctx)
	if err != nil {
		return Result{}, err
	}

	projects := make(map[string]config.ProjectRefConfig, len(options.Config.Projects))
	for _, project := range options.Config.Projects {
		projects[project.ID] = project
	}

	result := Result{DryRun: options.DryRun}
	for _, decision := range plan.Decisions {
		project, ok := projects[decision.Worktree.ProjectID]
		if !ok {
			result.Summary.Inspected++
			result.Summary.Skipped++
			result.Candidates = append(result.Candidates, Candidate{
				ID:           decision.Worktree.ID,
				ProjectID:    decision.Worktree.ProjectID,
				RepoPath:     decision.Worktree.RepoPath,
				WorktreePath: decision.Worktree.WorktreePath,
				Branch:       decision.Worktree.Branch,
				Action:       "skip",
				Reason:       "project_not_configured",
			})
			continue
		}
		worktreeRoot, err := worktreeRootForProject(project)
		if err != nil {
			return Result{}, err
		}
		candidate := candidateFromDecision(decision)
		result.Summary.Inspected++
		if decision.Action != ActionWouldClean {
			result.Summary.Skipped++
			result.Candidates = append(result.Candidates, candidate)
			continue
		}

		candidate = inspectCandidate(ctx, options.Git, decision.Worktree, worktreeRoot)
		if candidate.Action == "clean" {
			result.Summary.Eligible++
			if !options.DryRun {
				if err := options.Git.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{
					ProjectID:         decision.Worktree.ProjectID,
					RepoPath:          decision.Worktree.RepoPath,
					WorktreeRoot:      worktreeRoot,
					WorktreePath:      decision.Worktree.WorktreePath,
					Branch:            decision.Worktree.Branch,
					ProtectedBranches: protectedBranches(project),
				}); err != nil {
					candidate.Action = "error"
					candidate.Reason = "cleanup_failed"
					candidate.Error = err.Error()
					result.Summary.Errors++
				} else {
					result.Summary.Cleaned++
				}
			}
		} else if candidate.Action == "error" {
			result.Summary.Errors++
		} else {
			result.Summary.Skipped++
		}
		result.Candidates = append(result.Candidates, candidate)
	}

	return result, nil
}

func candidateFromDecision(decision Decision) Candidate {
	return Candidate{
		ID:           decision.Worktree.ID,
		ProjectID:    decision.Worktree.ProjectID,
		RepoPath:     decision.Worktree.RepoPath,
		WorktreePath: decision.Worktree.WorktreePath,
		Branch:       decision.Worktree.Branch,
		Action:       "skip",
		Reason:       decision.Reason,
	}
}

func worktreeRootForProject(project config.ProjectRefConfig) (string, error) {
	worktreeRoot := strings.TrimSpace(derefString(project.WorktreeRoot))
	if worktreeRoot != "" {
		return worktreeRoot, nil
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
}

func inspectCandidate(ctx context.Context, git GitGateway, record storage.WorktreeRecord, worktreeRoot string) Candidate {
	candidate := Candidate{
		ID:           record.ID,
		ProjectID:    record.ProjectID,
		RepoPath:     record.RepoPath,
		WorktreePath: record.WorktreePath,
		Branch:       record.Branch,
		Action:       "skip",
	}
	if record.Status == "cleaned" || record.CleanedAt != nil {
		candidate.Reason = "already_cleaned"
		return candidate
	}
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: record.WorktreePath, RepoPath: record.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
		candidate.Reason = "unsafe_path"
		candidate.Error = err.Error()
		return candidate
	}
	if _, err := os.Stat(record.WorktreePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			candidate.Reason = "missing_path"
			return candidate
		}
		candidate.Action = "error"
		candidate.Reason = "stat_failed"
		candidate.Error = err.Error()
		return candidate
	}
	clean, err := git.IsWorktreeClean(ctx, record.WorktreePath)
	if err != nil {
		candidate.Action = "error"
		candidate.Reason = "status_failed"
		candidate.Error = err.Error()
		return candidate
	}
	if !clean {
		candidate.Reason = "dirty_worktree"
		return candidate
	}
	candidate.Action = "clean"
	candidate.Reason = "terminal_clean"
	return candidate
}

func protectedBranches(project config.ProjectRefConfig) []string {
	branches := []string{}
	if base := strings.TrimSpace(derefString(project.BaseBranch)); base != "" {
		branches = append(branches, base)
	}
	return branches
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

package worktreecleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

	activeRefs, err := collectActiveReferences(ctx, options.Repos)
	if err != nil {
		return Result{}, err
	}

	result := Result{DryRun: options.DryRun}
	for _, project := range options.Config.Projects {
		worktreeRoot := strings.TrimSpace(derefString(project.WorktreeRoot))
		if worktreeRoot == "" {
			worktreeRoot, err = config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
			if err != nil {
				return Result{}, err
			}
		}

		records, err := options.Repos.Worktrees.ListByProject(ctx, project.ID)
		if err != nil {
			return Result{}, err
		}
		sort.Slice(records, func(i, j int) bool {
			if records[i].ProjectID != records[j].ProjectID {
				return records[i].ProjectID < records[j].ProjectID
			}
			return records[i].WorktreePath < records[j].WorktreePath
		})
		for _, record := range records {
			candidate := inspectCandidate(ctx, options.Git, record, worktreeRoot, activeRefs)
			result.Summary.Inspected++
			if candidate.Action == "clean" {
				result.Summary.Eligible++
				if !options.DryRun {
					if err := options.Git.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{
						ProjectID:         record.ProjectID,
						RepoPath:          record.RepoPath,
						WorktreeRoot:      worktreeRoot,
						WorktreePath:      record.WorktreePath,
						Branch:            record.Branch,
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
	}

	return result, nil
}

func inspectCandidate(ctx context.Context, git GitGateway, record storage.WorktreeRecord, worktreeRoot string, activeRefs activeReferences) Candidate {
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
	if activeRefs.references(record) {
		candidate.Reason = "active_loop"
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

type activeReferences struct {
	ids   map[string]struct{}
	paths map[string]struct{}
}

func collectActiveReferences(ctx context.Context, repos *storage.Repositories) (activeReferences, error) {
	refs := activeReferences{ids: map[string]struct{}{}, paths: map[string]struct{}{}}
	if repos.Loops == nil || repos.Runs == nil {
		return refs, nil
	}
	loops, err := repos.Loops.List(ctx)
	if err != nil {
		return refs, err
	}
	for _, loop := range loops {
		latestRun, err := repos.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return refs, err
		}
		if isTerminalLoop(loop.Status) && (latestRun == nil || latestRun.Status != "running") {
			continue
		}
		collectJSONStringRefs(loop.MetadataJSON, refs)
		if latestRun != nil {
			collectJSONStringRefs(latestRun.CheckpointJSON, refs)
		}
	}
	return refs, nil
}

func collectJSONStringRefs(raw *string, refs activeReferences) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return
	}
	var decoded any
	if err := json.Unmarshal([]byte(*raw), &decoded); err != nil {
		return
	}
	walkJSON(decoded, "", refs)
}

func walkJSON(value any, key string, refs activeReferences) {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			walkJSON(child, childKey, refs)
		}
	case []any:
		for _, child := range typed {
			walkJSON(child, key, refs)
		}
	case string:
		switch key {
		case "id":
			if strings.HasPrefix(typed, "worktree") {
				refs.ids[typed] = struct{}{}
			}
		case "path", "worktreePath":
			refs.paths[normalizePath(typed)] = struct{}{}
		}
	}
}

func (r activeReferences) references(record storage.WorktreeRecord) bool {
	if _, ok := r.ids[record.ID]; ok {
		return true
	}
	_, ok := r.paths[normalizePath(record.WorktreePath)]
	return ok
}

func isTerminalLoop(status string) bool {
	switch status {
	case "completed", "failed", "interrupted", "terminated", "stopped":
		return true
	default:
		return false
	}
}

func protectedBranches(project config.ProjectRefConfig) []string {
	branches := []string{}
	if base := strings.TrimSpace(derefString(project.BaseBranch)); base != "" {
		branches = append(branches, base)
	}
	return branches
}

func normalizePath(path string) string {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

type worktreeDiscardResult struct {
	WorktreePath *string `json:"worktreePath,omitempty"`
	Discarded    bool    `json:"discarded"`
	NoOp         bool    `json:"noOp"`
	Reason       string  `json:"reason,omitempty"`
}

type checkpointWorktreeRef struct {
	ID     string `json:"id,omitempty"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// checkpointWithWorktree extracts worktree location hints from a run checkpoint.
// When prepare-worktree fails on a dirty tree it often returns before writing
// checkpoint.worktree, leaving only work.branch (worker) or detail.headRefName
// (fixer). Those branch hints still resolve the managed worktree row.
type checkpointWithWorktree struct {
	Worktree *checkpointWorktreeRef `json:"worktree,omitempty"`
	Work     *struct {
		Branch string `json:"branch,omitempty"`
	} `json:"work,omitempty"`
	Detail *struct {
		HeadRefName string `json:"headRefName,omitempty"`
	} `json:"detail,omitempty"`
}

// discardLoopWorktreeChanges performs the operator opt-in dirty-worktree discard
// for loop retry. Planner loops and loops without a resolvable managed worktree
// are no-ops. Active run/queue must already be refused by the caller.
func (h *Handler) discardLoopWorktreeChanges(ctx context.Context, services looperdruntime.Services, loop storage.LoopRecord) (worktreeDiscardResult, error) {
	if loop.Type == string(domain.LoopTypePlanner) {
		return worktreeDiscardResult{NoOp: true, Reason: "planner_no_worktree"}, nil
	}
	if loop.Type != string(domain.LoopTypeFixer) && loop.Type != string(domain.LoopTypeReviewer) && loop.Type != string(domain.LoopTypeWorker) {
		return worktreeDiscardResult{NoOp: true, Reason: "loop_type_without_worktree"}, nil
	}
	if services.Repositories == nil {
		return worktreeDiscardResult{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}

	project, err := requireActiveProjectRecord(ctx, services.Repositories.Projects, loop.ProjectID)
	if err != nil {
		return worktreeDiscardResult{}, err
	}

	resolved, err := resolveManagedWorktreeForLoop(ctx, services.Repositories, *project, loop)
	if err != nil {
		return worktreeDiscardResult{}, err
	}
	if resolved == nil || strings.TrimSpace(resolved.Path) == "" {
		return worktreeDiscardResult{NoOp: true, Reason: "no_worktree"}, nil
	}

	if err := worktreesafety.Validate(worktreesafety.CheckInput{
		WorktreePath: resolved.Path,
		RepoPath:     project.RepoPath,
		WorktreeRoot: resolved.WorktreeRoot,
	}); err != nil {
		return worktreeDiscardResult{}, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: fmt.Sprintf("Cannot discard worktree changes for loop %s: %v", loop.ID, err),
		}
	}
	if sameFilesystemPath(resolved.Path, project.RepoPath) {
		return worktreeDiscardResult{}, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: fmt.Sprintf("Cannot discard worktree changes for loop %s: path must not equal project repo path", loop.ID),
		}
	}
	if !resolved.Managed {
		return worktreeDiscardResult{}, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: fmt.Sprintf("Cannot discard worktree changes for loop %s: path %s is not a Looper-managed worktree", loop.ID, resolved.Path),
		}
	}

	gitPath := ""
	if h.context.Config.Tools.GitPath != nil {
		gitPath = strings.TrimSpace(*h.context.Config.Tools.GitPath)
	}
	gateway := gitinfra.New(gitinfra.Options{GitPath: gitPath, Repos: services.Repositories, Now: h.now})
	discard, err := gateway.DiscardWorktreeChanges(ctx, gitinfra.DiscardWorktreeChangesInput{
		RepoPath:     project.RepoPath,
		WorktreeRoot: resolved.WorktreeRoot,
		WorktreePath: resolved.Path,
	})
	if err != nil {
		return worktreeDiscardResult{}, apiError{
			code:    pkgapi.ErrorCodeInternalError,
			status:  http.StatusInternalServerError,
			message: fmt.Sprintf("Failed to discard worktree changes at %s: %v", resolved.Path, err),
		}
	}

	path := discard.WorktreePath
	if path == "" {
		path = resolved.Path
	}
	result := worktreeDiscardResult{
		WorktreePath: stringPtrOrNil(path),
		Discarded:    !discard.NoOp,
		NoOp:         discard.NoOp,
	}
	if discard.NoOp {
		if _, statErr := os.Stat(path); statErr != nil && os.IsNotExist(statErr) {
			result.Reason = "worktree_missing"
		} else {
			result.Reason = "already_clean"
		}
	} else {
		result.Reason = "discarded"
	}

	projectID := loop.ProjectID
	loopID := loop.ID
	payload := map[string]any{
		"worktreePath": path,
		"branch":       resolved.Branch,
		"noOp":         result.NoOp,
		"reason":       result.Reason,
		"discarded":    result.Discarded,
		"source":       resolved.Source,
	}
	if resolved.ID != "" {
		payload["worktreeId"] = resolved.ID
	}
	// Audit is best-effort: git discard already succeeded, and retry must still
	// requeue. A transient events write failure must not strand the operator.
	_ = eventlog.Append(ctx, services.Repositories, eventlog.AppendInput{
		EventType: "looper.worktree.changes_discarded",
		ProjectID: &projectID,
		LoopID:    &loopID,
		ActorType: stringPtrOrNil("operator"),
		ActorID:   stringPtrOrNil("cli"),
		Payload:   payload,
		CreatedAt: h.now().UTC(),
	})
	return result, nil
}

type managedWorktreeRef struct {
	Path         string
	Branch       string
	ID           string
	WorktreeRoot string
	Source       string
	Managed      bool
}

func resolveManagedWorktreeForLoop(ctx context.Context, repos *storage.Repositories, project storage.ProjectRecord, loop storage.LoopRecord) (*managedWorktreeRef, error) {
	worktreeRoot, err := projectWorktreeRoot(project)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	var fromCheckpoint *checkpointWorktreeRef
	latestRun, err := repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return nil, err
	}
	if latestRun != nil && latestRun.CheckpointJSON != nil {
		fromCheckpoint = parseCheckpointWorktree(latestRun.CheckpointJSON)
	}

	var record *storage.WorktreeRecord
	if fromCheckpoint != nil {
		if id := strings.TrimSpace(fromCheckpoint.ID); id != "" {
			record, err = repos.Worktrees.GetByID(ctx, id)
			if err != nil {
				return nil, err
			}
		}
		if record == nil {
			if branch := strings.TrimSpace(fromCheckpoint.Branch); branch != "" {
				record, err = repos.Worktrees.GetByBranch(ctx, project.ID, branch)
				if err != nil {
					return nil, err
				}
			}
		}
		if record == nil {
			path := strings.TrimSpace(fromCheckpoint.Path)
			if path != "" {
				record, err = findProjectWorktreeByPath(ctx, repos, project.ID, path)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	path := ""
	branch := ""
	id := ""
	source := ""
	if record != nil && strings.TrimSpace(record.WorktreePath) != "" && record.CleanedAt == nil {
		path = strings.TrimSpace(record.WorktreePath)
		branch = strings.TrimSpace(record.Branch)
		id = record.ID
		source = "worktree_record"
	}
	if path == "" && fromCheckpoint != nil && strings.TrimSpace(fromCheckpoint.Path) != "" {
		path = strings.TrimSpace(fromCheckpoint.Path)
		branch = strings.TrimSpace(fromCheckpoint.Branch)
		id = strings.TrimSpace(fromCheckpoint.ID)
		source = "checkpoint"
	}
	if path == "" {
		return nil, nil
	}

	managed := false
	if worktreeRoot != "" && worktreesafety.IsSafe(worktreesafety.CheckInput{
		WorktreePath: path,
		RepoPath:     project.RepoPath,
		WorktreeRoot: worktreeRoot,
	}) {
		managed = true
	}
	if record != nil && sameFilesystemPath(record.WorktreePath, path) && record.ProjectID == project.ID {
		// Recorded worktree rows for this project are managed, even when the
		// path only barely satisfies root checks via the stored record.
		if worktreesafety.IsSafe(worktreesafety.CheckInput{
			WorktreePath: path,
			RepoPath:     project.RepoPath,
			WorktreeRoot: worktreeRoot,
		}) {
			managed = true
		}
	}

	return &managedWorktreeRef{
		Path:         path,
		Branch:       branch,
		ID:           id,
		WorktreeRoot: worktreeRoot,
		Source:       source,
		Managed:      managed,
	}, nil
}

func parseCheckpointWorktree(raw *string) *checkpointWorktreeRef {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	var checkpoint checkpointWithWorktree
	if err := json.Unmarshal([]byte(*raw), &checkpoint); err != nil {
		return nil
	}
	path := ""
	branch := ""
	id := ""
	if checkpoint.Worktree != nil {
		path = strings.TrimSpace(checkpoint.Worktree.Path)
		branch = strings.TrimSpace(checkpoint.Worktree.Branch)
		id = strings.TrimSpace(checkpoint.Worktree.ID)
	}
	// Dirty prepare-worktree often aborts before checkpoint.worktree is set.
	// Prefer an explicit worktree.branch, then worker work.branch, then fixer
	// detail.headRefName so GetByBranch can still locate the managed row.
	if branch == "" && checkpoint.Work != nil {
		branch = strings.TrimSpace(checkpoint.Work.Branch)
	}
	if branch == "" && checkpoint.Detail != nil {
		branch = strings.TrimSpace(checkpoint.Detail.HeadRefName)
	}
	if path == "" && branch == "" && id == "" {
		return nil
	}
	return &checkpointWorktreeRef{ID: id, Path: path, Branch: branch}
}

func findProjectWorktreeByPath(ctx context.Context, repos *storage.Repositories, projectID, path string) (*storage.WorktreeRecord, error) {
	items, err := repos.Worktrees.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].CleanedAt != nil {
			continue
		}
		if sameFilesystemPath(items[i].WorktreePath, path) {
			return &items[i], nil
		}
	}
	return nil, nil
}

func projectWorktreeRoot(project storage.ProjectRecord) (string, error) {
	if project.MetadataJSON != nil && strings.TrimSpace(*project.MetadataJSON) != "" {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(*project.MetadataJSON), &metadata); err == nil {
			if value, ok := metadata["worktreeRoot"].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value), nil
			}
		}
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
}

func sameFilesystemPath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

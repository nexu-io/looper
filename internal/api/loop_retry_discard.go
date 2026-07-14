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
	ID       string `json:"id,omitempty"`
	Path     string `json:"path,omitempty"`
	Branch   string `json:"branch,omitempty"`
	PRNumber int64  `json:"prNumber,omitempty"`
}

// checkpointWithWorktree extracts worktree location hints from a run checkpoint.
// When prepare-worktree fails on a dirty tree it often returns before writing
// checkpoint.worktree, leaving only work.branch (worker) or detail.headRefName
// (fixer). Those branch hints still resolve the managed worktree row.
// For push-existing workers, work.branch may be empty while the worktree was
// created under pr-<PRNumber>; executionMode + prNumber recover that branch.
// PRNumber (from work.prNumber or detail.prNumber) disambiguates branch-only
// lookups when multiple PRs share a head branch name.
type checkpointWithWorktree struct {
	Worktree *checkpointWorktreeRef `json:"worktree,omitempty"`
	Work     *struct {
		Branch        string `json:"branch,omitempty"`
		ExecutionMode string `json:"executionMode,omitempty"`
		PRNumber      int64  `json:"prNumber,omitempty"`
	} `json:"work,omitempty"`
	Detail *struct {
		HeadRefName string `json:"headRefName,omitempty"`
		PRNumber    int64  `json:"prNumber,omitempty"`
	} `json:"detail,omitempty"`
}

// discardLoopWorktreeChanges performs the operator opt-in dirty-worktree discard
// for loop retry. Agent loops (planner/fixer/reviewer/worker) resolve a managed
// worktree from the latest run checkpoint or worktree row; loops without a
// resolvable managed worktree are no-ops. Active run/queue must already be
// refused by the caller.
func (h *Handler) discardLoopWorktreeChanges(ctx context.Context, services looperdruntime.Services, loop storage.LoopRecord) (worktreeDiscardResult, error) {
	if loop.Type != string(domain.LoopTypePlanner) && loop.Type != string(domain.LoopTypeFixer) && loop.Type != string(domain.LoopTypeReviewer) && loop.Type != string(domain.LoopTypeWorker) {
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

	// Prefer loop.PRNumber, then checkpoint PR hints. GitHub head branch names are
	// not unique across forks/PRs; CreateWorktree embeds pr-<N> in the directory.
	prNumber := int64(0)
	if loop.PRNumber != nil && *loop.PRNumber > 0 {
		prNumber = *loop.PRNumber
	}
	if prNumber == 0 && fromCheckpoint != nil && fromCheckpoint.PRNumber > 0 {
		prNumber = fromCheckpoint.PRNumber
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
			path := strings.TrimSpace(fromCheckpoint.Path)
			if path != "" {
				record, err = findProjectWorktreeByPath(ctx, repos, project.ID, path)
				if err != nil {
					return nil, err
				}
			}
		}
		if record == nil {
			if branch := strings.TrimSpace(fromCheckpoint.Branch); branch != "" {
				record, err = findProjectWorktreeByBranch(ctx, repos, project.ID, branch, prNumber)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	// When checkpoint only has a branch (or none) but the loop is PR-scoped,
	// resolve via PR-tagged managed path so we never pick a sibling PR's row.
	if record == nil && prNumber > 0 {
		record, err = findProjectWorktreeByPR(ctx, repos, project.ID, prNumber)
		if err != nil {
			return nil, err
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
	prNumber := int64(0)
	if checkpoint.Worktree != nil {
		path = strings.TrimSpace(checkpoint.Worktree.Path)
		branch = strings.TrimSpace(checkpoint.Worktree.Branch)
		id = strings.TrimSpace(checkpoint.Worktree.ID)
	}
	// Dirty prepare-worktree often aborts before checkpoint.worktree is set.
	// Prefer an explicit worktree.branch, then worker work.branch, then
	// push-existing pr-<N> (CreateWorktree branch when work.branch is empty),
	// then fixer detail.headRefName so branch/PR lookup can still locate the row.
	if checkpoint.Work != nil && checkpoint.Work.PRNumber > 0 {
		prNumber = checkpoint.Work.PRNumber
	}
	if prNumber == 0 && checkpoint.Detail != nil && checkpoint.Detail.PRNumber > 0 {
		prNumber = checkpoint.Detail.PRNumber
	}
	if branch == "" && checkpoint.Work != nil {
		branch = strings.TrimSpace(checkpoint.Work.Branch)
		if branch == "" &&
			strings.TrimSpace(checkpoint.Work.ExecutionMode) == "push-existing" &&
			checkpoint.Work.PRNumber > 0 {
			// Matches worker runPrepareWorktreeStep when work.Branch is empty.
			branch = fmt.Sprintf("pr-%d", checkpoint.Work.PRNumber)
		}
	}
	if branch == "" && checkpoint.Detail != nil {
		branch = strings.TrimSpace(checkpoint.Detail.HeadRefName)
	}
	if path == "" && branch == "" && id == "" && prNumber == 0 {
		return nil
	}
	return &checkpointWorktreeRef{ID: id, Path: path, Branch: branch, PRNumber: prNumber}
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

// findProjectWorktreeByBranch resolves an active worktree row for project+branch.
// When prNumber is known, refuse rows whose CreateWorktree directory embeds a
// different pr-<M>: GitHub head branch names are not unique across PRs, and the
// unique (project_id, branch) index means the stored row may belong to a sibling
// PR that last claimed the branch. Prefer path/branch markers for pr-<N>.
func findProjectWorktreeByBranch(ctx context.Context, repos *storage.Repositories, projectID, branch string, prNumber int64) (*storage.WorktreeRecord, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, nil
	}
	items, err := repos.Worktrees.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var matches []storage.WorktreeRecord
	for i := range items {
		if items[i].CleanedAt != nil {
			continue
		}
		if strings.TrimSpace(items[i].Branch) != branch {
			continue
		}
		matches = append(matches, items[i])
	}
	if prNumber <= 0 {
		return pickUniqueActiveWorktree(matches), nil
	}
	var preferred []storage.WorktreeRecord
	for i := range matches {
		if worktreeBelongsToPR(matches[i], prNumber) {
			preferred = append(preferred, matches[i])
		}
	}
	if len(preferred) > 0 {
		return pickUniqueActiveWorktree(preferred), nil
	}
	// Known PR but no row clearly owned by it: do not discard a sibling PR path.
	return nil, nil
}

// worktreeBelongsToPR reports whether a worktree row is owned by the given PR.
// True only when ownership is proven: the directory embeds pr-<N>, or the branch
// is bare pr-<N> (push-existing). Untagged paths are refused when prNumber is
// known — RestoreWorktree can adopt a branch checkout without a pr-<N> path, and
// accepting those would let two PRs that share a head branch name discard each
// other's untagged worktree.
func worktreeBelongsToPR(record storage.WorktreeRecord, prNumber int64) bool {
	if prNumber <= 0 {
		return true
	}
	if worktreePathMatchesPR(record.WorktreePath, prNumber) {
		return true
	}
	if strings.TrimSpace(record.Branch) == fmt.Sprintf("pr-%d", prNumber) {
		return true
	}
	// Known PR scope: untagged or differently-tagged paths cannot prove ownership.
	return false
}

// findProjectWorktreeByPR finds the active managed worktree whose path embeds
// pr-<N>, used when branch lookup is empty or ambiguous but the loop is PR-scoped.
func findProjectWorktreeByPR(ctx context.Context, repos *storage.Repositories, projectID string, prNumber int64) (*storage.WorktreeRecord, error) {
	if prNumber <= 0 {
		return nil, nil
	}
	items, err := repos.Worktrees.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var matches []storage.WorktreeRecord
	for i := range items {
		if items[i].CleanedAt != nil {
			continue
		}
		if worktreePathMatchesPR(items[i].WorktreePath, prNumber) {
			matches = append(matches, items[i])
		}
	}
	return pickUniqueActiveWorktree(matches), nil
}

func worktreePathMatchesPR(worktreePath string, prNumber int64) bool {
	if prNumber <= 0 {
		return false
	}
	base := filepath.Base(strings.TrimSpace(worktreePath))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return false
	}
	// CreateWorktree dirs: looper-fix-<project>-pr-<N>[ -detached]
	// Worker push-existing may also use bare pr-<N>.
	marker := fmt.Sprintf("-pr-%d", prNumber)
	if strings.HasSuffix(base, marker) || strings.HasSuffix(base, marker+"-detached") {
		return true
	}
	return base == fmt.Sprintf("pr-%d", prNumber)
}

func worktreePathHasPRMarker(worktreePath string) bool {
	_, ok := worktreePathEmbeddedPR(worktreePath)
	return ok
}

// worktreePathEmbeddedPR extracts pr-<N> from a CreateWorktree directory base name.
func worktreePathEmbeddedPR(worktreePath string) (int64, bool) {
	base := filepath.Base(strings.TrimSpace(worktreePath))
	if base == "" || base == "." {
		return 0, false
	}
	// Bare pr-<N>
	var bare int64
	if _, err := fmt.Sscanf(base, "pr-%d", &bare); err == nil && bare > 0 && base == fmt.Sprintf("pr-%d", bare) {
		return bare, true
	}
	// …-pr-<N> or …-pr-<N>-detached
	const marker = "-pr-"
	idx := strings.LastIndex(base, marker)
	if idx < 0 {
		return 0, false
	}
	rest := base[idx+len(marker):]
	rest = strings.TrimSuffix(rest, "-detached")
	var n int64
	if _, err := fmt.Sscanf(rest, "%d", &n); err != nil || n <= 0 {
		return 0, false
	}
	if rest != fmt.Sprintf("%d", n) {
		return 0, false
	}
	return n, true
}

func pickUniqueActiveWorktree(matches []storage.WorktreeRecord) *storage.WorktreeRecord {
	switch len(matches) {
	case 0:
		return nil
	case 1:
		return &matches[0]
	default:
		// Prefer most recently updated among remaining PR-scoped matches.
		best := 0
		for i := 1; i < len(matches); i++ {
			if matches[i].UpdatedAt > matches[best].UpdatedAt {
				best = i
			}
		}
		return &matches[best]
	}
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

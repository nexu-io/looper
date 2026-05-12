package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

const javaScriptISOStringLayout = "2006-01-02T15:04:05.000Z"

var (
	missingWorktreeErrorPattern = regexp.MustCompile(`(?i)is not a working tree|does not exist|not found|no such file`)
	pushConflictErrorPattern    = regexp.MustCompile(`(?i)stale info|non-fast-forward|failed to push|rejected`)
)

type CheckoutMode string

const (
	CheckoutModeBranch   CheckoutMode = "branch"
	CheckoutModeDetached CheckoutMode = "detached"
)

type Options struct {
	GitPath string
	Repos   *storage.Repositories
	Now     func() time.Time
}

type Gateway struct {
	gitPath string
	repos   *storage.Repositories
	now     func() time.Time
}

type CreateWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
	Branch            string
	BaseBranch        string
	PRNumber          int64
	ProtectedBranches []string
	CheckoutMode      CheckoutMode
}

type RestoreWorktreeInput struct {
	ProjectID            string
	RepoPath             string
	Branch               string
	WorktreeRoot         string
	CheckoutMode         CheckoutMode
	ExpectedWorktreePath string
}

type PrepareWorktreeInput struct {
	RepoPath        string
	WorktreeRoot    string
	WorktreePath    string
	Branch          string
	Ref             string
	ExpectedHeadSHA string
	Remote          string
}

type PrepareWorktreeResult struct {
	HeadSHA string
	Clean   bool
}

type InspectHeadInput struct {
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
	BaseRef      string
}

type InspectHeadResult struct {
	HeadSHA               string
	NewCommitSHAs         []string
	HasUncommittedChanges bool
	ChangedFiles          []string
}

type CommitInput struct {
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
	Message      string
}

type CommitResult struct {
	CommitSHA string
}

type CleanupWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
	WorktreePath      string
	Branch            string
	ProtectedBranches []string
}

type PushInput struct {
	RepoPath              string
	WorktreeRoot          string
	WorktreePath          string
	Branch                string
	Remote                string
	ExpectedRemoteHeadSHA string
	ProtectedBranches     []string
}

type WorktreeListEntry struct {
	Path    string
	Branch  string
	HeadSHA string
	Bare    bool
}

type ProtectedBranchError struct {
	Branch string
}

func (e *ProtectedBranchError) Error() string {
	return fmt.Sprintf("Refusing to modify protected branch: %s", e.Branch)
}

type RemoteHeadChangedError struct {
	Branch          string
	ExpectedHeadSHA string
	ActualHeadSHA   string
}

func (e *RemoteHeadChangedError) Error() string {
	expected := e.ExpectedHeadSHA
	if expected == "" {
		expected = "unknown"
	}
	actual := e.ActualHeadSHA
	if actual == "" {
		actual = "unknown"
	}

	return fmt.Sprintf("Remote head changed for %s: expected %s, got %s", e.Branch, expected, actual)
}

func New(options Options) *Gateway {
	gitPath := strings.TrimSpace(options.GitPath)
	if gitPath == "" {
		gitPath = "git"
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &Gateway{
		gitPath: gitPath,
		repos:   options.Repos,
		now:     now,
	}
}

func (g *Gateway) CreateBranch(ctx context.Context, repoPath, branch, startPoint string, protectedBranches []string) error {
	if strings.TrimSpace(startPoint) == "" {
		return fmt.Errorf("startPoint is required")
	}
	protected := append([]string{}, protectedBranches...)
	if startPoint != "" {
		protected = append(protected, startPoint)
	}
	if err := g.AssertWritableBranch(branch, protected); err != nil {
		return err
	}
	return g.runGit(ctx, repoPath, nil, "branch", "--force", branch, startPoint)
}

func (g *Gateway) CreateWorktree(ctx context.Context, input CreateWorktreeInput) (storage.WorktreeRecord, error) {
	if err := g.AssertWritableBranch(input.Branch, append(append([]string{}, input.ProtectedBranches...), input.BaseBranch)); err != nil {
		return storage.WorktreeRecord{}, err
	}

	if err := os.MkdirAll(input.WorktreeRoot, 0o755); err != nil {
		return storage.WorktreeRecord{}, fmt.Errorf("create worktree root: %w", err)
	}

	worktreePath := filepath.Join(input.WorktreeRoot, buildWorktreeDirectoryName(input))
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktreePath, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}); err != nil {
		return storage.WorktreeRecord{}, err
	}
	checkoutMode := normalizeCheckoutMode(input.CheckoutMode)

	restored, err := g.RestoreWorktree(ctx, RestoreWorktreeInput{
		ProjectID:            input.ProjectID,
		RepoPath:             input.RepoPath,
		Branch:               input.Branch,
		WorktreeRoot:         input.WorktreeRoot,
		CheckoutMode:         checkoutMode,
		ExpectedWorktreePath: worktreePath,
	})
	if err != nil {
		return storage.WorktreeRecord{}, err
	}
	if restored != nil {
		return *restored, nil
	}

	if checkoutMode == CheckoutModeDetached {
		startPoint, err := g.resolveDetachedStartPoint(ctx, input)
		if err != nil {
			return storage.WorktreeRecord{}, err
		}
		if err := g.runGit(ctx, input.RepoPath, nil, "worktree", "add", "--force", "--detach", worktreePath, startPoint); err != nil {
			return storage.WorktreeRecord{}, err
		}
	} else {
		branchExists, err := g.branchExists(ctx, input.RepoPath, input.Branch)
		if err != nil {
			return storage.WorktreeRecord{}, err
		}
		args := []string{"worktree", "add", "--force"}
		if branchExists {
			args = append(args, worktreePath, input.Branch)
		} else {
			args = append(args, "-b", input.Branch, worktreePath, input.BaseBranch)
		}
		if err := g.runGit(ctx, input.RepoPath, nil, args...); err != nil {
			return storage.WorktreeRecord{}, err
		}
	}

	headSHA, err := g.getHeadSHA(ctx, worktreePath)
	if err != nil {
		return storage.WorktreeRecord{}, err
	}

	nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
	var existingRecord *storage.WorktreeRecord
	if g.repos != nil {
		existingRecord, err = g.repos.Worktrees.GetByBranch(ctx, input.ProjectID, input.Branch)
		if err != nil {
			return storage.WorktreeRecord{}, fmt.Errorf("get existing worktree by branch: %w", err)
		}
	}

	baseBranch := input.BaseBranch
	record := storage.WorktreeRecord{
		ID:           valueOr(existingRecordID(existingRecord), mustRandomID()),
		ProjectID:    input.ProjectID,
		RepoPath:     input.RepoPath,
		WorktreePath: worktreePath,
		Branch:       input.Branch,
		BaseBranch:   stringPtr(baseBranch),
		Status:       "active",
		HeadSHA:      stringPtrIfNotEmpty(headSHA),
		MetadataJSON: stringPtr(`{"recovered":false}`),
		CreatedAt:    valueOr(existingRecordCreatedAt(existingRecord), nowISO),
		UpdatedAt:    nowISO,
		CleanedAt:    nil,
	}

	if g.repos != nil {
		if err := g.repos.Worktrees.Upsert(ctx, record); err != nil {
			return storage.WorktreeRecord{}, fmt.Errorf("upsert worktree record: %w", err)
		}
	}

	return record, nil

}

func (g *Gateway) ListWorktrees(ctx context.Context, repoPath string) ([]WorktreeListEntry, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	return parseWorktreeList(result.Stdout), nil
}

func (g *Gateway) DetectGitHubRepo(ctx context.Context, repoPath string) (string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "config", "--get", "remote.origin.url")
	if err != nil {
		return "", err
	}

	return parseGitHubRepoFromRemoteURL(strings.TrimSpace(result.Stdout)), nil
}

func (g *Gateway) RestoreWorktree(ctx context.Context, input RestoreWorktreeInput) (*storage.WorktreeRecord, error) {
	checkoutMode := normalizeCheckoutMode(input.CheckoutMode)

	if g.repos != nil {
		stored, err := g.repos.Worktrees.GetByBranch(ctx, input.ProjectID, input.Branch)
		if err != nil {
			return nil, fmt.Errorf("get stored worktree by branch: %w", err)
		}
		if stored != nil && stored.Status != "cleaned" && normalizeComparablePath(stored.RepoPath) == normalizeComparablePath(input.RepoPath) && worktreesafety.IsSafe(worktreesafety.CheckInput{WorktreePath: stored.WorktreePath, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}) {
			storedHealthy, err := g.isHealthyWorktree(ctx, stored.WorktreePath)
			if err != nil {
				return nil, err
			}
			if !storedHealthy {
				g.tryRemoveWorktree(ctx, input.RepoPath, stored.WorktreePath)
			} else {
				storedCheckoutMatches, err := g.matchesRestoreCheckoutMode(ctx, stored.WorktreePath, checkoutMode, input.Branch)
				if err != nil {
					return nil, err
				}
				if storedCheckoutMatches {
					headSHA, err := g.getHeadSHA(ctx, stored.WorktreePath)
					if err != nil {
						return nil, err
					}
					nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
					restored := *stored
					restored.HeadSHA = stringPtrIfNotEmpty(headSHA)
					restored.Status = "active"
					restored.UpdatedAt = nowISO
					if err := g.repos.Worktrees.Upsert(ctx, restored); err != nil {
						return nil, fmt.Errorf("upsert restored worktree record: %w", err)
					}
					return &restored, nil
				}

				g.tryRemoveWorktree(ctx, input.RepoPath, stored.WorktreePath)
			}
		}
	}

	worktrees, err := g.ListWorktrees(ctx, input.RepoPath)
	if err != nil {
		return nil, err
	}

	var match *WorktreeListEntry
	for i := range worktrees {
		candidate := worktrees[i]
		if checkoutMode == CheckoutModeDetached {
			expectedPath := candidate.Path
			if input.ExpectedWorktreePath != "" {
				expectedPath = input.ExpectedWorktreePath
			}
			if normalizeComparablePath(candidate.Path) != normalizeComparablePath(expectedPath) {
				continue
			}
		} else if candidate.Branch != input.Branch {
			continue
		}
		if !worktreesafety.IsSafe(worktreesafety.CheckInput{WorktreePath: candidate.Path, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}) {
			continue
		}
		match = &candidate
		break
	}

	if match == nil {
		return nil, nil
	}

	healthy, err := g.isHealthyWorktree(ctx, match.Path)
	if err != nil {
		return nil, err
	}
	if !healthy {
		g.tryRemoveWorktree(ctx, input.RepoPath, match.Path)
		return nil, nil
	}

	checkoutMatches, err := g.matchesRestoreCheckoutMode(ctx, match.Path, checkoutMode, input.Branch)
	if err != nil {
		return nil, err
	}
	if !checkoutMatches {
		g.tryRemoveWorktree(ctx, input.RepoPath, match.Path)
		return nil, nil
	}

	nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
	record := storage.WorktreeRecord{
		ID:           mustRandomID(),
		ProjectID:    input.ProjectID,
		RepoPath:     input.RepoPath,
		WorktreePath: match.Path,
		Branch:       input.Branch,
		BaseBranch:   stringPtr(input.Branch),
		Status:       "active",
		HeadSHA:      stringPtrIfNotEmpty(match.HeadSHA),
		MetadataJSON: stringPtr(`{"recovered":true}`),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
		CleanedAt:    nil,
	}
	if g.repos != nil {
		existing, err := g.repos.Worktrees.GetByBranch(ctx, input.ProjectID, input.Branch)
		if err != nil {
			return nil, fmt.Errorf("get existing restored worktree by branch: %w", err)
		}
		if existing != nil {
			record = *existing
		}
		record.WorktreePath = match.Path
		record.HeadSHA = stringPtrIfNotEmpty(match.HeadSHA)
		record.Status = "active"
		record.UpdatedAt = nowISO
		if record.RepoPath == "" {
			record.RepoPath = input.RepoPath
		}
		if record.ProjectID == "" {
			record.ProjectID = input.ProjectID
		}
		if record.Branch == "" {
			record.Branch = input.Branch
		}
		if err := g.repos.Worktrees.Upsert(ctx, record); err != nil {
			return nil, fmt.Errorf("upsert discovered worktree record: %w", err)
		}
	}

	return &record, nil
}

func (g *Gateway) CleanupWorktree(ctx context.Context, input CleanupWorktreeInput) error {
	if err := g.AssertWritableBranch(input.Branch, input.ProtectedBranches); err != nil {
		return err
	}
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: input.WorktreePath, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot}); err != nil {
		return err
	}
	if err := g.runGit(ctx, input.RepoPath, nil, "worktree", "remove", "--force", input.WorktreePath); err != nil {
		if !missingWorktreeErrorPattern.MatchString(err.Error()) {
			return err
		}
	}

	if g.repos == nil {
		return nil
	}

	existing, err := g.repos.Worktrees.GetByBranch(ctx, input.ProjectID, input.Branch)
	if err != nil {
		return fmt.Errorf("get worktree before cleanup update: %w", err)
	}
	if existing == nil {
		return nil
	}

	nowISO := g.now().UTC().Format(javaScriptISOStringLayout)
	updated := *existing
	updated.Status = "cleaned"
	updated.CleanedAt = stringPtr(nowISO)
	updated.UpdatedAt = nowISO
	if err := g.repos.Worktrees.Upsert(ctx, updated); err != nil {
		return fmt.Errorf("update cleaned worktree record: %w", err)
	}

	return nil
}

func (g *Gateway) Push(ctx context.Context, input PushInput) error {
	if err := g.AssertWritableBranch(input.Branch, input.ProtectedBranches); err != nil {
		return err
	}
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return err
	}
	remote := input.Remote
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}

	if input.ExpectedRemoteHeadSHA != "" {
		isExpectedAncestor, err := g.isAncestor(ctx, input.WorktreePath, input.ExpectedRemoteHeadSHA, "HEAD")
		if err != nil {
			return err
		}
		if !isExpectedAncestor {
			return fmt.Errorf("Refusing fixer push for %s because local HEAD no longer descends from %s", input.Branch, input.ExpectedRemoteHeadSHA)
		}

		err = g.runGit(ctx, input.WorktreePath, nil,
			"push",
			"--porcelain",
			fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", input.Branch, input.ExpectedRemoteHeadSHA),
			"-u",
			remote,
			fmt.Sprintf("HEAD:refs/heads/%s", input.Branch),
		)
		if err != nil {
			if pushConflictErrorPattern.MatchString(err.Error()) {
				actualHeadSHA, lookupErr := g.getRemoteHeadSHA(ctx, input.WorktreePath, remote, input.Branch)
				if lookupErr != nil {
					return lookupErr
				}
				return &RemoteHeadChangedError{Branch: input.Branch, ExpectedHeadSHA: input.ExpectedRemoteHeadSHA, ActualHeadSHA: actualHeadSHA}
			}
			return err
		}
		return nil
	}

	return g.runGit(ctx, input.WorktreePath, nil, "push", "-u", remote, fmt.Sprintf("HEAD:refs/heads/%s", input.Branch))
}

func (g *Gateway) PrepareWorktree(ctx context.Context, input PrepareWorktreeInput) (PrepareWorktreeResult, error) {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return PrepareWorktreeResult{}, err
	}
	remote := input.Remote
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}
	targetSpec := strings.TrimSpace(input.Ref)
	resetRef := "FETCH_HEAD"
	errorRef := targetSpec
	if targetSpec == "" {
		targetSpec = strings.TrimSpace(input.Branch)
		resetRef = remote + "/" + targetSpec
		errorRef = targetSpec
	}
	if targetSpec == "" {
		return PrepareWorktreeResult{}, fmt.Errorf("branch or ref is required")
	}
	if err := g.runGit(ctx, input.WorktreePath, nil, "fetch", remote, targetSpec); err != nil {
		return PrepareWorktreeResult{}, err
	}

	remoteHeadSHA, err := g.getRevision(ctx, input.WorktreePath, resetRef)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	if input.ExpectedHeadSHA != "" && remoteHeadSHA != input.ExpectedHeadSHA {
		return PrepareWorktreeResult{}, &RemoteHeadChangedError{Branch: errorRef, ExpectedHeadSHA: input.ExpectedHeadSHA, ActualHeadSHA: remoteHeadSHA}
	}

	statusBeforeReset, err := g.readStatus(ctx, input.WorktreePath)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	if len(statusBeforeReset) > 0 {
		return PrepareWorktreeResult{HeadSHA: remoteHeadSHA, Clean: false}, nil
	}

	localHeadSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	if remoteHeadSHA != "" && localHeadSHA != remoteHeadSHA {
		if err := g.runGit(ctx, input.WorktreePath, nil, "reset", "--hard", resetRef); err != nil {
			return PrepareWorktreeResult{}, err
		}
	}

	statusAfterReset, err := g.readStatus(ctx, input.WorktreePath)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}
	headSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return PrepareWorktreeResult{}, err
	}

	return PrepareWorktreeResult{HeadSHA: headSHA, Clean: len(statusAfterReset) == 0}, nil
}

func (g *Gateway) InspectHead(ctx context.Context, input InspectHeadInput) (InspectHeadResult, error) {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return InspectHeadResult{}, err
	}
	headSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return InspectHeadResult{}, err
	}

	newCommitSHAs := []string{}
	if input.BaseRef != "" {
		newCommitSHAs, err = g.listCommitsSince(ctx, input.WorktreePath, input.BaseRef)
		if err != nil {
			return InspectHeadResult{}, err
		}
	}

	status, err := g.readStatus(ctx, input.WorktreePath)
	if err != nil {
		return InspectHeadResult{}, err
	}

	changedFiles := make([]string, 0, len(status))
	for _, entry := range status {
		changedFiles = append(changedFiles, entry.Path)
	}

	return InspectHeadResult{
		HeadSHA:               headSHA,
		NewCommitSHAs:         newCommitSHAs,
		HasUncommittedChanges: len(status) > 0,
		ChangedFiles:          changedFiles,
	}, nil
}

func (g *Gateway) Commit(ctx context.Context, input CommitInput) (CommitResult, error) {
	if err := g.validateMutationWorktree(input.WorktreePath, input.RepoPath, input.WorktreeRoot); err != nil {
		return CommitResult{}, err
	}
	if err := g.runGit(ctx, input.WorktreePath, nil, "add", "-A"); err != nil {
		return CommitResult{}, err
	}
	if err := g.runGit(ctx, input.WorktreePath, nil, "commit", "-m", input.Message); err != nil {
		return CommitResult{}, err
	}

	headSHA, err := g.getHeadSHA(ctx, input.WorktreePath)
	if err != nil {
		return CommitResult{}, err
	}
	if headSHA == "" {
		return CommitResult{}, fmt.Errorf("commitSHA is required")
	}

	return CommitResult{CommitSHA: headSHA}, nil
}

func (g *Gateway) validateMutationWorktree(worktreePath, repoPath, worktreeRoot string) error {
	if repoPath == "" && worktreeRoot == "" && g.repos != nil {
		// Legacy callers may not provide safety context. Keep them working while
		// adapter callers pass repo/root context for the hard safety check.
		return nil
	}
	return worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktreePath, RepoPath: repoPath, WorktreeRoot: worktreeRoot})
}

func (g *Gateway) AssertWritableBranch(branch string, protectedBranches []string) error {
	for _, protectedBranch := range protectedBranches {
		if protectedBranch == branch {
			return &ProtectedBranchError{Branch: branch}
		}
	}
	return nil
}

func (g *Gateway) getHeadSHA(ctx context.Context, repoPath string) (string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(result.Stdout), nil
}

func (g *Gateway) branchExists(ctx context.Context, repoPath, branch string) (bool, error) {
	_, err := g.runGitResult(ctx, repoPath, nil, "show-ref", "--quiet", "--verify", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) remoteBranchExists(ctx context.Context, repoPath, remote, branch string) (bool, error) {
	_, err := g.runGitResult(ctx, repoPath, nil, "show-ref", "--quiet", "--verify", "refs/remotes/"+remote+"/"+branch)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) resolveDetachedStartPoint(ctx context.Context, input CreateWorktreeInput) (string, error) {
	remote := "origin"
	hasRemote, err := g.hasRemote(ctx, input.RepoPath, remote)
	if err != nil {
		return "", err
	}
	if hasRemote {
		remoteHeadSHA, err := g.getRemoteHeadSHA(ctx, input.RepoPath, remote, input.Branch)
		if err != nil {
			return "", err
		}
		if remoteHeadSHA != "" {
			if err := g.runGit(ctx, input.RepoPath, nil, "fetch", remote, input.Branch); err != nil {
				return "", err
			}

			remoteBranchExists, err := g.remoteBranchExists(ctx, input.RepoPath, remote, input.Branch)
			if err != nil {
				return "", err
			}
			if remoteBranchExists {
				return remote + "/" + input.Branch, nil
			}
		}
	}

	branchExists, err := g.branchExists(ctx, input.RepoPath, input.Branch)
	if err != nil {
		return "", err
	}
	if branchExists {
		return input.Branch, nil
	}

	return input.BaseBranch, nil
}

func (g *Gateway) hasRemote(ctx context.Context, repoPath, remote string) (bool, error) {
	_, err := g.runGitResult(ctx, repoPath, nil, "config", "--get", "remote."+remote+".url")
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) isAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	_, err := g.runGitResult(ctx, repoPath, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) isHealthyWorktree(ctx context.Context, worktreePath string) (bool, error) {
	if _, err := os.Stat(worktreePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}

	_, err := g.runGitResult(ctx, worktreePath, nil, "status", "--porcelain", "--untracked-files=all")
	if err == nil {
		return true, nil
	}
	return false, err
}

func (g *Gateway) isDetachedWorktree(ctx context.Context, worktreePath string) (bool, error) {
	result, err := g.runGitResult(ctx, worktreePath, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(result.Stdout) == "HEAD", nil
}

func (g *Gateway) getCurrentBranch(ctx context.Context, worktreePath string) (string, error) {
	result, err := g.runGitResult(ctx, worktreePath, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(result.Stdout)
	if branch == "HEAD" {
		return "", nil
	}
	return branch, nil
}

func (g *Gateway) matchesRestoreCheckoutMode(ctx context.Context, worktreePath string, checkoutMode CheckoutMode, branch string) (bool, error) {
	if checkoutMode == CheckoutModeDetached {
		return g.isDetachedWorktree(ctx, worktreePath)
	}

	currentBranch, err := g.getCurrentBranch(ctx, worktreePath)
	if err != nil {
		return false, err
	}
	return currentBranch == branch, nil
}

func (g *Gateway) tryRemoveWorktree(ctx context.Context, repoPath, worktreePath string) {
	if err := g.runGit(ctx, repoPath, nil, "worktree", "remove", "--force", worktreePath); err != nil {
		return
	}
}

func (g *Gateway) getRemoteHeadSHA(ctx context.Context, repoPath, remote, branch string) (string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "ls-remote", "--heads", remote, branch)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.Split(result.Stdout, "\n")[0])
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

func (g *Gateway) getRevision(ctx context.Context, repoPath, ref string) (string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (g *Gateway) listCommitsSince(ctx context.Context, repoPath, baseRef string) ([]string, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "rev-list", "--reverse", baseRef+"..HEAD")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(result.Stdout, "\n")
	commits := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			commits = append(commits, trimmed)
		}
	}
	return commits, nil
}

type statusEntry struct {
	Code string
	Path string
}

func (g *Gateway) readStatus(ctx context.Context, repoPath string) ([]statusEntry, error) {
	result, err := g.runGitResult(ctx, repoPath, nil, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}

	entries := []statusEntry{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 3 {
			continue
		}
		entries = append(entries, statusEntry{Code: line[:2], Path: strings.TrimSpace(line[3:])})
	}
	return entries, nil
}

func (g *Gateway) runGit(ctx context.Context, cwd string, env map[string]string, args ...string) error {
	_, err := g.runGitResult(ctx, cwd, env, args...)
	return err
}

func (g *Gateway) runGitResult(ctx context.Context, cwd string, env map[string]string, args ...string) (shell.Result, error) {
	result, err := shell.Run(ctx, shell.Options{Command: g.gitPath, Args: args, CWD: cwd, Env: env})
	if err == nil {
		return result, nil
	}

	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) {
		message := strings.TrimSpace(commandErr.Result.Stderr)
		if message == "" {
			message = strings.TrimSpace(commandErr.Result.Stdout)
		}
		if message == "" {
			message = commandErr.Error()
		}
		formatted := *commandErr
		formatted.Message = message
		return formatted.Result, fmt.Errorf("git %s: %w", strings.Join(args, " "), &formatted)
	}

	return result, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

func buildWorktreeDirectoryName(input CreateWorktreeInput) string {
	if input.PRNumber != 0 {
		return fmt.Sprintf("looper-fix-%s-pr-%d", sanitizeBranchName(input.ProjectID), input.PRNumber)
	}

	return sanitizeBranchName(input.Branch)
}

func parseGitHubRepoFromRemoteURL(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}

	patterns := []*regexp.Regexp{
		regexp.MustCompile(`^git@github\.com:(?P<repo>.+?)(?:\.git)?$`),
		regexp.MustCompile(`^ssh://git@github\.com/(?P<repo>.+?)(?:\.git)?$`),
		regexp.MustCompile(`^https://github\.com/(?P<repo>.+?)(?:\.git)?$`),
	}

	for _, pattern := range patterns {
		match := pattern.FindStringSubmatch(remoteURL)
		if match == nil {
			continue
		}
		index := pattern.SubexpIndex("repo")
		if index > 0 {
			return match[index]
		}
	}

	return ""
}

func sanitizeBranchName(branch string) string {
	var builder strings.Builder
	for _, r := range branch {
		if ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') || r == '.' || r == '_' || r == '-' {
			builder.WriteRune(r)
			continue
		}
		builder.WriteRune('-')
	}
	return builder.String()
}

func isWithinRoot(path, root string) bool {
	resolvedPath := normalizeComparablePath(path)
	resolvedRoot := normalizeComparablePath(root)
	return resolvedPath == resolvedRoot || strings.HasPrefix(resolvedPath, resolvedRoot+"/")
}

func normalizeComparablePath(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		resolved = path
	}
	resolved = filepath.ToSlash(filepath.Clean(resolved))
	if strings.HasPrefix(resolved, "/private/") {
		return strings.TrimPrefix(resolved, "/private")
	}
	return resolved
}

func parseWorktreeList(output string) []WorktreeListEntry {
	entries := []WorktreeListEntry{}
	current := WorktreeListEntry{}

	flush := func() {
		if current.Path == "" {
			return
		}
		entries = append(entries, current)
		current = WorktreeListEntry{}
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}

		switch {
		case strings.HasPrefix(line, "worktree "):
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case strings.HasPrefix(line, "HEAD "):
			current.HeadSHA = strings.TrimPrefix(line, "HEAD ")
		case line == "bare":
			current.Bare = true
		}
	}

	flush()
	return entries
}

func normalizeCheckoutMode(mode CheckoutMode) CheckoutMode {
	if mode == CheckoutModeDetached {
		return CheckoutModeDetached
	}
	return CheckoutModeBranch
}

func mustRandomID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

func stringPtr(value string) *string {
	return &value
}

func stringPtrIfNotEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func existingRecordID(record *storage.WorktreeRecord) string {
	if record == nil {
		return ""
	}
	return record.ID
}

func existingRecordCreatedAt(record *storage.WorktreeRecord) string {
	if record == nil {
		return ""
	}
	return record.CreatedAt
}

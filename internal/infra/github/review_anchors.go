package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/diffanchor"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/infra/shell"
)

// Authority for inline review anchors is the complete base/head PR diff for the
// exact SHAs under review — not a bounded shell prefix of `gh pr diff`, and not
// the agent's requested line numbers by themselves.
const (
	reviewPathDiffCommandTimeout = 180 * time.Second

	ReviewAnchorAuthorityLocalPathDiff = "local_path_diff"
)

// BuildReviewAnchorIndexInput selects the complete base/head authority used to
// validate inline review comment anchors.
type BuildReviewAnchorIndexInput struct {
	CWD         string
	BaseSHA     string
	BaseRefName string
	HeadSHA     string
	Paths       []string
}

// BuildReviewAnchorIndex validates only the comment paths using local PR commits.
// Remote diff APIs are intentionally not part of review publication.
func (g *Gateway) BuildReviewAnchorIndex(ctx context.Context, input BuildReviewAnchorIndexInput) (*diffanchor.Index, string, error) {
	paths := uniqueReviewAnchorPaths(input.Paths)
	if len(paths) == 0 {
		// Callers that pass path slots (inline comments) but every path trims empty
		// have no usable authority. Returning (nil, "", nil) would let
		// normalizeReviewAnchors keep malformed inline comments without validation.
		if len(input.Paths) > 0 {
			return nil, "", fmt.Errorf("%w: inline comments require non-empty paths", ErrAnchorValidationUnavailable)
		}
		return nil, "", nil
	}
	baseSHA := strings.TrimSpace(input.BaseSHA)
	headSHA := strings.TrimSpace(input.HeadSHA)
	if baseSHA == "" || headSHA == "" {
		return nil, "", fmt.Errorf("%w: missing base or head SHA", ErrAnchorValidationUnavailable)
	}

	index, err := g.buildLocalPathAnchorIndexWithBaseRef(ctx, input.CWD, baseSHA, strings.TrimSpace(input.BaseRefName), headSHA, paths)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s", ErrAnchorValidationUnavailable, sanitizeReviewAnchorLocalError(err))
	}
	return index, ReviewAnchorAuthorityLocalPathDiff, nil
}

// sanitizeReviewAnchorLocalError maps local path-authority failures to path-free
// reason codes suitable for returned errors and agent-facing diagnostics.
func sanitizeReviewAnchorLocalError(err error) string {
	if err == nil {
		return "local_path_diff_failed"
	}
	switch {
	case errors.Is(err, ErrLocalCaptureTruncated):
		return DiffTruncationReasonLocalCapture
	case errors.Is(err, ErrReviewBaseHeadMismatch):
		return "local_base_head_mismatch"
	default:
		return "local_path_diff_failed"
	}
}

func (g *Gateway) buildLocalPathAnchorIndex(ctx context.Context, cwd, baseSHA, headSHA string, paths []string) (*diffanchor.Index, error) {
	return g.buildLocalPathAnchorIndexWithBaseRef(ctx, cwd, baseSHA, "", headSHA, paths)
}

func (g *Gateway) buildLocalPathAnchorIndexWithBaseRef(ctx context.Context, cwd, baseSHA, baseRefName, headSHA string, paths []string) (*diffanchor.Index, error) {
	var parsed diffanchor.Index
	err := g.readLocalPullRequestDiff(ctx, cwd, baseSHA, baseRefName, headSHA, paths, func(reader io.Reader) error {
		var parseErr error
		parsed, parseErr = diffanchor.ParseReader(reader)
		return parseErr
	})
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// ReadLocalPullRequestDiff reads immutable PR commits using merge-base semantics.
// Git writes to a disposable file, bypassing shell output capture limits. The
// file is removed on every return; snapshots never persist the patch.
func (g *Gateway) ReadLocalPullRequestDiff(ctx context.Context, cwd, baseSHA, headSHA string, paths []string, read func(io.Reader) error) error {
	return g.readLocalPullRequestDiff(ctx, cwd, baseSHA, "", headSHA, paths, read)
}

func (g *Gateway) readLocalPullRequestDiff(ctx context.Context, cwd, baseSHA, baseRefName, headSHA string, paths []string, read func(io.Reader) error) error {
	if err := g.ensureLocalBaseCommit(ctx, cwd, baseSHA, baseRefName); err != nil {
		return err
	}
	if err := g.verifyLocalCommitObject(ctx, cwd, headSHA); err != nil {
		return err
	}
	// Include both rename paths so a new-path-only query preserves LEFT ranges.
	if len(paths) > 0 {
		expanded, err := g.expandReviewAnchorPathsForRenames(ctx, cwd, baseSHA, headSHA, paths)
		if err != nil {
			return err
		}
		paths = expanded
	}
	args := []string{"--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M", "--unified=3", baseSHA + "..." + headSHA, "--"}
	args = append(args, paths...)
	return g.readGitDiffOutput(ctx, cwd, args, read)
}

func (g *Gateway) ensureLocalBaseCommit(ctx context.Context, cwd, baseSHA, baseRefName string) error {
	return g.ensureLocalCommit(ctx, cwd, baseSHA, baseRefName)
}

func (g *Gateway) ensureLocalCommit(ctx context.Context, cwd, sha, fallbackRef string) error {
	err := g.verifyLocalCommitObject(ctx, cwd, sha)
	if err == nil {
		return nil
	}
	sha = strings.TrimSpace(sha)
	fallbackRef = strings.TrimSpace(fallbackRef)
	if !isSafeGitRev(sha) {
		return err
	}
	// Empty refmap and --no-write-fetch-head keep the caller's FETCH_HEAD,
	// remote tracking refs, HEAD, and working tree untouched.
	if _, fetchErr := g.runGitForReviewAnchors(ctx, cwd, "fetch", "--no-write-fetch-head", "--no-tags", "--refmap=", "origin", sha); fetchErr != nil && isSafeGitRev(fallbackRef) {
		_, _ = g.runGitForReviewAnchors(ctx, cwd, "fetch", "--no-write-fetch-head", "--no-tags", "--refmap=", "origin", fallbackRef)
	}
	return g.verifyLocalCommitObject(ctx, cwd, sha)
}

// HasLocalMergeConflicts checks whether merging baseSHA and headSHA produces a
// merge conflict using git merge-tree --write-tree without touching HEAD,
// index, working tree, FETCH_HEAD, or remote tracking refs. Used to distinguish
// real conflicts from Forgejo's asynchronous mergeable=false CHECKING state.
func (g *Gateway) HasLocalMergeConflicts(ctx context.Context, cwd, baseSHA, baseRefName, headSHA, headRefName string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	baseSHA = strings.TrimSpace(baseSHA)
	headSHA = strings.TrimSpace(headSHA)
	if !isSafeGitRev(baseSHA) || !isSafeGitRev(headSHA) {
		return false, fmt.Errorf("merge conflict check requires valid base and head commits")
	}
	if err := g.ensureLocalCommit(ctx, cwd, baseSHA, baseRefName); err != nil {
		return false, err
	}
	if err := g.ensureLocalCommit(ctx, cwd, headSHA, headRefName); err != nil {
		return false, err
	}
	result, err := g.runGitForReviewAnchors(ctx, cwd, "merge-tree", "--write-tree", baseSHA, headSHA)
	if err == nil {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var cmdErr *shell.CommandExecutionError
	if errors.As(err, &cmdErr) && result.ExitCode == 1 {
		return true, nil
	}
	return false, err
}

// BaseAdvancePreservesMergeBase reports whether advancing the PR base commit
// from oldBaseSHA to newBaseSHA leaves git merge-base(base, headSHA) unchanged.
// When merge-base(oldBaseSHA, headSHA) == merge-base(newBaseSHA, headSHA), the
// three-dot PR diff base...head is identical and previously computed findings
// and inline diff anchors remain exact. Fails closed on missing commits, fetch
// failures, shifted merge-bases (e.g. stacked PR merged or base rewritten), or
// invalid git state.
func BaseAdvancePreservesMergeBase(ctx context.Context, gitPath, cwd, oldBaseSHA, newBaseSHA, headSHA, baseRefName string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	oldBaseSHA = strings.TrimSpace(oldBaseSHA)
	newBaseSHA = strings.TrimSpace(newBaseSHA)
	headSHA = strings.TrimSpace(headSHA)
	baseRefName = strings.TrimSpace(baseRefName)
	if oldBaseSHA == "" || newBaseSHA == "" || headSHA == "" {
		return false, nil
	}
	if commitSHAsMatch(oldBaseSHA, newBaseSHA) {
		return true, nil
	}
	if strings.TrimSpace(cwd) == "" || !isSafeGitRev(oldBaseSHA) || !isSafeGitRev(newBaseSHA) || !isSafeGitRev(headSHA) {
		return false, nil
	}
	gateway := &Gateway{gitPath: strings.TrimSpace(gitPath), cwd: strings.TrimSpace(cwd)}
	if err := gateway.verifyLocalCommitObject(ctx, cwd, oldBaseSHA); err != nil {
		return false, err
	}
	if err := gateway.verifyLocalCommitObject(ctx, cwd, headSHA); err != nil {
		return false, err
	}
	if err := gateway.ensureLocalBaseCommit(ctx, cwd, newBaseSHA, baseRefName); err != nil {
		return false, err
	}
	oldMergeBases, err := gateway.sortedMergeBases(ctx, cwd, oldBaseSHA, headSHA)
	if err != nil || len(oldMergeBases) != 1 {
		return false, err
	}
	newMergeBases, err := gateway.sortedMergeBases(ctx, cwd, newBaseSHA, headSHA)
	if err != nil || len(newMergeBases) != 1 {
		return false, err
	}
	return strings.EqualFold(oldMergeBases[0], newMergeBases[0]), nil
}

func (g *Gateway) sortedMergeBases(ctx context.Context, cwd, baseSHA, headSHA string) ([]string, error) {
	result, err := g.runGitForReviewAnchors(ctx, cwd, "merge-base", "--all", baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(result.Stdout, "\n")
	bases := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			bases = append(bases, strings.ToLower(trimmed))
		}
	}
	sort.Strings(bases)
	return bases, nil
}

func isSafeGitRev(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (g *Gateway) readGitDiffOutput(ctx context.Context, cwd string, args []string, read func(io.Reader) error) error {
	file, err := os.CreateTemp("", "looper-local-diff-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	// Insert before -- so the output option cannot become a pathspec.
	outputArgs := append([]string(nil), args[:2]...)
	outputArgs = append(outputArgs, "--output="+file.Name())
	outputArgs = append(outputArgs, args[2:]...)
	if _, err := g.runGitForReviewAnchors(ctx, cwd, outputArgs...); err != nil {
		return err
	}
	return read(file)
}

// expandReviewAnchorPathsForRenames adds rename/copy partner paths for any comment
// path involved in a base...head rename or copy. Without both sides of the pair in
// the pathspec, `git diff base...head -- new/path` is a pure add and is not a
// complete authority for LEFT (or non-added RIGHT) anchors on renamed files.
func (g *Gateway) expandReviewAnchorPathsForRenames(ctx context.Context, cwd, baseSHA, headSHA string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return paths, nil
	}
	// Whole-tree name-status is one line per path (not file content). Rename
	// detection must not be path-limited: `git diff --name-status -M -- new/path`
	// reports A for renames when the old path is omitted from the pathspec.
	var expanded []string
	err := g.readGitDiffOutput(ctx, cwd, []string{"--literal-pathspecs", "diff", "--name-status", "-M", "--no-ext-diff", "--no-textconv", "--no-color", baseSHA + "..." + headSHA}, func(reader io.Reader) error {
		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		expanded = expandPathsWithRenamePartners(paths, string(data))
		return nil
	})
	return expanded, err
}

// expandPathsWithRenamePartners returns unique sorted paths including both sides of
// any rename (R*) or copy (C*) row from `git diff --name-status` that touches a
// requested path.
func expandPathsWithRenamePartners(paths []string, nameStatus string) []string {
	wanted := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		wanted[path] = struct{}{}
	}
	if len(wanted) == 0 {
		return uniqueReviewAnchorPaths(paths)
	}
	for _, line := range strings.Split(nameStatus, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		_, oldPath, newPath, ok := parseNameStatusRenameOrCopy(line)
		if !ok {
			continue
		}
		if _, hitOld := wanted[oldPath]; hitOld {
			wanted[newPath] = struct{}{}
		}
		if _, hitNew := wanted[newPath]; hitNew {
			wanted[oldPath] = struct{}{}
		}
	}
	out := make([]string, 0, len(wanted))
	for path := range wanted {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// parseNameStatusRenameOrCopy parses a git --name-status rename/copy row.
// Forms: "R100\told\tnew", "C075\told\tnew". Paths may be C-style quoted.
func parseNameStatusRenameOrCopy(line string) (status, oldPath, newPath string, ok bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 3 {
		return "", "", "", false
	}
	status = strings.TrimSpace(fields[0])
	if status == "" {
		return "", "", "", false
	}
	code := status[0]
	if code != 'R' && code != 'C' {
		return "", "", "", false
	}
	oldPath = unquoteGitNameStatusPath(fields[1])
	newPath = unquoteGitNameStatusPath(fields[2])
	if oldPath == "" || newPath == "" {
		return "", "", "", false
	}
	return status, oldPath, newPath, true
}

func unquoteGitNameStatusPath(path string) string {
	path = strings.TrimSpace(path)
	if len(path) < 2 || path[0] != '"' || path[len(path)-1] != '"' {
		return path
	}
	unquoted, err := strconv.Unquote(path)
	if err != nil {
		return path
	}
	return unquoted
}

func (g *Gateway) verifyLocalCommitObject(ctx context.Context, cwd, sha string) error {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return fmt.Errorf("%w: empty commit SHA", ErrReviewBaseHeadMismatch)
	}
	result, err := g.runGitForReviewAnchors(ctx, cwd, "rev-parse", "--verify", sha+"^{commit}")
	if err != nil {
		return fmt.Errorf("%w: commit %s is not available locally: %v", ErrReviewBaseHeadMismatch, sha, err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got == "" {
		return fmt.Errorf("%w: commit %s resolved empty locally", ErrReviewBaseHeadMismatch, sha)
	}
	if !commitSHAsMatch(sha, got) {
		return fmt.Errorf("%w: expected %s, local object is %s", ErrReviewBaseHeadMismatch, sha, got)
	}
	return nil
}

func commitSHAsMatch(expected, actual string) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	actual = strings.ToLower(strings.TrimSpace(actual))
	if expected == "" || actual == "" {
		return false
	}
	if expected == actual {
		return true
	}
	// Allow abbreviated expected SHAs when they uniquely prefix the resolved object.
	if len(expected) >= 7 && len(expected) < len(actual) && strings.HasPrefix(actual, expected) {
		return true
	}
	if len(actual) >= 7 && len(actual) < len(expected) && strings.HasPrefix(expected, actual) {
		return true
	}
	return false
}

func (g *Gateway) runGitForReviewAnchors(ctx context.Context, cwd string, args ...string) (shell.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gitRun := g.gitRun
	if gitRun == nil {
		gitRun = func(ctx context.Context, input shell.Options) (shell.Result, error) {
			return hostingidentity.RunGit(ctx, input, shell.Run)
		}
	}
	gitPath := strings.TrimSpace(g.gitPath)
	if gitPath == "" {
		gitPath = "git"
	}
	result, err := gitRun(ctx, shell.Options{
		Command: gitPath,
		Args:    args,
		CWD:     valueOr(strings.TrimSpace(cwd), g.cwd),
		Timeout: reviewPathDiffCommandTimeout,
	})
	if err == nil {
		return result, nil
	}
	// Summary omits pathspecs after "--" so secret-shaped comments[].path never
	// appears in returned errors if a caller logs them before sanitization.
	cmdSummary := reviewAnchorGitCommandSummary(args)
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
		return result, fmt.Errorf("git %s: %w", cmdSummary, &formatted)
	}
	return result, fmt.Errorf("git %s: %w", cmdSummary, err)
}

// reviewAnchorGitCommandSummary formats git argv for errors without pathspecs.
// Arguments after "--" are replaced with a count so path-shaped secrets cannot leak.
func reviewAnchorGitCommandSummary(args []string) string {
	if len(args) == 0 {
		return "(no args)"
	}
	for i, arg := range args {
		if arg != "--" {
			continue
		}
		pathCount := len(args) - i - 1
		prefix := strings.Join(args[:i+1], " ")
		if pathCount <= 0 {
			return prefix
		}
		return fmt.Sprintf("%s <%d paths>", prefix, pathCount)
	}
	return strings.Join(args, " ")
}

func uniqueReviewAnchorPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// HeadAdvanceIsAncestor reports whether oldHeadSHA is a strict or equal
// ancestor of newHeadSHA in the local Git repository (fetching newHeadSHA from
// origin if missing locally without mutating HEAD or the working tree).
// Fails closed on force-pushes, missing commits, or Git errors.
func HeadAdvanceIsAncestor(ctx context.Context, gitPath, cwd, oldHeadSHA, newHeadSHA, headRefName string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	oldHeadSHA = strings.TrimSpace(oldHeadSHA)
	newHeadSHA = strings.TrimSpace(newHeadSHA)
	headRefName = strings.TrimSpace(headRefName)
	if oldHeadSHA == "" || newHeadSHA == "" {
		return false, nil
	}
	if commitSHAsMatch(oldHeadSHA, newHeadSHA) {
		return true, nil
	}
	if strings.TrimSpace(cwd) == "" || !isSafeGitRev(oldHeadSHA) || !isSafeGitRev(newHeadSHA) {
		return false, nil
	}
	gateway := &Gateway{gitPath: strings.TrimSpace(gitPath), cwd: strings.TrimSpace(cwd)}
	if err := gateway.verifyLocalCommitObject(ctx, cwd, oldHeadSHA); err != nil {
		return false, err
	}
	if err := gateway.ensureLocalCommit(ctx, cwd, newHeadSHA, headRefName); err != nil {
		return false, err
	}
	return gateway.isCommitAncestor(ctx, cwd, oldHeadSHA, newHeadSHA)
}

// HeadAdvanceDisjointFromPaths reports whether oldHeadSHA is an ancestor of
// newHeadSHA AND no commit in oldHeadSHA..newHeadSHA touched any path in paths
// (including rename/copy partners). Fails closed on empty paths, force-pushes,
// missing commits, or any path overlap.
func HeadAdvanceDisjointFromPaths(ctx context.Context, gitPath, cwd, oldHeadSHA, newHeadSHA, headRefName string, paths []string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	oldHeadSHA = strings.TrimSpace(oldHeadSHA)
	newHeadSHA = strings.TrimSpace(newHeadSHA)
	headRefName = strings.TrimSpace(headRefName)
	if len(paths) == 0 || oldHeadSHA == "" || newHeadSHA == "" {
		return false, nil
	}
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			return false, nil
		}
		cleaned = append(cleaned, p)
	}
	if commitSHAsMatch(oldHeadSHA, newHeadSHA) {
		return true, nil
	}
	if strings.TrimSpace(cwd) == "" || !isSafeGitRev(oldHeadSHA) || !isSafeGitRev(newHeadSHA) {
		return false, nil
	}
	gateway := &Gateway{gitPath: strings.TrimSpace(gitPath), cwd: strings.TrimSpace(cwd)}
	if err := gateway.verifyLocalCommitObject(ctx, cwd, oldHeadSHA); err != nil {
		return false, err
	}
	if err := gateway.ensureLocalCommit(ctx, cwd, newHeadSHA, headRefName); err != nil {
		return false, err
	}
	ancestor, err := gateway.isCommitAncestor(ctx, cwd, oldHeadSHA, newHeadSHA)
	if err != nil || !ancestor {
		return false, err
	}
	deltaPaths, err := gateway.diffTouchedPaths(ctx, cwd, oldHeadSHA+".."+newHeadSHA)
	if err != nil {
		return false, err
	}
	for _, p := range cleaned {
		if _, touched := deltaPaths[p]; touched {
			return false, nil
		}
	}
	return true, nil
}

// HeadAdvanceDisjointFromPullRequestDiff reports whether oldHeadSHA is an
// ancestor of newHeadSHA AND the delta oldHeadSHA..newHeadSHA touches zero
// files from the reviewed PR diff baseSHA...oldHeadSHA. Intended to run
// against the main repository path so the shared object store can fetch
// newHeadSHA without touching the active reviewer worktree.
func HeadAdvanceDisjointFromPullRequestDiff(ctx context.Context, gitPath, cwd, baseSHA, oldHeadSHA, newHeadSHA, headRefName string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	baseSHA = strings.TrimSpace(baseSHA)
	oldHeadSHA = strings.TrimSpace(oldHeadSHA)
	newHeadSHA = strings.TrimSpace(newHeadSHA)
	headRefName = strings.TrimSpace(headRefName)
	if baseSHA == "" || oldHeadSHA == "" || newHeadSHA == "" {
		return false, nil
	}
	if commitSHAsMatch(oldHeadSHA, newHeadSHA) {
		return true, nil
	}
	if strings.TrimSpace(cwd) == "" || !isSafeGitRev(baseSHA) || !isSafeGitRev(oldHeadSHA) || !isSafeGitRev(newHeadSHA) {
		return false, nil
	}
	gateway := &Gateway{gitPath: strings.TrimSpace(gitPath), cwd: strings.TrimSpace(cwd)}
	if err := gateway.verifyLocalCommitObject(ctx, cwd, baseSHA); err != nil {
		return false, err
	}
	if err := gateway.verifyLocalCommitObject(ctx, cwd, oldHeadSHA); err != nil {
		return false, err
	}
	if err := gateway.ensureLocalCommit(ctx, cwd, newHeadSHA, headRefName); err != nil {
		return false, err
	}
	ancestor, err := gateway.isCommitAncestor(ctx, cwd, oldHeadSHA, newHeadSHA)
	if err != nil || !ancestor {
		return false, err
	}
	prPaths, err := gateway.diffTouchedPaths(ctx, cwd, baseSHA+"..."+oldHeadSHA)
	if err != nil || len(prPaths) == 0 {
		return false, err
	}
	deltaPaths, err := gateway.diffTouchedPaths(ctx, cwd, oldHeadSHA+".."+newHeadSHA)
	if err != nil {
		return false, err
	}
	for p := range prPaths {
		if _, touched := deltaPaths[p]; touched {
			return false, nil
		}
	}
	return true, nil
}

func (g *Gateway) isCommitAncestor(ctx context.Context, cwd, ancestorSHA, descendantSHA string) (bool, error) {
	_, err := g.runGitForReviewAnchors(ctx, cwd, "merge-base", "--is-ancestor", ancestorSHA, descendantSHA)
	if err == nil {
		return true, nil
	}
	var cmdErr *shell.CommandExecutionError
	if errors.As(err, &cmdErr) && cmdErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (g *Gateway) diffTouchedPaths(ctx context.Context, cwd, revRange string) (map[string]struct{}, error) {
	var raw string
	err := g.readGitDiffOutput(ctx, cwd, []string{"--literal-pathspecs", "diff", "--name-status", "-z", "-M", "--no-ext-diff", "--no-textconv", "--no-color", revRange}, func(reader io.Reader) error {
		data, readErr := io.ReadAll(reader)
		raw = string(data)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return parseNameStatusZPaths(raw), nil
}

func parseNameStatusZPaths(output string) map[string]struct{} {
	paths := map[string]struct{}{}
	fields := strings.Split(output, "\x00")
	for i := 0; i < len(fields); {
		status := fields[i]
		i++
		if status == "" {
			continue
		}
		switch status[0] {
		case 'R', 'C':
			if i+1 >= len(fields) {
				return paths
			}
			if fields[i] != "" {
				paths[fields[i]] = struct{}{}
			}
			if fields[i+1] != "" {
				paths[fields[i+1]] = struct{}{}
			}
			i += 2
		default:
			if i >= len(fields) {
				return paths
			}
			if fields[i] != "" {
				paths[fields[i]] = struct{}{}
			}
			i++
		}
	}
	return paths
}

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
	CWD     string
	BaseSHA string
	HeadSHA string
	Paths   []string
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

	index, err := g.buildLocalPathAnchorIndex(ctx, input.CWD, baseSHA, headSHA, paths)
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
	var parsed diffanchor.Index
	err := g.ReadLocalPullRequestDiff(ctx, cwd, baseSHA, headSHA, paths, func(reader io.Reader) error {
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
	if err := g.verifyLocalCommitObject(ctx, cwd, baseSHA); err != nil {
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
	gitRun := g.gitRun
	if gitRun == nil {
		gitRun = shell.Run
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

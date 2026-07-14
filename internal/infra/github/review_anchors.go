package github

import (
	"context"
	"errors"
	"fmt"
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
	reviewPathDiffMaxCapturedBytes = 32 * 1024 * 1024
	reviewPathDiffCommandTimeout   = 180 * time.Second

	ReviewAnchorAuthorityLocalPathDiff = "local_path_diff"
	ReviewAnchorAuthorityRemotePRDiff  = "remote_pr_diff"
)

// BuildReviewAnchorIndexInput selects the complete base/head authority used to
// validate inline review comment anchors.
type BuildReviewAnchorIndexInput struct {
	CWD     string
	BaseSHA string
	HeadSHA string
	Paths   []string
	// RemoteDiff is an optional fallback that must return a complete, untruncated
	// PR diff. Local capture truncation and true GitHub oversized responses must
	// surface as ErrLocalCaptureTruncated / ErrDiffTooLarge so they are not
	// parsed as complete authority.
	RemoteDiff func(context.Context) (string, error)
}

// BuildReviewAnchorIndex builds an authoritative diffanchor.Index for the
// comment paths. It prefers a path-targeted local base...head git diff after
// verifying local objects match the refreshed PR SHAs, and only falls back to a
// complete remote PR diff when that remote payload is fully available.
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

	localIndex, localErr := g.buildLocalPathAnchorIndex(ctx, input.CWD, baseSHA, headSHA, paths)
	if localErr == nil {
		return localIndex, ReviewAnchorAuthorityLocalPathDiff, nil
	}

	// Never embed raw localErr: runGitForReviewAnchors can include full pathspecs after
	// `git diff --`, and comments[].path may be secret-shaped (e.g. SERVICE_TOKEN=...).
	localReason := sanitizeReviewAnchorLocalError(localErr)
	if input.RemoteDiff == nil {
		return nil, "", fmt.Errorf("%w: %s", ErrAnchorValidationUnavailable, localReason)
	}
	remoteDiff, remoteErr := input.RemoteDiff(ctx)
	if remoteErr == nil {
		// Only complete remote payloads may be parsed into an authoritative index.
		parsed := diffanchor.Parse(remoteDiff)
		return &parsed, ReviewAnchorAuthorityRemotePRDiff, nil
	}
	if errors.Is(remoteErr, ErrLocalCaptureTruncated) || errors.Is(remoteErr, ErrDiffTooLarge) {
		return nil, "", fmt.Errorf("%w: remote PR diff unavailable (%s); local path authority failed: %s", ErrAnchorValidationUnavailable, remoteDiffAuthorityReason(remoteErr), localReason)
	}
	// Remote transport/message is also path-free: do not concatenate remoteErr text.
	return nil, "", fmt.Errorf("%w: remote PR diff failed; local path authority failed: %s", ErrAnchorValidationUnavailable, localReason)
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
	case errors.Is(err, ErrDiffTooLarge):
		return DiffTruncationReasonGitHubTooLarge
	case errors.Is(err, ErrReviewBaseHeadMismatch):
		return "local_base_head_mismatch"
	default:
		return "local_path_diff_failed"
	}
}

func remoteDiffAuthorityReason(err error) string {
	switch {
	case errors.Is(err, ErrLocalCaptureTruncated):
		return DiffTruncationReasonLocalCapture
	case errors.Is(err, ErrDiffTooLarge):
		return DiffTruncationReasonGitHubTooLarge
	default:
		return AnchorValidationUnavailableReason
	}
}

func (g *Gateway) buildLocalPathAnchorIndex(ctx context.Context, cwd, baseSHA, headSHA string, paths []string) (*diffanchor.Index, error) {
	if err := g.verifyLocalCommitObject(ctx, cwd, baseSHA); err != nil {
		return nil, err
	}
	if err := g.verifyLocalCommitObject(ctx, cwd, headSHA); err != nil {
		return nil, err
	}

	// Path-only diffs for the post-rename path emit a pure new-file patch unless the
	// pre-rename path is also selected. Expand rename/copy partners so the local
	// authority matches a complete PR rename diff (LEFT ranges + real RIGHT hunks).
	paths, err := g.expandReviewAnchorPathsForRenames(ctx, cwd, baseSHA, headSHA, paths)
	if err != nil {
		return nil, err
	}

	// Three-dot range matches GitHub PR diff semantics (merge-base(base, head)...head).
	// Use --literal-pathspecs so comments[].path values that look like Git pathspec
	// magic (e.g. ":(foo).txt") are treated as literal filenames, not magic.
	args := make([]string, 0, 7+len(paths))
	args = append(args, "--literal-pathspecs", "diff", "--no-ext-diff", "--no-color", baseSHA+"..."+headSHA, "--")
	args = append(args, paths...)
	result, err := g.runGitForReviewAnchors(ctx, cwd, args...)
	if result.StdoutTruncated {
		// Incomplete path-targeted output is never authoritative.
		return nil, ErrLocalCaptureTruncated
	}
	if err != nil {
		return nil, err
	}
	parsed := diffanchor.Parse(result.Stdout)
	return &parsed, nil
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
	result, err := g.runGitForReviewAnchors(ctx, cwd,
		"--literal-pathspecs", "diff", "--name-status", "-M", "--no-ext-diff", "--no-color", baseSHA+"..."+headSHA,
	)
	if result.StdoutTruncated {
		return nil, ErrLocalCaptureTruncated
	}
	if err != nil {
		return nil, err
	}
	expanded := expandPathsWithRenamePartners(paths, result.Stdout)
	return expanded, nil
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
		Command:          gitPath,
		Args:             args,
		CWD:              valueOr(strings.TrimSpace(cwd), g.cwd),
		Timeout:          reviewPathDiffCommandTimeout,
		MaxCapturedBytes: reviewPathDiffMaxCapturedBytes,
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

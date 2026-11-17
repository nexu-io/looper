package failureclass

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"syscall"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
)

type Boundary string

const (
	BoundaryGitRemote     Boundary = "git_remote"
	BoundaryGitLocal      Boundary = "git_local"
	BoundaryGitHubAPI     Boundary = "github_api"
	BoundaryModelProvider Boundary = "model_provider"
	BoundaryAgentProcess  Boundary = "agent_process"
	BoundaryLocalWorktree Boundary = "local_worktree"
	BoundaryStorage       Boundary = "storage"
	BoundaryConfig        Boundary = "config"
	BoundaryCheckpoint    Boundary = "checkpoint"
	BoundaryPolicy        Boundary = "policy"
	BoundaryUnknown       Boundary = "unknown"
)

type RunnerKind string

const (
	RunnerReviewer RunnerKind = "reviewer"
	RunnerWorker   RunnerKind = "worker"
	RunnerFixer    RunnerKind = "fixer"
	RunnerPlanner  RunnerKind = "planner"
)

type Kind string

const (
	RetryableTransient   Kind = "retryable_transient"
	RetryableAfterResume Kind = "retryable_after_resume"
	RecoverableInfra     Kind = "recoverable_infra"
	NonRetryable         Kind = "non_retryable"
	ManualIntervention   Kind = "manual_intervention"
	HITLInterrupt        Kind = "hitl_interrupt"
)

type Context struct {
	Runner          RunnerKind
	Step            string
	Boundary        Boundary
	SideEffectState string
}

type BoundaryError struct {
	Boundary Boundary
	Err      error
}

// WithBoundary attaches a failure boundary used by Classify when the caller's
// context boundary is unknown. If err is already wrapped with a non-empty
// boundary, that boundary is preserved so local construction/config failures
// are not reclassified as external (retryable) transport boundaries.
func WithBoundary(err error, boundary Boundary) error {
	if err == nil {
		return nil
	}
	var existing *BoundaryError
	if errors.As(err, &existing) && existing.Boundary != "" {
		return err
	}
	return &BoundaryError{Boundary: boundary, Err: err}
}

func (e *BoundaryError) Error() string { return e.Err.Error() }

func (e *BoundaryError) Unwrap() error { return e.Err }

func Classify(err error, ctx Context) Kind {
	if err == nil {
		return NonRetryable
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return RetryableTransient
	}
	if githubinfra.IsTransientError(err) {
		return RetryableTransient
	}
	// Prefer an explicit boundary attached to the error over the caller's step
	// default. Worktree steps often default to git_remote (fetch), but local
	// integrity failures wrap BoundaryLocalWorktree after a confirmed probe.
	var boundaryErr *BoundaryError
	if errors.As(err, &boundaryErr) && boundaryErr.Boundary != "" {
		ctx.Boundary = boundaryErr.Boundary
	}

	message := strings.ToLower(githubinfra.ErrorMessage(err))
	if message == "" {
		message = strings.ToLower(err.Error())
	}
	// Concurrent createRunContext races hit idx_runs_one_running_per_loop. The
	// durable index is correct; treat a leaked UNIQUE as retryable so it cannot
	// park the queue in manual_intervention (issue #634).
	if isOneRunningRunPerLoopViolation(message) {
		return RetryableTransient
	}
	if isRecoverableInfra(err, message, ctx.Boundary) {
		return RecoverableInfra
	}
	if isManualWorktreeMessage(message) || ctx.Boundary == BoundaryLocalWorktree {
		return ManualIntervention
	}
	if ctx.Boundary == BoundaryGitHubAPI && isRetryableGitHubGraphQLUnauthorized(message) {
		return RetryableTransient
	}
	if isDeterministicDenial(message) || isGitHubAPIDeterministicDenial(err, message, ctx.Boundary) || isInternalDeterministicBoundary(ctx.Boundary) {
		return NonRetryable
	}
	if isExternalBoundary(ctx.Boundary) {
		return RetryableTransient
	}
	return NonRetryable
}

func isRecoverableInfra(err error, message string, boundary Boundary) bool {
	// Hosting adapters already distinguish retryable launch pressure from a
	// missing configured CLI. Preserve that explicit policy; it must not become
	// a disposable-worktree repair just because the message contains ENOENT.
	var classified *BoundaryError
	if boundary == BoundaryConfig || (errors.As(err, &classified) && shell.IsStartFailure(err)) {
		return false
	}
	// Resource exhaustion is an environmental condition regardless of which
	// operation happened to observe it. Retrying it under its own class lets the
	// runtime wait for the condition reconciler instead of spending agent turns.
	for _, target := range []error{syscall.ENOSPC, syscall.EDQUOT, syscall.EMFILE, syscall.ENFILE, syscall.EAGAIN, syscall.ENOMEM} {
		if errors.Is(err, target) {
			return true
		}
	}
	for _, fragment := range []string{
		"no space left on device",
		"disk quota exceeded",
		"too many open files",
		"resource temporarily unavailable",
		"cannot allocate memory",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}

	// ENOENT is recoverable only at boundaries backed by disposable local
	// infrastructure. At API/config boundaries it can describe a genuinely
	// missing target or executable and must retain the boundary's normal policy.
	if recoverableMissingPathBoundary(boundary) && (errors.Is(err, fs.ErrNotExist) || strings.Contains(message, "no such file or directory")) {
		return true
	}
	if strings.Contains(message, "fork/exec") && strings.Contains(message, "no such file or directory") {
		return true
	}
	if boundary == BoundaryLocalWorktree || boundary == BoundaryGitLocal || boundary == BoundaryGitRemote {
		for _, fragment := range []string{
			"missing worktree",
			"worktree path does not exist",
			"stale worktree",
			"worktree is stale",
			"not a git repository",
		} {
			if strings.Contains(message, fragment) {
				return true
			}
		}
	}
	return false
}

func recoverableMissingPathBoundary(boundary Boundary) bool {
	switch boundary {
	case BoundaryLocalWorktree, BoundaryGitLocal, BoundaryGitRemote, BoundaryAgentProcess:
		return true
	default:
		return false
	}
}

func isExternalBoundary(boundary Boundary) bool {
	switch boundary {
	case BoundaryGitRemote, BoundaryGitHubAPI, BoundaryModelProvider, BoundaryAgentProcess:
		return true
	default:
		return false
	}
}

func isInternalDeterministicBoundary(boundary Boundary) bool {
	switch boundary {
	case BoundaryGitLocal, BoundaryStorage, BoundaryConfig, BoundaryCheckpoint, BoundaryPolicy:
		return true
	default:
		return false
	}
}

// IsOneRunningRunPerLoopViolation reports whether err is the partial unique
// index on runs(loop_id) WHERE status='running'. Exported so runners can map
// the conflict to a typed busy error without string-matching in three places.
func IsOneRunningRunPerLoopViolation(err error) bool {
	if err == nil {
		return false
	}
	return isOneRunningRunPerLoopViolation(strings.ToLower(err.Error()))
}

func isOneRunningRunPerLoopViolation(message string) bool {
	if !strings.Contains(message, "unique constraint failed") {
		return false
	}
	return strings.Contains(message, "runs.loop_id") ||
		strings.Contains(message, "idx_runs_one_running_per_loop")
}

func isManualWorktreeMessage(message string) bool {
	return strings.Contains(message, "dirty worktree") ||
		strings.Contains(message, "worktree is dirty") ||
		strings.Contains(message, "uncommitted changes") ||
		strings.Contains(message, "manual intervention required")
}

func isDeterministicDenial(message string) bool {
	for _, fragment := range []string{
		"could not resolve to a pullrequest",
		"could not resolve to an issue",
		"protected branch",
		"branch protection",
		"policy denied",
		"checkpoint invariant",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func isGitHubAPIDeterministicDenial(err error, message string, boundary Boundary) bool {
	if boundary != BoundaryGitHubAPI {
		return false
	}
	// Typed provider status codes (e.g. ForgejoHTTPError) are authoritative for
	// permanent client failures. String "HTTP 404" alone stays retryable for
	// GitHub CLI errors because generic REST 404s are ambiguous; missing PR
	// targets on GitHub use "Could not resolve" instead.
	switch httpStatusCode(err) {
	case 400, 404, 422:
		return true
	}
	for _, fragment := range []string{
		"http 400",
		"http 422",
		"400 bad request",
		"422 unprocessable",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// httpStatusCoder is implemented by provider HTTP errors that expose the
// original response status without requiring string parsing (e.g. Forgejo
// ForgejoHTTPError). Classification uses errors.As so wrapped values still
// match.
//
// Trade-off (typed status classification):
//   - Prevents: Forgejo/provider permanent client failures (400/404/422) from
//     being treated as retryable_transient when the fixer tags them
//     BoundaryGitHubAPI, which would requeue deleted/missing PRs forever on
//     unlimited queues.
//   - Cost: a second classification path beside message fragments; providers
//     must implement HTTPStatusCode() or they fall through to string rules.
//     Only 400/404/422 are terminal via this path — 401/403/5xx stay on
//     existing message/boundary rules (auth denials vs transient outages).
//   - Why not string "HTTP 404" alone: generic GitHub CLI REST 404s are
//     ambiguous (missing issue vs missing endpoint vs temporary routing) and
//     must stay retryable; missing GitHub PR targets use "Could not resolve"
//     instead. Typed status is authoritative only when the provider preserves
//     the numeric code on the error value.
//   - Why not import provider packages here: failureclass stays free of
//     forge/github concrete types; the small interface is the extension point.
type httpStatusCoder interface {
	HTTPStatusCode() int
}

func httpStatusCode(err error) int {
	var coder httpStatusCoder
	if errors.As(err, &coder) {
		return coder.HTTPStatusCode()
	}
	return 0
}

func isRetryableGitHubGraphQLUnauthorized(message string) bool {
	if !(strings.Contains(message, "graphql") && strings.Contains(message, "401")) {
		return false
	}
	for _, fragment := range []string{
		"bad credentials",
		"authentication failed",
		"permission denied",
		"not authorized",
		"invalid token",
		"token expired",
	} {
		if strings.Contains(message, fragment) {
			return false
		}
	}
	return true
}

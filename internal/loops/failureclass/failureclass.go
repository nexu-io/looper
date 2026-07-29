package failureclass

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"syscall"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
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

func WithBoundary(err error, boundary Boundary) error {
	if err == nil {
		return nil
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
	if ctx.Boundary == BoundaryUnknown {
		var boundaryErr *BoundaryError
		if errors.As(err, &boundaryErr) && boundaryErr.Boundary != "" {
			ctx.Boundary = boundaryErr.Boundary
		}
	}

	message := strings.ToLower(githubinfra.ErrorMessage(err))
	if message == "" {
		message = strings.ToLower(err.Error())
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
	if isDeterministicDenial(message) || isGitHubAPIDeterministicDenial(message, ctx.Boundary) || isInternalDeterministicBoundary(ctx.Boundary) {
		return NonRetryable
	}
	if isExternalBoundary(ctx.Boundary) {
		return RetryableTransient
	}
	return NonRetryable
}

func isRecoverableInfra(err error, message string, boundary Boundary) bool {
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

func isGitHubAPIDeterministicDenial(message string, boundary Boundary) bool {
	if boundary != BoundaryGitHubAPI {
		return false
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

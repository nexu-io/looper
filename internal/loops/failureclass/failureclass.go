package failureclass

import (
	"context"
	"errors"
	"strings"

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
	NonRetryable         Kind = "non_retryable"
	ManualIntervention   Kind = "manual_intervention"
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
	if isManualWorktreeMessage(message) || ctx.Boundary == BoundaryLocalWorktree {
		return ManualIntervention
	}
	if ctx.Boundary == BoundaryGitHubAPI && isRetryableGitHubGraphQLUnauthorized(message) {
		return RetryableTransient
	}
	if isDeterministicDenial(message) || isInternalDeterministicBoundary(ctx.Boundary) {
		return NonRetryable
	}
	if isExternalBoundary(ctx.Boundary) {
		return RetryableTransient
	}
	return NonRetryable
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
		"http 400",
		"http 401",
		"http 403",
		"http 404",
		"http 422",
		"400 bad request",
		"401 unauthorized",
		"403 forbidden",
		"404 not found",
		"422 unprocessable",
		"bad credentials",
		"authentication failed",
		"permission denied",
		"not authorized",
		"repository not found",
		"could not resolve to a repository",
		"could not resolve to a pullrequest",
		"protected branch",
		"branch protection",
		"policy denied",
		"invalid model",
		"unsupported model",
		"config validation",
		"schema",
		"checkpoint invariant",
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

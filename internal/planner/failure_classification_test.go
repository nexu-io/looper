package planner

import (
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/loops/failureclass"
)

func TestClassifyFailureDoesNotRetryUnknownExternalLookingMessage(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(errors.New("model provider request failed: broken pipe"))
	if got.kind != FailureNonRetryable {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureNonRetryable)
	}
}

func TestClassifyFailureRetriesBoundaryExternalTransport(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(failureclass.WithBoundary(errors.New("model provider request failed: HTTP 503 Service Unavailable"), failureclass.BoundaryModelProvider))
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

func TestClassifyFailureMapsRecoverableInfra(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailureWithBoundary(errors.New("git worktree: no space left on device"), failureclass.BoundaryGitRemote)
	if got.kind != FailureRecoverableInfra {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRecoverableInfra)
	}
	if !shouldRetryQueueFailure(got.kind, 1, -1) {
		t.Fatal("recoverable infrastructure failure must remain queued while its condition can self-clear")
	}
}

package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/loops/failureclass"
)

func TestClassifyFailureDoesNotRetryUnknownExternalLookingMessage(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(errors.New("git fetch origin failed: broken pipe"))
	if got.kind != FailureNonRetryable {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureNonRetryable)
	}
}

func TestClassifyFailureRetriesBoundaryExternalTransport(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(failureclass.WithBoundary(errors.New("git fetch origin failed: broken pipe"), failureclass.BoundaryGitRemote))
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

func TestClassifyFailurePreservesContextTransient(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(context.DeadlineExceeded)
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

func TestClassifyFailureMapsRecoverableInfra(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailureWithBoundary(errors.New("fork/exec git: resource temporarily unavailable"), failureclass.BoundaryAgentProcess)
	if got.kind != FailureRecoverableInfra {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRecoverableInfra)
	}
	if !shouldRetryQueueFailure(got.kind, 1, -1) {
		t.Fatal("recoverable infrastructure failure must remain queued while its condition can self-clear")
	}
}

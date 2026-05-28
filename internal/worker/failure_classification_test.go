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

package fixer

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/loops/failureclass"
)

func TestClassifyFailureRetriesContextCancellation(t *testing.T) {
	runner := &Runner{}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		got := runner.classifyFailure(err)
		if got.kind != FailureRetryableTransient {
			t.Fatalf("classifyFailure(%v) kind = %s, want %s", err, got.kind, FailureRetryableTransient)
		}
	}
}

func TestClassifyFailureDoesNotRetryUnknownExternalLookingMessage(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(errors.New("git push failed: connection reset by peer"))
	if got.kind != FailureNonRetryable {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureNonRetryable)
	}
}

func TestClassifyFailureRetriesBoundaryExternalTransport(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(failureclass.WithBoundary(errors.New("git push failed: connection reset by peer"), failureclass.BoundaryGitRemote))
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

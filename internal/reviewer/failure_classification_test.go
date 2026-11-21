package reviewer

import (
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/loops/failureclass"
)

func TestClassifyFailureMapsRecoverableInfra(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailureForProjectAndBoundary("", errors.New("chdir /tmp/reviewer-worktree: no such file or directory"), failureclass.BoundaryGitRemote)
	if got.kind != FailureRecoverableInfra {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRecoverableInfra)
	}
	if !shouldRetryQueueFailure(got.kind, 1, -1) {
		t.Fatal("recoverable infrastructure failure must remain queued while its condition can self-clear")
	}
}

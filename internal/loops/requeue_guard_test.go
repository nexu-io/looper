package loops

import (
	"testing"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

func TestPullRequestTargetGuardKeyMatchesLoopRecordKey(t *testing.T) {
	t.Parallel()
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	loop := storage.LoopRecord{
		ProjectID:  "proj",
		Type:       string(domain.LoopTypeFixer),
		TargetType: string(domain.LoopTargetTypePullRequest),
		TargetID:   &targetID,
		Repo:       &repo,
		PRNumber:   &prNumber,
	}
	fromRecord := LoopTargetGuardKeyFromRecord(loop)
	fromPR := PullRequestTargetGuardKey("proj", repo, prNumber)
	if fromRecord == "" || fromRecord != fromPR {
		t.Fatalf("PR keys = record %q / explicit %q, want equal non-empty", fromRecord, fromPR)
	}
	// Cross-type identity: reviewer discovery and fixer discard share one mutex.
	reviewerKey := LoopTargetGuardKey("proj", string(domain.LoopTypeReviewer), string(domain.LoopTargetTypePullRequest), TargetKeyFromLoopRecord(loop))
	if reviewerKey != fromPR {
		t.Fatalf("reviewer key %q != fixer PR key %q", reviewerKey, fromPR)
	}
}

func TestLoopTargetGuardKeyOmitsTypeForPullRequest(t *testing.T) {
	t.Parallel()
	fixer := LoopTargetGuardKey("proj", "fixer", "pull_request", "pull_request:acme/looper:42")
	worker := LoopTargetGuardKey("proj", "worker", "pull_request", "pull_request:acme/looper:42")
	if fixer == "" || fixer != worker {
		t.Fatalf("PR keys = %q / %q, want equal non-empty shared key", fixer, worker)
	}
	issueFixer := LoopTargetGuardKey("proj", "fixer", "issue", "issue:acme/looper:7")
	issueWorker := LoopTargetGuardKey("proj", "worker", "issue", "issue:acme/looper:7")
	if issueFixer == "" || issueFixer == issueWorker {
		t.Fatalf("issue keys = %q / %q, want distinct type-scoped keys", issueFixer, issueWorker)
	}
	if got := LoopTargetGuardKey("proj", "worker", "project", "project:proj"); got != "" {
		t.Fatalf("project worker key = %q, want empty (concurrent workers)", got)
	}
}

package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nexu-io/looper/internal/config"
)

const sandboxGitHubFetchContract = "The daemon reads GitHub for this sandboxed review. Read the supplied PR context file and inspect the local Git diff at the seeded base and head. Publish findings only through the trusted review-submit command."

func usesSandboxGitHubContext(cfg config.Config, projectID string, identity config.HostingIdentityKind) bool {
	return cfg.Roles.Reviewer.Nono != nil && identity == "" && reviewerProjectProviderKind(cfg, projectID) == config.ProviderKindGitHub
}

// writeSandboxGitHubContext keeps PR-sized data outside the agent command line.
// Network requests are scoped by the daemon's existing project and PR binding.
func (r *Runner) writeSandboxGitHubContext(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint) (string, error) {
	query := ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath}
	detail, err := r.github.ViewPullRequest(ctx, query)
	if err != nil {
		return "", fmt.Errorf("read sandbox PR metadata: %w", err)
	}
	if detail.HeadSHA != checkpoint.Snapshot.HeadSHA {
		return "", &loopError{message: "PR head changed while loading sandbox review context", kind: FailureRetryableAfterResume, interrupted: true}
	}
	if reason := reviewerPublishDriftReason(input, checkpoint, detail); reason != "" {
		return "", &loopError{message: reason, kind: FailureRetryableAfterResume, interrupted: true}
	}
	comments, err := r.github.ListIssueComments(ctx, query)
	if err != nil {
		return "", fmt.Errorf("read sandbox PR comments: %w", err)
	}
	threads, err := r.github.ListReviewThreads(ctx, ListReviewThreadsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath, AllPages: true})
	if err != nil {
		return "", fmt.Errorf("read sandbox PR threads: %w", err)
	}
	data, err := json.Marshal(struct {
		PR           PullRequestDetail `json:"pr"`
		Conversation []IssueComment    `json:"conversation"`
		Threads      []ReviewThread    `json:"threads"`
	}{detail, comments, threads})
	if err != nil {
		return "", err
	}
	path := filepath.Join(input.sandboxDir, "pull-request.json")
	return path, os.WriteFile(path, data, 0600)
}

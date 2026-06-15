package failureclass

import (
	"errors"
	"testing"
)

func TestClassifyExternalBoundaryTransportFailures(t *testing.T) {
	tests := []struct {
		name     string
		boundary Boundary
		message  string
	}{
		{name: "git remote", boundary: BoundaryGitRemote, message: "git fetch origin: ssh_exchange_identification: Connection closed by remote host"},
		{name: "github api", boundary: BoundaryGitHubAPI, message: "GraphQL request failed: HTTP 504 Gateway Timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(errors.New(tt.message), Context{Runner: RunnerReviewer, Boundary: tt.boundary})
			if got != RetryableTransient {
				t.Fatalf("Classify() = %s, want %s", got, RetryableTransient)
			}
		})
	}
}

func TestClassifyUnknownBoundaryDoesNotPromoteByMessage(t *testing.T) {
	got := Classify(errors.New("git ls-remote failed: broken pipe"), Context{Runner: RunnerReviewer, Boundary: BoundaryUnknown})
	if got != NonRetryable {
		t.Fatalf("Classify() = %s, want %s", got, NonRetryable)
	}
}

func TestClassifyUsesWrappedBoundaryAuthority(t *testing.T) {
	err := WithBoundary(errors.New("git ls-remote failed: broken pipe"), BoundaryGitRemote)
	got := Classify(err, Context{Runner: RunnerReviewer, Boundary: BoundaryUnknown})
	if got != RetryableTransient {
		t.Fatalf("Classify() = %s, want %s", got, RetryableTransient)
	}
}

func TestClassifyRetriesGitHubGraphQLUnauthorizedAtGitHubBoundary(t *testing.T) {
	got := Classify(errors.New(`Post "https://api.github.com/graphql": HTTP 401 Unauthorized`), Context{Runner: RunnerReviewer, Boundary: BoundaryGitHubAPI})
	if got != RetryableTransient {
		t.Fatalf("Classify() = %s, want %s", got, RetryableTransient)
	}
}

func TestClassifyRetriesCredentialFailuresAtGitHubBoundary(t *testing.T) {
	got := Classify(errors.New("GitHub API failed: HTTP 401 Unauthorized: bad credentials"), Context{Runner: RunnerReviewer, Boundary: BoundaryGitHubAPI})
	if got != RetryableTransient {
		t.Fatalf("Classify() = %s, want %s", got, RetryableTransient)
	}
}

func TestClassifyRepairableExternalFailuresRetry(t *testing.T) {
	tests := []struct {
		name     string
		boundary Boundary
		message  string
		want     Kind
	}{
		{name: "github auth", boundary: BoundaryGitHubAPI, message: "GitHub API failed: HTTP 403 Forbidden", want: RetryableTransient},
		{name: "invalid git repository", boundary: BoundaryGitRemote, message: "git worktree list --porcelain: fatal: not a git repository (or any of the parent directories): .git", want: RetryableTransient},
		{name: "missing repo directory", boundary: BoundaryGitRemote, message: "start command: chdir /tmp/missing-repo: no such file or directory", want: RetryableTransient},
		{name: "invalid model", boundary: BoundaryModelProvider, message: "invalid model", want: RetryableTransient},
		{name: "repo not found", boundary: BoundaryGitHubAPI, message: "GraphQL: Could not resolve to a Repository", want: RetryableTransient},
		{name: "config", boundary: BoundaryConfig, message: "config validation failed", want: NonRetryable},
		{name: "checkpoint", boundary: BoundaryCheckpoint, message: "checkpoint invariant missing", want: NonRetryable},
		{name: "dirty worktree", boundary: BoundaryLocalWorktree, message: "dirty worktree", want: ManualIntervention},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(errors.New(tt.message), Context{Runner: RunnerWorker, Boundary: tt.boundary})
			if got != tt.want {
				t.Fatalf("Classify() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClassifyTerminalTargetMissingFailures(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{name: "pull request", message: "GraphQL: Could not resolve to a PullRequest with the number of 71. (repository.pullRequest)"},
		{name: "issue", message: "GraphQL: Could not resolve to an Issue with the number of 42. (repository.issue)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(errors.New(tt.message), Context{Runner: RunnerReviewer, Boundary: BoundaryGitHubAPI})
			if got != NonRetryable {
				t.Fatalf("Classify() = %s, want %s", got, NonRetryable)
			}
		})
	}
}

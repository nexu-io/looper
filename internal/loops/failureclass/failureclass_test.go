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

func TestClassifyDoesNotRetryCredentialFailures(t *testing.T) {
	got := Classify(errors.New("GitHub API failed: HTTP 401 Unauthorized: bad credentials"), Context{Runner: RunnerReviewer, Boundary: BoundaryGitHubAPI})
	if got != NonRetryable {
		t.Fatalf("Classify() = %s, want %s", got, NonRetryable)
	}
}

func TestClassifyDeterministicFailuresDoNotBecomeTransient(t *testing.T) {
	tests := []struct {
		name     string
		boundary Boundary
		message  string
		want     Kind
	}{
		{name: "github auth", boundary: BoundaryGitHubAPI, message: "GitHub API failed: HTTP 403 Forbidden", want: NonRetryable},
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

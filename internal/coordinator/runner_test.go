package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestDiscoverIssuesRespectsPollInterval(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.PollInterval = "1m"
	now := time.Date(2026, time.May, 14, 10, 0, 0, 0, time.UTC)
	runner := New(Options{Config: &cfg, Now: func() time.Time { return now }})

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if !first.Ticked || first.Skipped {
		t.Fatalf("first DiscoverIssues() = %#v, want first coordinator tick to run", first)
	}

	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if !second.Skipped || second.Ticked {
		t.Fatalf("second DiscoverIssues() = %#v, want poll interval gate to skip", second)
	}

	now = now.Add(time.Minute)
	third, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if !third.Ticked || third.Skipped {
		t.Fatalf("third DiscoverIssues() = %#v, want coordinator tick after interval", third)
	}
}

func TestDiscoverIssuesRespectsTrimmedPollInterval(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.PollInterval = " 1m "
	now := time.Date(2026, time.May, 14, 10, 0, 0, 0, time.UTC)
	runner := New(Options{Config: &cfg, Now: func() time.Time { return now }})

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if !first.Ticked || first.Skipped {
		t.Fatalf("first DiscoverIssues() = %#v, want first coordinator tick to run", first)
	}

	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if !second.Skipped || second.Ticked {
		t.Fatalf("second DiscoverIssues() = %#v, want poll interval gate to skip", second)
	}
}

func TestShouldSkipIssueForSweeperManagedLabels(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	coordinatorCfg := cfg.Roles.Coordinator
	sweeperCfg := cfg.Roles.Sweeper

	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{name: "pending", labels: []string{sweeperCfg.Lifecycle.PendingLabel}},
		{name: "closed", labels: []string{sweeperCfg.Lifecycle.ClosedLabel}},
		{name: "quarantine", labels: []string{sweeperCfg.Security.QuarantineLabel}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !ShouldSkipIssue(IssueSummary{Labels: tc.labels}, coordinatorCfg, sweeperCfg) {
				t.Fatalf("ShouldSkipIssue(%v) = false, want true", tc.labels)
			}
		})
	}

	if ShouldSkipIssue(IssueSummary{Labels: []string{"dispatch/plan"}}, coordinatorCfg, sweeperCfg) {
		t.Fatal("ShouldSkipIssue(dispatch/plan) = true, want false for coordinator-owned labels")
	}
}

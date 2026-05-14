package coordinator

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

type DiscoveryInput struct {
	ProjectID string
	Repo      string
}

type DiscoveryResult struct {
	Skipped bool
	Ticked  bool
}

type IssueSummary struct {
	Labels []string
}

type Options struct {
	Config *config.Config
	Now    func() time.Time
}

type Runner struct {
	config *config.Config
	now    func() time.Time

	mu                sync.Mutex
	lastTickByProject map[string]time.Time
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{config: options.Config, now: now, lastTickByProject: map[string]time.Time{}}
}

func (r *Runner) DiscoverIssues(_ context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if !r.shouldRunTick(input.ProjectID) {
		return DiscoveryResult{Skipped: true}, nil
	}
	return DiscoveryResult{Ticked: true}, nil
}

// ShouldSkipIssue reserves the structural cross-role boundary with Sweeper.
// Future triage discovery must skip issues that Sweeper already marked pending,
// retired, or quarantined so the two roles never fight over authority.
func ShouldSkipIssue(issue IssueSummary, roleCfg config.CoordinatorRoleConfig, sweeperCfg config.SweeperRoleConfig) bool {
	_ = roleCfg
	return hasLabel(issue.Labels, sweeperCfg.Lifecycle.PendingLabel) ||
		hasLabel(issue.Labels, sweeperCfg.Lifecycle.ClosedLabel) ||
		hasLabel(issue.Labels, sweeperCfg.Security.QuarantineLabel)
}

func (r *Runner) shouldRunTick(projectID string) bool {
	interval := r.pollInterval(projectID)
	if interval <= 0 {
		return true
	}
	now := r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	lastRun, ok := r.lastTickByProject[projectID]
	if ok && now.Sub(lastRun) < interval {
		return false
	}
	r.lastTickByProject[projectID] = now
	return true
}

func (r *Runner) pollInterval(projectID string) time.Duration {
	if r == nil || r.config == nil {
		return 0
	}
	roleCfg := config.ProjectRoleConfigs(*r.config, projectID).Coordinator
	interval, err := time.ParseDuration(strings.TrimSpace(roleCfg.PollInterval))
	if err != nil {
		return 0
	}
	return interval
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

package sweeper

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	QueueType              = "sweeper"
	QueueTypeWarn          = "sweeper:warn"
	QueueTypeClose         = "sweeper:close"
	QueueTypeReconcile     = "sweeper:reconcile"
	defaultClaimedBy       = "sweeper"
	defaultSkippedSummary  = "sweeper runner skeleton: no-op"
	javaScriptISOStringUTC = "2006-01-02T15:04:05.000Z"
)

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Limit     int
}

type DiscoveryResult struct {
	QueueItems []storage.QueueItemRecord
	Skipped    int
}

type ProcessResult struct {
	QueueItemID string
	Status      string
	Summary     string
}

type Options struct {
	Repos  *storage.Repositories
	Logger bootstrap.Logger
	Now    func() time.Time
	Config *config.Config
}

type Runner struct {
	repos   *storage.Repositories
	logger  bootstrap.Logger
	now     func() time.Time
	config  *config.Config
	claimer string
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{repos: options.Repos, logger: options.Logger, now: now, config: options.Config, claimer: defaultClaimedBy}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discover(ctx, input)
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discover(ctx, input)
}

func (r *Runner) DiscoverReconcile(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discover(ctx, input)
}

func (r *Runner) ProcessNext(ctx context.Context, claimedBy string) (*ProcessResult, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("sweeper queue repository is not configured")
	}
	claimedBy = strings.TrimSpace(claimedBy)
	if claimedBy == "" {
		claimedBy = r.claimer
	}
	for _, queueType := range []string{QueueTypeWarn, QueueTypeClose, QueueTypeReconcile, QueueType} {
		item, err := r.repos.Queue.ClaimNextOfType(ctx, r.nowISO(), claimedBy, queueType)
		if err != nil {
			return nil, err
		}
		if item == nil {
			continue
		}
		return r.ProcessClaimedQueueItem(ctx, *item)
	}
	return nil, nil
}

func (r *Runner) ProcessClaimedQueueItem(ctx context.Context, queueItem storage.QueueItemRecord) (*ProcessResult, error) {
	if !isSupportedQueueType(queueItem.Type) {
		return nil, fmt.Errorf("unsupported sweeper queue item type %q", queueItem.Type)
	}
	if strings.TrimSpace(queueItem.ID) == "" {
		return nil, fmt.Errorf("sweeper queue item id is required")
	}
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("sweeper queue repository is not configured")
	}
	result := &ProcessResult{QueueItemID: queueItem.ID, Status: "skipped", Summary: defaultSkippedSummary}
	if err := r.repos.Queue.Complete(ctx, queueItem.ID, r.nowISO()); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Runner) discover(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if r.repos == nil || r.repos.Projects == nil {
		return DiscoveryResult{}, fmt.Errorf("sweeper project repository is not configured")
	}
	project, err := r.repos.Projects.GetByID(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project == nil {
		return DiscoveryResult{}, fmt.Errorf("project not found: %s", input.ProjectID)
	}
	if project.Archived || !r.autoDiscoveryEnabled(input.ProjectID) {
		return DiscoveryResult{Skipped: 1}, nil
	}
	return DiscoveryResult{}, nil
}

func (r *Runner) autoDiscoveryEnabled(projectID string) bool {
	if r.config == nil {
		return true
	}
	return config.ProjectRoleConfigs(*r.config, projectID).Sweeper.AutoDiscovery
}

func (r *Runner) nowISO() string {
	return r.now().UTC().Format(javaScriptISOStringUTC)
}

func isSupportedQueueType(queueType string) bool {
	switch queueType {
	case QueueType, QueueTypeWarn, QueueTypeClose, QueueTypeReconcile:
		return true
	default:
		return false
	}
}

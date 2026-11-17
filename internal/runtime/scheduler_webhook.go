package runtime

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/webhookforward"
	"github.com/nexu-io/looper/internal/worker"
)

// Issue and Actions events reuse existing role discovery and eligibility.
// Issue discovery fetches the event's issue directly; Actions without a PR
// reference coalesce a scan per project. Queue callbacks wake the claimer.
func discoverWebhookProject(ctx context.Context, input defaultSchedulerTickInput, request webhookforward.ProjectDiscovery) error {
	if input.Config == nil {
		return errors.New("webhook discovery configuration is unavailable")
	}
	projectID, repo := request.ProjectID, request.Repo
	var project config.ProjectRefConfig
	found := false
	for _, candidate := range input.Config.Projects {
		if candidate.ID == projectID {
			project, found = candidate, true
			break
		}
	}
	identity, resolved := config.ProjectRepositoryIdentity(*input.Config, project)
	if !found || !resolved || identity.Key() != request.RepositoryKey || config.ProjectWebhookMode(*input.Config, project) != config.WebhookModeTunnel {
		return nil
	}
	roles := config.ProjectRoleConfigs(*input.Config, projectID)
	var errs []error
	for _, lane := range request.Lanes {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		var err error
		switch lane {
		case webhookforward.LanePlanner:
			if input.Planner != nil && roles.Planner.AutoDiscovery && discoveryEnabled(input.PlannerDiscoveryEnabled) {
				_, err = input.Planner.DiscoverIssues(ctx, planner.DiscoveryInput{ProjectID: projectID, Repo: repo, IssueNumber: request.Number})
			}
		case webhookforward.LaneWorker:
			if runner, ok := input.Worker.(workerIssueDiscoveryScheduler); ok && roles.Worker.AutoDiscovery && discoveryEnabled(input.WorkerDiscoveryEnabled) {
				_, err = runner.DiscoverIssues(ctx, worker.DiscoveryInput{ProjectID: projectID, Repo: repo, IssueNumber: request.Number})
			}
		case webhookforward.LaneReviewer:
			if input.Reviewer != nil && roles.Reviewer.Discovery.AutoDiscovery && discoveryEnabled(input.ReviewerDiscoveryEnabled) {
				if request.ObjectType == "pull_request" {
					_, err = input.Reviewer.DiscoverPullRequest(ctx, reviewer.TargetedDiscoveryInput{ProjectID: projectID, Repo: repo, PRNumber: request.Number})
				} else {
					_, err = input.Reviewer.DiscoverPullRequests(ctx, reviewer.DiscoveryInput{ProjectID: projectID, Repo: repo})
				}
			}
		case webhookforward.LaneFixer:
			if input.Fixer != nil && roles.Fixer.AutoDiscovery && discoveryEnabled(input.FixerDiscoveryEnabled) {
				switch request.ObjectType {
				case "pull_request":
					_, err = input.Fixer.DiscoverPullRequest(ctx, fixer.TargetedDiscoveryInput{ProjectID: projectID, Repo: repo, PRNumber: request.Number})
				case "base_branch":
					_, err = input.Fixer.DiscoverPullRequestsForBaseBranchUpdate(ctx, fixer.BaseBranchDiscoveryInput{ProjectID: projectID, Repo: repo, BaseRefName: request.Branch})
				default:
					_, err = input.Fixer.DiscoverPullRequests(ctx, fixer.DiscoveryInput{ProjectID: projectID, Repo: repo})
				}
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func forgejoDiscoveryIssues(ctx context.Context, client *forge.ForgejoClient, input forge.ListIssuesInput, number int64) ([]forge.Issue, error) {
	if number <= 0 {
		return client.ListOpenIssues(ctx, input)
	}
	issue, err := client.ViewIssue(ctx, number)
	var apiErr *forge.ForgejoHTTPError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(issue.State, "open") || issue.IsPullRequest {
		return nil, nil
	}
	return []forge.Issue{issue}, nil
}

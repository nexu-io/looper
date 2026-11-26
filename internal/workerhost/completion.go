package workerhost

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func NotifyWorkerCompleted(ctx context.Context, cfg config.Config, repos *storage.Repositories, notificationGateway *notify.Gateway, logger bootstrap.Logger, input worker.RunCompletedInput) error {
	workerNotificationKeyID := firstNonEmpty(input.RunID, input.LoopID)
	payload := notify.SystemNotificationPayload{
		ProjectID:  input.ProjectID,
		LoopID:     input.LoopID,
		RunID:      input.RunID,
		Subtitle:   input.Subtitle,
		EntityType: "run",
		EntityID:   input.RunID,
	}
	switch {
	case input.Status == "failed" && input.FailureKind == worker.FailureManualIntervention:
		payload.Level = "action_required"
		payload.Title = "Looper Worker Needs Attention"
		payload.Body = input.Summary
		payload.DedupeKey = fmt.Sprintf("runtime.worker.action_required:%s", workerNotificationKeyID)
	case input.Status == "failed":
		payload.Level = "failure"
		payload.Title = "Looper Worker Failed"
		payload.Body = input.Summary
		payload.DedupeKey = fmt.Sprintf("runtime.worker.failed:%s", workerNotificationKeyID)
	case input.Status == "skipped":
		payload.Level = "action_required"
		payload.Title = "Looper Worker Needs Attention"
		payload.Body = input.Summary
		payload.DedupeKey = fmt.Sprintf("runtime.worker.action_required:%s", workerNotificationKeyID)
	case input.PullRequestNumber > 0:
		payload.Level = "action_required"
		payload.Title = "Looper Worker Opened a PR"
		payload.Body = fmt.Sprintf("PR #%d is ready: %s", input.PullRequestNumber, firstNonEmpty(input.PullRequestURL, input.Summary))
		payload.DedupeKey = fmt.Sprintf("runtime.worker.pr_ready:%s", input.RunID)
		// The requested work is delivered — @-mention the owner so it isn't missed.
		payload.MentionOpenIds = ownerMentions(cfg, input.ProjectID)
		// Reflect the PR in Plane's own state column: In Progress → In Review. Plane
		// projects only; best-effort (a failure must never disturb the notification).
		SetPlaneWorkItemState(ctx, &cfg, repos, logger, input.ProjectID, input.LoopID, "In Review")
	default:
		payload.Level = "success"
		payload.Title = "执行结果"
		payload.Body = firstNonEmpty(input.Summary, "Worker completed successfully")
		payload.DedupeKey = fmt.Sprintf("runtime.worker.completed:%s", input.RunID)
	}
	notificationGateway.Notify(ctx, payload)
	// Log the outcome to the loop's story so the anchor reads as a narrative.
	if strings.TrimSpace(input.LoopID) != "" && strings.EqualFold(strings.TrimSpace(cfg.Notifications.Webhook.Mode), "app") {
		switch {
		case input.PullRequestNumber > 0:
			link := firstNonEmpty(input.PullRequestURL, "")
			if link != "" {
				notificationGateway.RecordMilestone(ctx, input.LoopID, fmt.Sprintf("🔀 已开 [PR #%d](%s)", input.PullRequestNumber, link))
			} else {
				notificationGateway.RecordMilestone(ctx, input.LoopID, fmt.Sprintf("🔀 已开 PR #%d", input.PullRequestNumber))
			}
		case input.Status == "failed" && input.FailureKind == worker.FailureManualIntervention:
			notificationGateway.RecordMilestone(ctx, input.LoopID, "⏸ 需要人处理")
		case input.Status == "failed":
			notificationGateway.RecordMilestone(ctx, input.LoopID, "⚠️ 本轮失败,重试中")
		case input.Status != "skipped":
			notificationGateway.RecordMilestone(ctx, input.LoopID, "已提交执行结果")
		}
	} else {
		notificationGateway.RefreshThreadHeader(ctx, input.LoopID, nil, 0)
	}
	return nil
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
func ownerMentions(cfg config.Config, project string) []string {
	if owner := config.ProjectOwner(cfg, project); owner != "" {
		return []string{owner}
	}
	return nil
}

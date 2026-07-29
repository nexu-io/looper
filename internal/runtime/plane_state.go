package runtime

import (
	"context"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/storage"
)

// setPlaneWorkItemState reflects a loop's lifecycle in the Plane work item's own state
// column (In Progress → In Review → Done), alongside the label-driven pipeline. It
// resolves the Plane gateway for the project, reads the loop's originating work-item URL
// from metadata, and PATCHes the state. Plane projects only (a no-op for github/forgejo).
//
// Strictly best-effort: every failure path returns silently or logs a warning, so a
// Plane hiccup can never block the notification / label / merge flow that calls it.
func setPlaneWorkItemState(ctx context.Context, cfg *config.Config, repos *storage.Repositories, logger bootstrap.Logger, projectID, loopID, stateName string) bool {
	gateway, planeProjectID, ok := planeDocForProject(cfg, projectID)
	if !ok || gateway == nil {
		return true // not a Plane project — nothing to retry
	}
	if repos == nil || repos.Loops == nil {
		return false
	}
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return false
	}
	issueURL, _ := loopIssueURLAndSpecPR(loop.MetadataJSON)
	workItemID := planedoc.WorkItemIDFromURL(issueURL)
	if workItemID == "" {
		return false
	}
	if err := gateway.SetWorkItemState(ctx, planeProjectID, workItemID, stateName); err != nil {
		if logger != nil {
			logger.Warn("set plane work item state failed (continuing)", map[string]any{"loopId": loopID, "state": stateName, "error": err.Error()})
		}
		return false
	}
	return true
}

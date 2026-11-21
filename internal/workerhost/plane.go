package workerhost

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/storage"
)

// PlaneDocForProject builds a Plane spec-document gateway + the Plane project UUID
// for a project whose task-source provider is "plane", so the planner / worker /
// reviewer can read and write Plane spec pages and work-item links at runtime (the
// §8 flowchart). Returns (nil, "", false) for github/forgejo projects — those keep
// the repo-file spec path unchanged. The API key comes from the provider's TokenEnv
// (same env var the Plane task-source client uses).
func PlaneDocForProject(cfg *config.Config, projectID string) (*planedoc.Gateway, string, bool) {
	if cfg == nil {
		return nil, "", false
	}
	projectID = strings.TrimSpace(projectID)
	for _, project := range cfg.Projects {
		if project.ID != projectID {
			continue
		}
		if config.ResolvedProjectProviderKind(*cfg, project) != config.ProviderKindPlane {
			return nil, "", false
		}
		provider, found := ForgejoProviderByID(*cfg, project.Provider)
		if !found {
			return nil, "", false
		}
		apiKey := ""
		if provider.TokenEnv != nil {
			apiKey = strings.TrimSpace(os.Getenv(strings.TrimSpace(*provider.TokenEnv)))
		}
		workspace, planeProjectID := "", ""
		if provider.Workspace != nil {
			workspace = strings.TrimSpace(*provider.Workspace)
		}
		if provider.ProjectID != nil {
			planeProjectID = strings.TrimSpace(*provider.ProjectID)
		}
		gateway := planedoc.New(planedoc.Options{
			PlanePath:  derefString(cfg.Tools.PlanePath),
			APIBaseURL: strings.TrimSpace(provider.BaseURL),
			APIKey:     apiKey,
			Workspace:  workspace,
		})
		return gateway, planeProjectID, true
	}
	return nil, "", false
}

// SetPlaneWorkItemState reflects a loop's lifecycle in the Plane work item's own state
// column (In Progress → In Review → Done), alongside the label-driven pipeline. It
// resolves the Plane gateway for the project, reads the loop's originating work-item URL
// from metadata, and PATCHes the state. Plane projects only (a no-op for github/forgejo).
//
// Strictly best-effort: every failure path returns silently or logs a warning, so a
// Plane hiccup can never block the notification / label / merge flow that calls it.
func SetPlaneWorkItemState(ctx context.Context, cfg *config.Config, repos *storage.Repositories, logger bootstrap.Logger, projectID, loopID, stateName string) bool {
	gateway, planeProjectID, ok := PlaneDocForProject(cfg, projectID)
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
	issueURL, _ := LoopIssueURLAndSpecPR(loop.MetadataJSON)
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

// LoopIssueURLAndSpecPR reads a planner loop's originating issue URL + whether it has
// already opened a spec PR, from the loop metadata.
func LoopIssueURLAndSpecPR(metadataJSON *string) (issueURL string, hasSpecPR bool) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return "", false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return "", false
	}
	if s, ok := meta["issueUrl"].(string); ok {
		issueURL = strings.TrimSpace(s)
	} else if s, ok := meta["issueURL"].(string); ok {
		issueURL = strings.TrimSpace(s)
	}
	// Worker loops carry only prUrl at the top level and nest the originating
	// work-item URL under worker.issueUrl. Planner loops record it top-level (above).
	// Without this fallback, setPlaneWorkItemState reads "" for every worker loop,
	// so the Plane state never advances to In Review / Done for a worker-opened PR.
	if issueURL == "" {
		if worker, ok := meta["worker"].(map[string]any); ok {
			if value, ok := worker["issueUrl"].(string); ok {
				issueURL = strings.TrimSpace(value)
			} else if value, ok := worker["issueURL"].(string); ok {
				issueURL = strings.TrimSpace(value)
			}
		}
	}
	if value, ok := meta["prUrl"].(string); ok && strings.TrimSpace(value) != "" {
		hasSpecPR = true
	}
	return issueURL, hasSpecPR
}

package runtime

import (
	"os"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
)

type planeDocFactory func(*config.Config, string) (*planedoc.Gateway, string, bool)

func (r *Runtime) resolvePlaneDoc(cfg *config.Config, projectID string) (*planedoc.Gateway, string, bool) {
	if r != nil && r.planeDocFactory != nil {
		return r.planeDocFactory(cfg, projectID)
	}
	return planeDocForProject(cfg, projectID)
}

// planeDocForProject builds a Plane spec-document gateway + the Plane project UUID
// for a project whose task-source provider is "plane", so the planner / worker /
// reviewer can read and write Plane spec pages and work-item links at runtime (the
// §8 flowchart). Returns (nil, "", false) for github/forgejo projects — those keep
// the repo-file spec path unchanged. The API key comes from the provider's TokenEnv
// (same env var the Plane task-source client uses).
func planeDocForProject(cfg *config.Config, projectID string) (*planedoc.Gateway, string, bool) {
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
		provider, found := forgejoProviderByID(*cfg, project.Provider)
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

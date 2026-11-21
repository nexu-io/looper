package planner

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/storage"
)

// TestProductSpecGateLive exercises the planner's node D/E against a real Plane
// deployment: a feature work item with NO product spec → the gate comments on the
// work item asking product and returns a hold. Skipped unless PLANE_LIVE_E2E=1
// (needs PLANE_API_KEY + the test project/work-item). Cleans up the comment it posts.
func TestProductSpecGateLive(t *testing.T) {
	if os.Getenv("PLANE_LIVE_E2E") != "1" || os.Getenv("PLANE_API_KEY") == "" {
		t.Skip("set PLANE_LIVE_E2E=1 + PLANE_API_KEY to run the live planner gate test")
	}
	planeProject := envOr("PLANE_TEST_PROJECT", "db35f0e7-5004-4632-ba84-074164c95491")
	workItem := envOr("PLANE_TEST_WORK_ITEM", "4a59e298-3901-4642-a3c3-d80a9a0c7697")
	gw := planedoc.New(planedoc.Options{
		APIBaseURL: envOr("PLANE_API_BASE_URL", "https://plane.powerformer.net/api/v1"),
		APIKey:     os.Getenv("PLANE_API_KEY"),
		Workspace:  envOr("PLANE_WORKSPACE_SLUG", "open-design"),
	})
	ctx := context.Background()

	// Ensure the work item has no product-spec link so the gate holds.
	if links, err := gw.ListWorkItemLinks(ctx, planeProject, workItem); err == nil {
		for _, l := range links {
			if l.Title == planedoc.ProductSpecLinkTitle {
				t.Skip("work item already has a product spec; test needs one without")
			}
		}
	}

	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, planeProject, true }}
	issueURL := "https://plane.powerformer.net/open-design/projects/" + planeProject + "/issues/" + workItem
	in := stepInput{Project: storage.ProjectRecord{ID: "live-proj"}}
	cp := plannerCheckpoint{Issue: &checkpointIssue{Title: "LIVE gate test", URL: issueURL, Labels: []string{"kind/feature", "looper:plan"}}}

	gateErr := r.productSpecGate(ctx, in, cp)
	if gateErr == nil || gateErr.kind != FailureManualIntervention {
		t.Fatalf("gate = %v, want a manual-intervention hold (feature without product spec)", gateErr)
	}
	t.Logf("gate held (no product spec): %s", gateErr.message)

	// Now link a product spec and verify the gate PROCEEDS (node D pass path).
	page, err := gw.CreatePage(ctx, planeProject, "LIVE product spec", "# Product spec\n验收: e2e")
	if err != nil {
		t.Fatalf("CreatePage error = %v", err)
	}
	if err := gw.UpsertSpecLink(ctx, planeProject, workItem, planedoc.ProductSpecLinkTitle, page.URL); err != nil {
		t.Fatalf("UpsertSpecLink error = %v", err)
	}
	if gateErr := r.productSpecGate(ctx, in, cp); gateErr != nil {
		t.Fatalf("gate = %v, want nil (product spec present → proceed)", gateErr)
	}
	t.Logf("gate proceeded once product spec was linked (page %s) — clean up externally", page.ID)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

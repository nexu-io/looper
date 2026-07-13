package planedoc

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestGatewayLiveRoundTrip exercises the real `plane` CLI against a live Plane
// deployment: create a page → associate it as the work item's tech-spec link →
// reverse-query it → read the body → clean up. Skipped unless PLANE_LIVE_E2E=1
// (needs PLANE_API_KEY + the test project/work-item). It proves the gateway's arg
// construction matches the actual CLI, not just the fake.
func TestGatewayLiveRoundTrip(t *testing.T) {
	if os.Getenv("PLANE_LIVE_E2E") != "1" {
		t.Skip("set PLANE_LIVE_E2E=1 (and PLANE_API_KEY) to run the live Plane round-trip")
	}
	key := os.Getenv("PLANE_API_KEY")
	if key == "" {
		t.Skip("PLANE_API_KEY not set")
	}
	proj := envOr("PLANE_TEST_PROJECT", "db35f0e7-5004-4632-ba84-074164c95491")
	wi := envOr("PLANE_TEST_WORK_ITEM", "4a59e298-3901-4642-a3c3-d80a9a0c7697")

	g := New(Options{
		APIBaseURL: envOr("PLANE_API_BASE_URL", "https://plane.powerformer.net/api/v1"),
		APIKey:     key,
		Workspace:  envOr("PLANE_WORKSPACE_SLUG", "open-design"),
	})
	ctx := context.Background()

	// §8.4 write path: create the tech-spec page AND associate it in one call.
	page, err := g.WriteTechSpec(ctx, proj, wi, "LIVE-looper-tech-spec", "# 技术方案\n- gateway 集成冒烟")
	if err != nil {
		t.Fatalf("WriteTechSpec error = %v", err)
	}
	t.Logf("wrote tech spec page %s (%s)", page.ID, page.URL)

	// Idempotent: re-associating the same URL is a no-op (no duplicate link).
	if err := g.UpsertSpecLink(ctx, proj, wi, TechSpecLinkTitle, page.URL); err != nil {
		t.Fatalf("UpsertSpecLink (repeat) error = %v", err)
	}
	url, found, err := g.FindSpecLink(ctx, proj, wi, TechSpecLinkTitle)
	if err != nil || !found || url != page.URL {
		t.Fatalf("FindSpecLink = %q, %v, %v; want %q", url, found, err, page.URL)
	}
	// §8.4 read path: resolve the work item's tech spec (link → page → body).
	html, found, err := g.ReadSpec(ctx, proj, wi, TechSpecLinkTitle)
	if err != nil || !found || !strings.Contains(html, "技术方案") {
		t.Fatalf("ReadSpec = %q (found %v, err %v), want the body HTML", html, found, err)
	}

	// Cleanup: remove the spike link + page directly via the CLI.
	links, _ := g.ListWorkItemLinks(ctx, proj, wi)
	for _, l := range links {
		if l.Title == TechSpecLinkTitle {
			_, _ = g.runPlane(ctx, "", append([]string{"api", "link", "delete", "--project", proj, "--work-item", wi, l.ID}, g.globalArgs()...)...)
		}
	}
	_, _ = g.runPlane(ctx, "", append([]string{"api", "page", "delete", "--project", proj, page.ID}, g.globalArgs()...)...)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

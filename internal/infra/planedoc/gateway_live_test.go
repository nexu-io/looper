package planedoc

import (
	"context"
	"encoding/json"
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

// TestGatewayLivePageComments exercises node H's review surface against the real
// deployment (powerformer/plane PR #11 page comments, reached via the request escape
// hatch): create a page → post a comment → list it back → clean up. Skipped unless
// PLANE_LIVE_E2E=1.
func TestGatewayLivePageComments(t *testing.T) {
	if os.Getenv("PLANE_LIVE_E2E") != "1" {
		t.Skip("set PLANE_LIVE_E2E=1 (and PLANE_API_KEY) to run the live page-comment test")
	}
	key := os.Getenv("PLANE_API_KEY")
	if key == "" {
		t.Skip("PLANE_API_KEY not set")
	}
	proj := envOr("PLANE_TEST_PROJECT", "db35f0e7-5004-4632-ba84-074164c95491")
	g := New(Options{
		APIBaseURL: envOr("PLANE_API_BASE_URL", "https://plane.powerformer.net/api/v1"),
		APIKey:     key,
		Workspace:  envOr("PLANE_WORKSPACE_SLUG", "open-design"),
	})
	ctx := context.Background()

	page, err := g.CreatePage(ctx, proj, "LIVE-node-H-page-comments", "# tech spec\n验收: e2e")
	if err != nil {
		t.Fatalf("CreatePage error = %v", err)
	}
	defer func() {
		_, _ = g.runPlane(ctx, "", append([]string{"api", "page", "delete", "--project", proj, page.ID}, g.globalArgs()...)...)
	}()

	body := "<p>[looper] node H 辅助审:第2节验收标准缺失,请补。</p>"
	created, err := g.CreatePageComment(ctx, proj, page.ID, body)
	if err != nil {
		t.Fatalf("CreatePageComment error = %v", err)
	}
	if !strings.Contains(created.CommentStripped, "验收标准缺失") {
		t.Fatalf("created comment = %q, want the posted text", created.CommentStripped)
	}
	t.Logf("posted page comment %s by %s", created.ID, created.DisplayName)

	comments, err := g.ListPageComments(ctx, proj, page.ID)
	if err != nil {
		t.Fatalf("ListPageComments error = %v", err)
	}
	found := false
	for _, c := range comments {
		if c.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListPageComments (%d) did not include the posted comment %s", len(comments), created.ID)
	}
	t.Logf("listed %d page comment(s), found the posted one", len(comments))
}

// TestGatewayLiveAddWorkItemLabel exercises node H's worker-dispatch primitive against
// the real deployment: create a work item → add looper:worker-ready → verify it's on
// the item → idempotent re-add → clean up. Skipped unless PLANE_LIVE_E2E=1.
func TestGatewayLiveAddWorkItemLabel(t *testing.T) {
	if os.Getenv("PLANE_LIVE_E2E") != "1" || os.Getenv("PLANE_API_KEY") == "" {
		t.Skip("set PLANE_LIVE_E2E=1 + PLANE_API_KEY to run the live add-label test")
	}
	proj := envOr("PLANE_TEST_PROJECT", "db35f0e7-5004-4632-ba84-074164c95491")
	g := New(Options{
		APIBaseURL: envOr("PLANE_API_BASE_URL", "https://plane.powerformer.net/api/v1"),
		APIKey:     os.Getenv("PLANE_API_KEY"),
		Workspace:  envOr("PLANE_WORKSPACE_SLUG", "open-design"),
	})
	ctx := context.Background()

	created, err := g.runPlane(ctx, "", append([]string{"api", "work-item", "create", "--project", proj, "--name", "LIVE-add-label-probe", "--json"}, g.globalArgs()...)...)
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	var wi struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(created.Stdout)), &wi); err != nil || wi.ID == "" {
		t.Fatalf("decode work item: %v (%q)", err, created.Stdout)
	}
	defer func() {
		_, _ = g.runPlane(ctx, "", append([]string{"api", "work-item", "delete", "--project", proj, wi.ID}, g.globalArgs()...)...)
	}()

	if err := g.AddWorkItemLabel(ctx, proj, wi.ID, "looper:worker-ready"); err != nil {
		t.Fatalf("AddWorkItemLabel: %v", err)
	}
	ids, err := g.workItemLabelIDs(ctx, proj, wi.ID)
	if err != nil || len(ids) == 0 {
		t.Fatalf("workItemLabelIDs = %v, %v; want the added label", ids, err)
	}
	// Idempotent re-add is a no-op (label set unchanged).
	if err := g.AddWorkItemLabel(ctx, proj, wi.ID, "looper:worker-ready"); err != nil {
		t.Fatalf("AddWorkItemLabel (repeat): %v", err)
	}
	ids2, _ := g.workItemLabelIDs(ctx, proj, wi.ID)
	if len(ids2) != len(ids) {
		t.Fatalf("re-add changed the label set: %v → %v", ids, ids2)
	}
	t.Logf("added looper:worker-ready to work item %s (labels=%v)", wi.ID, ids)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

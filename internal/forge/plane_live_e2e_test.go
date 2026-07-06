package forge

import (
	"context"
	"os"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

// TestPlaneAssigneeFilterLiveE2E exercises the real Plane API to prove that
// ListOpenIssues(Assignee: <uuid>) — the primitive per-person Plane routing is
// wired onto — returns only work-items assigned to that member UUID. Gated:
//
//	PLANE_LIVE_E2E=1 PLANE_API_KEY=... PLANE_E2E_WORKSPACE=... \
//	PLANE_E2E_PROJECT=... PLANE_E2E_ASSIGNEE=<present-uuid> \
//	go test ./internal/forge -run TestPlaneAssigneeFilterLiveE2E -count=1
func TestPlaneAssigneeFilterLiveE2E(t *testing.T) {
	if os.Getenv("PLANE_LIVE_E2E") != "1" {
		t.Skip("set PLANE_LIVE_E2E=1 (+ PLANE_API_KEY/PLANE_E2E_WORKSPACE/PLANE_E2E_PROJECT/PLANE_E2E_ASSIGNEE) to run")
	}
	workspace := os.Getenv("PLANE_E2E_WORKSPACE")
	projectID := os.Getenv("PLANE_E2E_PROJECT")
	assignee := os.Getenv("PLANE_E2E_ASSIGNEE")
	if workspace == "" || projectID == "" || assignee == "" || os.Getenv("PLANE_API_KEY") == "" {
		t.Fatal("PLANE_API_KEY, PLANE_E2E_WORKSPACE, PLANE_E2E_PROJECT, PLANE_E2E_ASSIGNEE are required")
	}
	tokenEnv := "PLANE_API_KEY"
	baseURL := os.Getenv("PLANE_E2E_BASE_URL")
	provider := config.ProviderConfig{ID: "plane-e2e", Kind: config.ProviderKindPlane, BaseURL: baseURL, TokenEnv: &tokenEnv, Workspace: &workspace, ProjectID: &projectID}
	client, err := NewPlaneClientFromConfig(provider, "acme/looper")
	if err != nil {
		t.Fatalf("NewPlaneClientFromConfig() error = %v", err)
	}
	ctx := context.Background()

	all, err := client.ListOpenIssues(ctx, ListIssuesInput{})
	if err != nil {
		t.Fatalf("ListOpenIssues(unfiltered) error = %v", err)
	}
	mine, err := client.ListOpenIssues(ctx, ListIssuesInput{Assignee: assignee})
	if err != nil {
		t.Fatalf("ListOpenIssues(assignee=%s) error = %v", assignee, err)
	}
	absent := "00000000-0000-0000-0000-000000000000"
	none, err := client.ListOpenIssues(ctx, ListIssuesInput{Assignee: absent})
	if err != nil {
		t.Fatalf("ListOpenIssues(assignee=%s) error = %v", absent, err)
	}

	t.Logf("unfiltered=%d assignee(%s)=%d absent=%d", len(all), assignee, len(mine), len(none))

	if len(mine) == 0 {
		t.Fatalf("assignee-filtered list is empty; expected at least one work-item assigned to %s", assignee)
	}
	if len(mine) >= len(all) {
		t.Fatalf("assignee-filtered=%d not a strict subset of unfiltered=%d; filter had no effect", len(mine), len(all))
	}
	if len(none) != 0 {
		t.Fatalf("filtering by an unassigned UUID returned %d items, want 0", len(none))
	}

	allNumbers := make(map[int64]bool, len(all))
	for _, issue := range all {
		allNumbers[issue.Number] = true
	}
	for _, issue := range mine {
		if !allNumbers[issue.Number] {
			t.Fatalf("assignee-filtered work-item #%d not present in the unfiltered listing", issue.Number)
		}
		if !identityListContains(issue.Assignees, assignee) {
			t.Fatalf("work-item #%d returned by the assignee filter does not carry assignee %s: %+v", issue.Number, assignee, issue.Assignees)
		}
	}
}

func identityListContains(ids []Identity, want string) bool {
	for _, id := range ids {
		if id.Login == want {
			return true
		}
	}
	return false
}

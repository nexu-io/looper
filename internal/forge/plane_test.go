package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// planeTestServer serves a canned Plane project (labels + work-items + user) and
// records the auth / user-agent headers of every request so the mapping and the
// Cloudflare-safe User-Agent can be asserted without touching the network.
func planeTestServer(t *testing.T) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var requests []recordedRequest
	const wsPrefix = "/api/v1/workspaces/acme/projects/proj-123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("X-API-Key"), Body: r.Header.Get("User-Agent")})
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/me/":
			writeJSON(t, w, http.StatusOK, map[string]any{"id": "me-uuid", "email": "ralph@example.test", "display_name": "Ralph"})
		case r.Method == http.MethodGet && r.URL.Path == wsPrefix+"/labels/":
			writeJSON(t, w, http.StatusOK, map[string]any{"results": []map[string]any{
				{"id": "lbl-plan", "name": "looper:plan"},
				{"id": "lbl-bug", "name": "bug"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == wsPrefix+"/work-items/":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"next_cursor":       "",
				"next_page_results": false,
				"results": []map[string]any{
					{"id": "wi-1", "sequence_id": 42, "name": "First item", "description_html": "<p>Hello <b>world</b></p>", "labels": []string{"lbl-plan"}, "assignees": []string{"user-uuid-1"}, "updated_at": "2026-06-30T00:00:00Z"},
					{"id": "wi-2", "sequence_id": 43, "name": "Second item", "description_html": "<p>No trigger</p>", "labels": []string{"lbl-bug"}, "assignees": []string{}},
				},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func newPlaneTestClient(t *testing.T, server *httptest.Server) *PlaneClient {
	t.Helper()
	client, err := NewPlaneClient(RepositoryRef{ProviderID: "plane-od", Kind: ProviderKindPlane, BaseURL: server.URL + "/api/v1", Repo: "acme/looper"}, "acme", "proj-123", "super-secret-key", WithPlaneHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("NewPlaneClient() error = %v", err)
	}
	return client
}

func TestPlaneListOpenIssuesMapsAndFiltersByLabel(t *testing.T) {
	t.Parallel()
	server, requests := planeTestServer(t)
	client := newPlaneTestClient(t, server)

	issues, err := client.ListOpenIssues(context.Background(), ListIssuesInput{Labels: []string{"looper:plan"}})
	if err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("ListOpenIssues() returned %d issues, want 1 (filtered by looper:plan)", len(issues))
	}
	issue := issues[0]
	if issue.Number != 42 {
		t.Fatalf("issue.Number = %d, want 42 (sequence_id)", issue.Number)
	}
	if issue.Title != "First item" {
		t.Fatalf("issue.Title = %q, want %q", issue.Title, "First item")
	}
	if issue.Body != "Hello world" {
		t.Fatalf("issue.Body = %q, want %q (HTML stripped)", issue.Body, "Hello world")
	}
	if len(issue.Labels) != 1 || issue.Labels[0].Name != "looper:plan" {
		t.Fatalf("issue.Labels = %+v, want [looper:plan] (UUID resolved to name)", issue.Labels)
	}
	if len(issue.Assignees) != 1 || issue.Assignees[0].Login != "user-uuid-1" {
		t.Fatalf("issue.Assignees = %+v, want [user-uuid-1]", issue.Assignees)
	}
	if !strings.Contains(issue.HTMLURL, "/acme/projects/proj-123/issues/wi-1") {
		t.Fatalf("issue.HTMLURL = %q, want it to point at the Plane work-item web page", issue.HTMLURL)
	}

	// Every request must carry the API key and a custom (non-Go-default) UA so
	// Cloudflare's WAF does not block it.
	if len(*requests) == 0 {
		t.Fatalf("expected at least one recorded request")
	}
	for _, req := range *requests {
		if req.Auth != "super-secret-key" {
			t.Fatalf("request %s %s X-API-Key = %q, want the configured key", req.Method, req.Path, req.Auth)
		}
		if req.Body == "" || strings.HasPrefix(req.Body, "Go-http-client") {
			t.Fatalf("request %s %s User-Agent = %q, want a custom UA", req.Method, req.Path, req.Body)
		}
	}
}

func TestPlaneViewIssueResolvesBySequenceID(t *testing.T) {
	t.Parallel()
	server, _ := planeTestServer(t)
	client := newPlaneTestClient(t, server)

	issue, err := client.ViewIssue(context.Background(), 43)
	if err != nil {
		t.Fatalf("ViewIssue() error = %v", err)
	}
	if issue.Number != 43 {
		t.Fatalf("issue.Number = %d, want 43", issue.Number)
	}
	if issue.Title != "Second item" {
		t.Fatalf("issue.Title = %q, want %q", issue.Title, "Second item")
	}
	if len(issue.Labels) != 1 || issue.Labels[0].Name != "bug" {
		t.Fatalf("issue.Labels = %+v, want [bug]", issue.Labels)
	}
}

func TestPlaneCurrentUserIdentity(t *testing.T) {
	t.Parallel()
	server, _ := planeTestServer(t)
	client := newPlaneTestClient(t, server)

	identity, err := client.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser() error = %v", err)
	}
	if identity.Login != "Ralph" {
		t.Fatalf("identity.Login = %q, want %q", identity.Login, "Ralph")
	}
}

func TestPlaneCapabilitiesAndKind(t *testing.T) {
	t.Parallel()
	server, _ := planeTestServer(t)
	client := newPlaneTestClient(t, server)

	if client.Kind() != ProviderKindPlane {
		t.Fatalf("Kind() = %q, want %q", client.Kind(), ProviderKindPlane)
	}
	caps := client.Capabilities()
	if !caps.Issues || !caps.Labels || !caps.Comments || !caps.Assignees {
		t.Fatalf("Capabilities() = %+v, want issue-side capabilities enabled", caps)
	}
	if caps.PullRequests || caps.Diffs || caps.NativeReviews {
		t.Fatalf("Capabilities() = %+v, want pull-request/diff/review capabilities disabled", caps)
	}
	if ref := client.Repository(); ref.Repo != "acme/looper" || ref.Kind != ProviderKindPlane {
		t.Fatalf("Repository() = %+v, want code repo acme/looper and kind plane", ref)
	}
}

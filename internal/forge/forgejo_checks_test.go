package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForgejoCommitChecksUseLatestStatesAcrossPages(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token super-secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/repos/acme/looper/statuses/head":
			if r.URL.Query().Get("sort") != "highestindex" {
				t.Error("status discovery must include successes and failures in descending ID order")
			}
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Total-Pages", "2")
				_, _ = w.Write([]byte(`[{"id":4,"context":"unit","status":"success"},{"id":3,"context":"lint","status":"failure","description":"lint failed in app.go","target_url":"https://ci.test/lint"}]`))
			} else {
				_, _ = w.Write([]byte(`[{"id":2,"context":"unit","status":"failure"}]`))
			}
		case "/api/v1/repos/acme/looper/actions/runs":
			if r.URL.Query().Get("head_sha") != "head" || r.URL.Query().Get("event") != "" {
				t.Errorf("action query = %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `</api/v1/repos/acme/looper/actions/runs?page=2>; rel="next"`)
				_, _ = w.Write([]byte(`{"workflow_runs":[{"id":100,"index_in_repo":9,"workflow_id":"ci.yml","commit_sha":"head","status":"success","event":"push","prettyref":"feature","html_url":"https://forge.test/actions/runs/9"},{"id":200,"workflow_id":"other.yml","commit_sha":"another-head","status":"failure"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"workflow_runs":[{"id":99,"workflow_id":"ci.yml","commit_sha":"head","status":"failure","event":"push","prettyref":"feature"},{"id":98,"workflow_id":"deploy.yml","commit_sha":"head","status":"failure","event":"push","prettyref":"feature","title":"Deploy failed","html_url":"https://forge.test/actions/runs/8"}]}`))
			}
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	checks, err := newForgejoTestClient(t, server.URL).ListCommitChecks(context.Background(), "head")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 4 {
		t.Fatalf("checks = %#v, want current status per context/workflow", checks)
	}
	if checks[0].Name != "ci.yml" || checks[0].State != "SUCCESS" || checks[0].ActionRunID != 100 {
		t.Errorf("action rerun = %#v, want success and API ID 100, not display index 9", checks[0])
	}
	if checks[1].Name != "deploy.yml" || checks[1].State != "FAILURE" || checks[1].Description != "Deploy failed" {
		t.Errorf("failed action = %#v", checks[1])
	}
	if checks[2].Name != "lint" || checks[2].URL != "https://ci.test/lint" || checks[2].Description != "lint failed in app.go" {
		t.Errorf("external failure diagnostics = %#v", checks[2])
	}
	if checks[3].Name != "unit" || checks[3].State != "SUCCESS" {
		t.Errorf("status rerun = %#v, historical failure must not remain actionable", checks[3])
	}
}

func TestForgejoCommitChecksDoNotHidePermissionOrTransportErrors(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/statuses/") {
					_, _ = w.Write([]byte(`[{"id":1,"context":"unit","status":"failure"}]`))
					return
				}
				w.WriteHeader(code)
			}))
			defer server.Close()
			checks, err := newForgejoTestClient(t, server.URL).ListCommitChecks(context.Background(), "head")
			if code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
				if err != nil || len(checks) != 1 || checks[0].State != "FAILURE" {
					t.Fatalf("checks = %#v, %v; unavailable Actions must preserve external checks", checks, err)
				}
				return
			}
			var httpErr *ForgejoHTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != code {
				t.Fatalf("error = %v, want HTTP %d", err, code)
			}
		})
	}
}

func TestForgejoPullRequestRetainsUnknownMergeability(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	for _, mergeable := range []*bool{nil, &no, &yes} {
		got := convertPullRequest(forgejoPullRequest{State: "open", Mergeable: mergeable}).Mergeable
		if got != mergeable {
			t.Fatalf("Mergeable = %v, want original tri-state %v", got, mergeable)
		}
	}
}

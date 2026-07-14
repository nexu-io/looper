package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForgejoNativeReviewRequestDiscoveryAndPublicationContract(t *testing.T) {
	requested := map[int64][]forgejoUser{2: {{ID: 7, Login: "reviewer"}}}
	reviews := map[int64][]forgejoPullRequestReview{}
	var reviewerPayload map[string][]string
	var reviewPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 1, "state": "open", "user": map[string]any{"login": "alice"}, "head": map[string]any{"sha": "head-1"}, "requested_reviewers": requested[1]},
				{"number": 2, "state": "open", "user": map[string]any{"login": "bob"}, "head": map[string]any{"sha": "head-2"}, "requested_reviewers": requested[2]},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/1/requested_reviewers":
			if err := json.NewDecoder(r.Body).Decode(&reviewerPayload); err != nil {
				t.Fatalf("decode reviewer payload: %v", err)
			}
			requested[1] = []forgejoUser{{ID: 7, Login: reviewerPayload["reviewers"][0]}}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/2/reviews":
			if err := json.NewDecoder(r.Body).Decode(&reviewPayload); err != nil {
				t.Fatalf("decode review payload: %v", err)
			}
			review := forgejoPullRequestReview{ID: 11, State: "APPROVED", Body: reviewPayload["body"].(string), CommitID: reviewPayload["commit_id"].(string), User: forgejoUser{ID: 7, Login: "reviewer"}}
			reviews[2] = append(reviews[2], review)
			_ = json.NewEncoder(w).Encode(review)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			var number int64
			_, _ = fmtSscanfPullNumber(r.URL.Path, &number)
			_ = json.NewEncoder(w).Encode(reviews[number])
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/reviews/") && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := newForgejoTestClient(t, server.URL)
	if err := client.AddPullRequestReviewers(context.Background(), 1, []string{"reviewer"}); err != nil {
		t.Fatalf("AddPullRequestReviewers() error = %v", err)
	}
	if got := reviewerPayload["reviewers"]; len(got) != 1 || got[0] != "reviewer" {
		t.Fatalf("reviewer payload = %#v", reviewerPayload)
	}
	discovered, err := client.ListReviewRequestedPullRequests(context.Background(), "reviewer", 10)
	if err != nil {
		t.Fatalf("ListReviewRequestedPullRequests() error = %v", err)
	}
	if len(discovered) != 2 || discovered[0].Number != 1 || discovered[1].Number != 2 {
		t.Fatalf("discovered = %#v, want PRs 1 and 2", discovered)
	}
	marker := "<!-- looper:review id=reviewer:loop:head-2 head=head-2 outcome=clean -->"
	if _, err := client.CreatePullRequestReview(context.Background(), CreatePullRequestReviewInput{Number: 2, Event: "APPROVE", CommitID: "head-2", Body: "Looks good\n\n" + marker}); err != nil {
		t.Fatalf("CreatePullRequestReview() error = %v", err)
	}
	if reviewPayload["event"] != "APPROVED" || reviewPayload["commit_id"] != "head-2" {
		t.Fatalf("review payload = %#v", reviewPayload)
	}
	listed, err := client.ListPullRequestReviews(context.Background(), 2)
	if err != nil {
		t.Fatalf("ListPullRequestReviews() error = %v", err)
	}
	if len(listed) != 1 || listed[0].State != "APPROVED" || !strings.Contains(listed[0].Body, marker) {
		t.Fatalf("reviews = %#v", listed)
	}
}

func TestForgejoNativeReviewOperationsFailThroughCapabilityPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/swagger.v1.json" {
			_, _ = w.Write([]byte(`{"paths":{}}`))
			return
		}
		t.Fatalf("native endpoint called despite unsupported capability: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	client := newForgejoTestClient(t, server.URL)
	err := client.AddPullRequestReviewers(context.Background(), 1, []string{"reviewer"})
	var capabilityErr *UnsupportedCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Capability != "reviewRequests" || capabilityErr.State != ProbeStateUnsupported {
		t.Fatalf("error = %T %v, want unsupported reviewRequests capability", err, err)
	}
}

func TestListPullRequestReviewsRequiresReviewCommentsCapability(t *testing.T) {
	// Instance advertises list/create reviews but not per-review comments.
	// List must fail the capability gate before calling native endpoints so a
	// later body-only submit cannot leave an untracked review after marker list fails.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/swagger.v1.json" {
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}}}}`))
			return
		}
		t.Fatalf("native endpoint called despite missing review comments capability: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := newForgejoTestClient(t, server.URL)
	_, err := client.ListPullRequestReviews(context.Background(), 2)
	var capabilityErr *UnsupportedCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Capability != "nativeReviews" || capabilityErr.State != ProbeStateUnsupported {
		t.Fatalf("error = %T %v, want unsupported nativeReviews capability", err, err)
	}
}

func fmtSscanfPullNumber(path string, number *int64) (int, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for index, part := range parts {
		if part == "pulls" && index+1 < len(parts) {
			return fmt.Sscanf(parts[index+1], "%d", number)
		}
	}
	return 0, errors.New("pull number not found")
}

package reviewer

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestVerifyAgentNativeReviewMarkerScopesBlockingCommentFallbackToForgejoAuthor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kind   config.ProviderKind
		author string
		allow  bool
	}{
		{name: "Forgejo self review", kind: config.ProviderKindForgejo, author: "Reviewer", allow: true},
		{name: "Forgejo other author", kind: config.ProviderKindForgejo, author: "alice"},
		{name: "Forgejo missing author", kind: config.ProviderKindForgejo},
		{name: "GitHub self review retains event policy", kind: config.ProviderKindGitHub, author: "reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg.Providers = []config.ProviderConfig{{ID: "provider", Kind: tc.kind}}
			cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Repo: "acme/looper", Provider: "provider"}}
			github := &fakeGitHubGateway{currentLogin: "reviewer", reviewMarkerMissing: true}
			runner := New(Options{GitHub: github, CustomInstructions: &cfg})
			input := stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/repo"}, Loop: storage.LoopRecord{ID: "loop"}, Repo: "acme/looper", PRNumber: 42}
			found, err := runner.verifyAgentNativeReviewMarker(context.Background(), input, "head", "", tc.author)
			if err != nil || found.Found {
				t.Fatalf("verify = %#v, %v; want no matching marker", found, err)
			}
			if len(github.reviewMarkerInputs) != 3 {
				t.Fatalf("marker lookups = %d, want all supported marker forms", len(github.reviewMarkerInputs))
			}
			for _, lookup := range github.reviewMarkerInputs {
				if lookup.AllowBlockingComment != tc.allow || lookup.AuthorLogin != "reviewer" || !strings.Contains(lookup.Marker, "head=head") {
					t.Fatalf("lookup = %#v; want current author/head and blocking fallback %t", lookup, tc.allow)
				}
			}
		})
	}
}

func TestForgejoSelfBlockingReviewBackfillsEngagementWithoutChangingGitHubPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kind   config.ProviderKind
		author string
		want   string
	}{
		{name: "Forgejo self review", kind: config.ProviderKindForgejo, author: "reviewer", want: "old-head"},
		{name: "Forgejo other author", kind: config.ProviderKindForgejo, author: "alice"},
		{name: "GitHub self review", kind: config.ProviderKindGitHub, author: "reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			cfg, err := config.DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg.Providers = []config.ProviderConfig{{ID: "provider", Kind: tc.kind}}
			cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Repo: "acme/looper", Provider: "provider"}}
			cfg.Roles.Reviewer.Behavior.ReviewEvents.Blocking = config.ReviewerReviewEventRequestChanges
			runner := New(Options{Repos: fixture.repos, CustomInstructions: &cfg})
			meta := `{"followUpdates":true,"loop":{"enabled":true}}`
			loop := storage.LoopRecord{ID: "self-review-loop", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Status: "completed", MetadataJSON: &meta, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
			if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
				t.Fatal(err)
			}
			detail := PullRequestDetail{Number: 42, HeadSHA: "new-head", Author: tc.author, Reviews: []map[string]any{{
				"user": map[string]any{"login": "reviewer"}, "state": "COMMENTED", "commit_id": "old-head",
				"body": "<!-- looper:review id=reviewer:self-review-loop head=old-head outcome=blocking -->",
			}}}
			updated, err := runner.backfillPublishedHeadFromLooperReview(context.Background(), storage.ProjectRecord{ID: "project_1"}, loop, detail, "reviewer")
			if err != nil {
				t.Fatal(err)
			}
			metadata := parseJSONObject(updated.MetadataJSON)
			got, _ := metadata["lastPublishedHeadSha"].(string)
			if got != tc.want {
				t.Fatalf("engaged head = %q, want %q", got, tc.want)
			}
		})
	}
}

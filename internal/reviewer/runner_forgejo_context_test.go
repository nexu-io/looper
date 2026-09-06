package reviewer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/forge"
)

func TestForgejoReviewerPromptUsesPersistedURLAndConfiguredTransport(t *testing.T) {
	t.Parallel()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example/forge", Auth: config.ProviderAuthTea, TeaPath: stringPtr("/opt/tea"), TeaLogin: stringPtr("team-login")}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "p", Provider: "fj", Repo: "core/looper"}}
	for _, legacy := range []bool{false, true} {
		for _, actualURL := range []string{"", "https://canonical.example/prefix/core/looper/pulls/42"} {
			checkpoint := reviewerCheckpoint{Detail: checkpointDetailFromDetail(PullRequestDetail{URL: actualURL, HeadSHA: "abc123", ChecksSummary: "unit: FAILURE"}), Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}
			encoded, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			var resumed reviewerCheckpoint
			if err := json.Unmarshal(encoded, &resumed); err != nil {
				t.Fatal(err)
			}
			prompt, _ := buildReviewPromptWithInstructions("p", cfg, "core/looper", 42, resumed, "run", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove, Blocking: config.ReviewerReviewEventRequestChanges}, false, true, "", config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "codex", "", "/opt/looper", false, legacy, "")
			wantURL := actualURL
			if wantURL == "" {
				wantURL = "https://code.example/forge/core/looper/pulls/42"
			}
			for _, required := range []string{`"url": "` + wantURL + `"`, "'/opt/tea' api --login 'team-login' -i", "unit: FAILURE"} {
				if !strings.Contains(prompt, required) {
					t.Errorf("legacy=%v url=%s missing %q", legacy, actualURL, required)
				}
			}
			if !legacy && !strings.Contains(prompt, disclosure.Slogan) {
				t.Error("native review prompt lost the shared GitHub disclosure slogan")
			}
			for _, forbidden := range []string{"https://github.com/core/looper/pull/42", "gh pr view", "gh api graphql"} {
				if strings.Contains(prompt, forbidden) {
					t.Errorf("Forgejo prompt contains %q", forbidden)
				}
			}
			if !legacy && (!strings.Contains(prompt, "downgrades REQUEST_CHANGES to COMMENT while retaining outcome=blocking") || strings.Contains(prompt, "Resolvable inline review comments are required")) {
				t.Error("native prompt lost self-blocking fallback or requires unsupported resolve")
			}
		}
	}
}

func TestForgejoReviewerLegacyCommentHasHumanProseAndSharedDisclosure(t *testing.T) {
	t.Parallel()
	summary := forge.NewReviewerSummary(4, []forge.ReviewItem{
		{ReviewItemID: "R-001", Status: forge.ReviewItemStatusOpen, Title: "Retain the discount", Body: "The price calculation drops the discount; apply it before rounding.", Files: []string{"price.go"}, LastSeenRoundID: 4},
		{ReviewItemID: "R-002", Status: forge.ReviewItemStatusResolved, Title: "Historical finding", Body: "Already fixed.", LastSeenRoundID: 3},
	})
	body, err := renderReviewerSummaryComment(summary, "I found one price calculation issue.")
	if err != nil {
		t.Fatal(err)
	}
	stamper := disclosure.Stamper{Config: config.DefaultDisclosureConfig(), Agent: "codex"}
	body = stamper.Markdown(body, "reviewer", disclosure.ChannelIssueComment)
	visible := reviewHumanVisibleBody(body)
	for _, required := range []string{"I found one price calculation issue.", "Retain the discount", "apply it before rounding", "price.go"} {
		if !strings.Contains(visible, required) {
			t.Errorf("visible comment missing %q: %s", required, visible)
		}
	}
	for _, forbidden := range []string{"Reviewer Summary", "Review round", "Open items", "Resolved", "R-001", "Historical finding", "schema_version"} {
		if strings.Contains(visible, forbidden) {
			t.Errorf("protocol leaked to visible comment %q: %s", forbidden, visible)
		}
	}
	if !strings.Contains(body, stamper.MarkdownStamp("reviewer")) || !strings.Contains(body, "runner=reviewer · agent=codex · "+disclosure.Slogan) {
		t.Fatalf("GitHub disclosure missing: %s", body)
	}
	parsed, err := forge.ParseReviewerSummary(body)
	if err != nil || parsed.ReviewRoundID != 4 || len(parsed.Items) != 2 || parsed.Items[1].Status != forge.ReviewItemStatusResolved {
		t.Fatalf("hidden legacy protocol = %#v, %v", parsed, err)
	}
}

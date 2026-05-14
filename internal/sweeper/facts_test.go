package sweeper

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

func TestBuildFingerprintSortsPolicyLabelsForStableOutput(t *testing.T) {
	t.Parallel()

	bundleA := FactBundle{State: "open", UpdatedAt: "2026-04-11T12:00:00Z", HeadSHA: "abc123", PolicyLabelsPresent: []string{"keep", "pending"}, LastHumanCommentAt: "2026-04-10T12:00:00Z", HumanCommentCountSinceOpen: 3}
	bundleB := FactBundle{State: "open", UpdatedAt: "2026-04-11T12:00:00Z", HeadSHA: "abc123", PolicyLabelsPresent: []string{"pending", "keep"}, LastHumanCommentAt: "2026-04-10T12:00:00Z", HumanCommentCountSinceOpen: 3}

	fingerprintA, err := BuildFingerprint(bundleA)
	if err != nil {
		t.Fatalf("BuildFingerprint(bundleA) error = %v", err)
	}
	fingerprintB, err := BuildFingerprint(bundleB)
	if err != nil {
		t.Fatalf("BuildFingerprint(bundleB) error = %v", err)
	}

	if fingerprintA != fingerprintB {
		t.Fatalf("BuildFingerprint() mismatch for reordered labels: %q != %q", fingerprintA, fingerprintB)
	}

	var record FingerprintRecord
	if err := json.Unmarshal([]byte(fingerprintA), &record); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got := strings.Join(record.Inputs.PolicyLabelsPresent, ","); got != "keep,pending" {
		t.Fatalf("sorted policy labels = %q, want keep,pending", got)
	}
}

func TestPolicyLabelsPresentIncludesOnlyConfiguredRelevantLabels(t *testing.T) {
	t.Parallel()

	roleCfg := config.SweeperRoleConfig{
		Lifecycle: config.SweeperLifecycleConfig{PendingLabel: "pending", ClosedLabel: "closed", KeepLabel: "keep"},
		Security:  config.SweeperSecurityConfig{QuarantineLabel: "quarantine"},
		Triggers:  config.SweeperTriggersConfig{ExcludeLabels: []string{"skip-sweeper"}, LooperInternalLabels: []string{"looper/internal"}},
	}

	got := PolicyLabelsPresent([]string{"docs", " keep ", "unknown", "SKIP-SWEEPER", "LoOpEr/InTeRnAl", "pending", "closed"}, roleCfg)
	if want := "closed,keep,looper/internal,pending,skip-sweeper"; strings.Join(got, ",") != want {
		t.Fatalf("PolicyLabelsPresent() = %v, want %s", got, want)
	}
}

func TestTruncateFactBodyAppendsMarkerWhenTrimmed(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("a", phase1BodyCapBytes) + " trailing overflow"
	got, truncated := TruncateFactBody(body)
	if !truncated {
		t.Fatal("TruncateFactBody() truncated = false, want true")
	}
	if !strings.HasSuffix(got, "\n... [truncated]") {
		t.Fatalf("TruncateFactBody() suffix missing: %q", got)
	}
	if len(got) <= phase1BodyCapBytes {
		t.Fatalf("TruncateFactBody() len = %d, want marker-appended output", len(got))
	}
}

func TestDeriveHumanCommentStatsExcludesBotsAndCountsReviewThreadComments(t *testing.T) {
	t.Parallel()

	issueComments := []githubinfra.CommentInfo{
		{Author: "human-1", AuthorAssociation: "CONTRIBUTOR", CreatedAt: "2026-04-11T12:00:00Z"},
		{Author: "bot-user", AuthorAssociation: "BOT", CreatedAt: "2026-04-11T12:30:00Z"},
		{Author: "excluded-user", AuthorAssociation: "MEMBER", CreatedAt: "2026-04-11T12:45:00Z"},
		{Author: "looper-bot", AuthorAssociation: "MEMBER", CreatedAt: "2026-04-11T13:00:00Z"},
	}
	reviewThreads := []githubinfra.ReviewThread{
		{Comments: []githubinfra.ReviewThreadComment{
			{Author: "human-2", AuthorAssociation: "MEMBER", CreatedAt: "2026-04-11T13:30:00Z"},
			{Author: "excluded-user", AuthorAssociation: "MEMBER", CreatedAt: "2026-04-11T13:45:00Z"},
			{Author: "thread-bot", AuthorAssociation: "BOT", CreatedAt: "2026-04-11T14:00:00Z"},
		}},
	}

	latest, count := DeriveHumanCommentStats(issueComments, reviewThreads, []string{"excluded-user"}, "looper-bot")
	if latest != "2026-04-11T13:30:00Z" || count != 2 {
		t.Fatalf("DeriveHumanCommentStats() = (%q, %d), want (2026-04-11T13:30:00Z, 2)", latest, count)
	}
}

func TestNewMarkerUUIDReturnsNonEmptySweeperScopedIdentifier(t *testing.T) {
	t.Parallel()

	got := NewMarkerUUID()
	if got == "" {
		t.Fatal("NewMarkerUUID() = empty, want non-empty")
	}
	if !strings.HasPrefix(got, "sweeper_") {
		t.Fatalf("NewMarkerUUID() = %q, want sweeper_ prefix", got)
	}
	if len(got) <= len("sweeper_") {
		t.Fatalf("NewMarkerUUID() = %q, want suffix content", got)
	}
}

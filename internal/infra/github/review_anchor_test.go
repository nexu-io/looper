package github

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/diffanchor"
)

func TestNormalizeReviewAnchorsPreservesValidAndDowngradesInvalid(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n")
	body, comments, flags, _ := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Valid inline", Path: "app.go", Line: 1, Side: "RIGHT"},
		{Body: "Invalid inline", Path: "app.go", Line: 99, Side: "RIGHT"},
	}, &idx)

	if len(comments) != 1 || comments[0].Body != "Valid inline" || comments[0].Line != 1 || comments[0].Side != "RIGHT" {
		t.Fatalf("valid anchor was not preserved exactly: %#v", comments)
	}
	if !strings.Contains(body, "Invalid inline") || !strings.Contains(body, "Location: app.go RIGHT line 99") || !strings.Contains(body, "Inline comment could not be anchored") {
		t.Fatalf("invalid anchor was not downgraded with fallback location:\n%s", body)
	}
	if strings.Index(body, "Location: app.go RIGHT line 99") > strings.Index(body, "Invalid inline") {
		t.Fatalf("downgraded feedback should start with fallback location:\n%s", body)
	}
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
}

func TestNormalizeReviewAnchorsMovesNearbyOutOfRangeAnchorToNearestHunk(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -10,2 +10,2 @@\n old\n+new\n")
	body, comments, flags, _ := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Nearby issue", Path: "app.go", Line: 13, Side: "RIGHT"},
	}, &idx)

	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
	if body != "Needs changes" {
		t.Fatalf("body = %q, want unchanged top-level body", body)
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want one nearest-anchored comment", comments)
	}
	if comments[0].Path != "app.go" || comments[0].Line != 11 || comments[0].Side != "RIGHT" {
		t.Fatalf("comment anchor = %#v, want app.go RIGHT line 11", comments[0])
	}
	if !strings.Contains(comments[0].Body, "Original requested location: app.go RIGHT line 13") || !strings.Contains(comments[0].Body, "Nearby issue") {
		t.Fatalf("comment body did not preserve original location:\n%s", comments[0].Body)
	}
}

func TestNormalizeReviewAnchorsFallsBackForFarOutOfRangeAnchor(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -10,2 +10,2 @@\n old\n+new\n")
	body, comments, flags, _ := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Far issue", Path: "app.go", Line: 99, Side: "RIGHT"},
	}, &idx)

	if len(comments) != 0 {
		t.Fatalf("comments = %#v, want unanchorable comment converted to body", comments)
	}
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
	if !strings.Contains(body, "Location: app.go RIGHT line 99") || !strings.Contains(body, "Far issue") || !strings.Contains(body, "Inline comment could not be anchored") {
		t.Fatalf("body did not preserve far out-of-range feedback with context:\n%s", body)
	}
	if strings.Contains(body, "Downgraded from inline review comment") {
		t.Fatalf("body contains noisy downgrade wording:\n%s", body)
	}
}

func TestNormalizeReviewAnchorsDoesNotMoveWrongSideContextAnchor(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -10,3 +10,3 @@\n context\n-old\n+new\n tail\n")
	body, comments, flags, _ := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Context issue", Path: "app.go", Line: 10, Side: "LEFT"},
	}, &idx)

	if len(comments) != 0 {
		t.Fatalf("comments = %#v, want wrong-side context anchor converted to body", comments)
	}
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
	if !strings.Contains(body, "Location: app.go LEFT line 10") || !strings.Contains(body, "Context issue") {
		t.Fatalf("body did not preserve wrong-side context feedback with context:\n%s", body)
	}
}

func TestNormalizeReviewAnchorsDoesNotMoveAmbiguousNearestAnchor(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -10,1 +10,1 @@\n-a\n+b\n@@ -14,1 +14,1 @@\n-c\n+d\n")
	body, comments, flags, _ := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Between hunks", Path: "app.go", Line: 12, Side: "RIGHT"},
	}, &idx)

	if len(comments) != 0 {
		t.Fatalf("comments = %#v, want ambiguous nearest anchor converted to body", comments)
	}
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
	if !strings.Contains(body, "Location: app.go RIGHT line 12") || !strings.Contains(body, "Between hunks") {
		t.Fatalf("body did not preserve ambiguous nearest feedback with context:\n%s", body)
	}
}

func TestNormalizeReviewAnchorsCanonicalizesValidSides(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n")
	_, comments, flags, _ := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Valid inline", Path: "app.go", StartLine: 1, StartSide: "right", Line: 2, Side: "right"},
	}, &idx)

	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want one preserved valid comment", comments)
	}
	if comments[0].Side != "RIGHT" || comments[0].StartSide != "RIGHT" {
		t.Fatalf("comment sides = %q/%q, want canonical RIGHT/RIGHT", comments[0].Side, comments[0].StartSide)
	}
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
}

func TestNormalizeReviewAnchorsPreservesValidPathSpaces(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/ leading.go b/ leading.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n")
	_, comments, flags, _ := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Valid inline", Path: " leading.go", Line: 1, Side: "RIGHT"},
	}, &idx)

	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want one preserved valid comment", comments)
	}
	if comments[0].Path != " leading.go" {
		t.Fatalf("comment path = %q, want preserved leading-space path", comments[0].Path)
	}
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
}

func TestNormalizeReviewAnchorsFlagsUnlocatedTopLevelComment(t *testing.T) {
	t.Parallel()
	_, _, flags, _ := normalizeReviewAnchors("This is vague and needs work.", nil, nil)
	if len(flags) != 1 || flags[0].Kind != "top-level-location-missing" {
		t.Fatalf("flags = %#v, want top-level-location-missing", flags)
	}
}

func TestNormalizeReviewAnchorsDoesNotFlagEmptyTopLevelBody(t *testing.T) {
	t.Parallel()
	_, _, flags, _ := normalizeReviewAnchors("", nil, nil)
	if len(flags) != 0 {
		t.Fatalf("flags = %#v, want none for empty top-level body", flags)
	}
}

func TestReviewQualityGateRequiresActualCleanMarker(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"<!-- looper:review id=abc head=def outcome=clean -->\n<!-- looper:review id=abc head=def outcome=clean -->",
		"<!-- looper:review id=abc head=def -->",
	} {
		applies, err := reviewQualityGateApplies("APPROVE", body)
		if err == nil {
			t.Fatalf("reviewQualityGateApplies() error = nil for %q, want marker validation error", body)
		}
		if !applies {
			t.Fatalf("reviewQualityGateApplies() = false for %q, want true without exactly one well-formed clean marker", body)
		}
	}
	applies, err := reviewQualityGateApplies("APPROVE", "This prose mentions outcome=clean but has no marker.")
	if err != nil || !applies {
		t.Fatalf("reviewQualityGateApplies(prose outcome) = %v, %v; want true, nil", applies, err)
	}
}

func TestReviewQualityGateRejectsEventOutcomeMismatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		event string
		body  string
	}{
		{event: "APPROVE", body: "<!-- looper:review id=abc head=def outcome=actionable -->"},
		{event: "REQUEST_CHANGES", body: "<!-- looper:review id=abc head=def outcome=clean -->"},
	} {
		if _, err := reviewQualityGateApplies(tc.event, tc.body); err == nil {
			t.Fatalf("reviewQualityGateApplies(%q, %q) error = nil, want mismatch error", tc.event, tc.body)
		}
	}
}

func TestReviewQualityGateUsesMarkerOutcome(t *testing.T) {
	t.Parallel()
	applies, err := reviewQualityGateApplies("COMMENT", "Top-level clean review.\n<!-- looper:review id=abc head=def outcome=clean -->")
	if err != nil || applies {
		t.Fatalf("reviewQualityGateApplies(clean COMMENT) = %v, %v; want false, nil", applies, err)
	}
	applies, err = reviewQualityGateApplies("COMMENT", "Top-level actionable review.\n<!-- looper:review id=abc head=def outcome=actionable -->")
	if err != nil || !applies {
		t.Fatalf("reviewQualityGateApplies(actionable COMMENT) = %v, %v; want true, nil", applies, err)
	}
}

func TestReviewQualityGateRejectsExtraMalformedMarker(t *testing.T) {
	t.Parallel()
	body := "<!-- looper:review id=abc head=def outcome=clean -->\n<!-- looper:review id=abc head=def -->"
	if _, err := reviewQualityGateApplies("APPROVE", body); err == nil {
		t.Fatalf("reviewQualityGateApplies() error = nil, want extra malformed marker rejected")
	}
}

func TestNormalizeReviewAnchorsClearsSingleLineStartRange(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n")
	_, comments, flags, _ := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Valid inline", Path: "app.go", StartLine: 1, StartSide: "RIGHT", Line: 1, Side: "RIGHT"},
	}, &idx)
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want one preserved valid comment", comments)
	}
	if comments[0].StartLine != 0 || comments[0].StartSide != "" {
		t.Fatalf("single-line range = start_line %d start_side %q, want cleared", comments[0].StartLine, comments[0].StartSide)
	}
}

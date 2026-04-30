package github

import (
	"strings"
	"testing"

	"github.com/powerformer/looper/internal/diffanchor"
)

func TestNormalizeReviewAnchorsPreservesValidAndDowngradesInvalid(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n")
	body, comments, flags := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Valid inline", Path: "app.go", Line: 1, Side: "RIGHT"},
		{Body: "Invalid inline", Path: "app.go", Line: 99, Side: "RIGHT"},
	}, &idx)

	if len(comments) != 1 || comments[0].Body != "Valid inline" || comments[0].Line != 1 || comments[0].Side != "RIGHT" {
		t.Fatalf("valid anchor was not preserved exactly: %#v", comments)
	}
	if !strings.Contains(body, "Invalid inline") || !strings.Contains(body, "Location: app.go RIGHT line 99") || !strings.Contains(body, "Downgraded from inline review comment") {
		t.Fatalf("invalid anchor was not downgraded with fallback location:\n%s", body)
	}
	if strings.Index(body, "Location: app.go RIGHT line 99") > strings.Index(body, "Invalid inline") {
		t.Fatalf("downgraded feedback should start with fallback location:\n%s", body)
	}
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
}

func TestNormalizeReviewAnchorsCanonicalizesValidSides(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n")
	_, comments, flags := normalizeReviewAnchors("Needs changes", []ReviewComment{
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

func TestNormalizeReviewAnchorsTrimsValidPath(t *testing.T) {
	t.Parallel()
	idx := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n")
	_, comments, flags := normalizeReviewAnchors("Needs changes", []ReviewComment{
		{Body: "Valid inline", Path: " app.go \t", Line: 1, Side: "RIGHT"},
	}, &idx)

	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want one preserved valid comment", comments)
	}
	if comments[0].Path != "app.go" {
		t.Fatalf("comment path = %q, want trimmed app.go", comments[0].Path)
	}
	if len(flags) != 0 {
		t.Fatalf("unexpected quality flags: %#v", flags)
	}
}

func TestNormalizeReviewAnchorsFlagsUnlocatedTopLevelComment(t *testing.T) {
	t.Parallel()
	_, _, flags := normalizeReviewAnchors("This is vague and needs work.", nil, nil)
	if len(flags) != 1 || flags[0].Kind != "top-level-location-missing" {
		t.Fatalf("flags = %#v, want top-level-location-missing", flags)
	}
}

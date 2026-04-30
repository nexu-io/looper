package diffanchor

import (
	"strings"
	"testing"
)

func TestParseSingleFileDiffAndValidateAnchors(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/app.go b/app.go\n@@ -10,3 +10,4 @@ func run() {\n context\n-old()\n+new()\n+more()\n tail\n"
	idx := Parse(diff)

	if !idx.Validate(Anchor{Path: "app.go", Line: 12, Side: SideRight}).Valid {
		t.Fatalf("RIGHT anchor on added line should be valid: %#v", idx.Ranges)
	}
	if !idx.Validate(Anchor{Path: "app.go", Line: 11, Side: SideLeft}).Valid {
		t.Fatalf("LEFT anchor on removed line should be valid: %#v", idx.Ranges)
	}
	if got := idx.Validate(Anchor{Path: "app.go", Line: 99, Side: SideRight}); got.Valid || !strings.Contains(got.LocationText, "app.go RIGHT line 99") {
		t.Fatalf("out-of-range validation = %#v, want invalid with fallback location", got)
	}
}

func TestParseMultiHunkDiffSeparatesRanges(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/app.go b/app.go\n@@ -1,2 +1,2 @@\n-one\n+ONE\n two\n@@ -20,2 +20,2 @@\n-old\n+new\n keep\n"
	idx := Parse(diff)

	if !idx.Validate(Anchor{Path: "app.go", Line: 1, Side: SideRight}).Valid {
		t.Fatal("first hunk anchor should be valid")
	}
	if !idx.Validate(Anchor{Path: "app.go", Line: 20, Side: SideRight}).Valid {
		t.Fatal("second hunk anchor should be valid")
	}
	if got := idx.Validate(Anchor{Path: "app.go", StartLine: 1, Line: 20, Side: SideRight, StartSide: SideRight}); got.Valid {
		t.Fatalf("multiline anchor spanning hunks should be invalid")
	}
}

func TestParseDeletedFileWithSpacesUsesDiffGitPath(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/a b.txt b/a b.txt\ndeleted file mode 100644\n--- a/a b.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-old\n-line\n"
	idx := Parse(diff)

	if !idx.Validate(Anchor{Path: "a b.txt", Line: 1, Side: SideLeft}).Valid {
		t.Fatalf("LEFT anchor on deleted file with spaces should be valid: %#v", idx.Ranges)
	}
	if got := idx.Validate(Anchor{Path: "b.txt", Line: 1, Side: SideLeft}); got.Valid {
		t.Fatalf("truncated path anchor should be invalid: %#v", got)
	}
}

func TestParseDeletedFilePathContainingBSlash(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/foo b/bar.txt b/foo b/bar.txt\ndeleted file mode 100644\n--- a/foo b/bar.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-old\n-line\n"
	idx := Parse(diff)

	if !idx.Validate(Anchor{Path: "foo b/bar.txt", Line: 1, Side: SideLeft}).Valid {
		t.Fatalf("LEFT anchor on deleted file containing b/ should be valid: %#v", idx.Ranges)
	}
	if got := idx.Validate(Anchor{Path: "bar.txt", Line: 1, Side: SideLeft}); got.Valid {
		t.Fatalf("truncated path anchor should be invalid: %#v", got)
	}
}

func TestParseQuotedGitPathsUnescapesHeaders(t *testing.T) {
	t.Parallel()
	path := "a\tb.txt"
	diff := "diff --git \"a/a\\tb.txt\" \"b/a\\tb.txt\"\n--- \"a/a\\tb.txt\"\n+++ \"b/a\\tb.txt\"\n@@ -1,1 +1,1 @@\n-old\n+new\n"
	idx := Parse(diff)

	if !idx.Validate(Anchor{Path: path, Line: 1, Side: SideRight}).Valid {
		t.Fatalf("RIGHT anchor on quoted path should be valid: %#v", idx.Ranges)
	}
	if !idx.Validate(Anchor{Path: path, Line: 1, Side: SideLeft}).Valid {
		t.Fatalf("LEFT anchor on quoted path should be valid: %#v", idx.Ranges)
	}
	if got := idx.Validate(Anchor{Path: `"b/a\tb.txt"`, Line: 1, Side: SideRight}); got.Valid {
		t.Fatalf("still-quoted path anchor should be invalid: %#v", got)
	}
}

func TestValidateMultilineMarkdownAnchorAcrossHeadingChange(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/docs/spec.md b/docs/spec.md\n@@ -1,4 +1,4 @@\n # Old heading\n-content\n+# New heading\n+content\n tail\n"
	idx := Parse(diff)

	if got := idx.Validate(Anchor{Path: "docs/spec.md", StartLine: 1, Line: 2, Side: SideRight, StartSide: SideRight}); !got.Valid {
		t.Fatalf("contiguous RIGHT multiline anchor across heading change should be valid: %#v ranges=%#v", got, idx.Ranges)
	}
}

func TestParseMarkdownHeadingContext(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/docs/spec.md b/docs/spec.md\n@@ -4,3 +4,4 @@\n # Reviewer anchors\n existing\n+new requirement\n tail\n"
	idx := Parse(diff)
	section := idx.FormatPromptSection(10)
	if !strings.Contains(section, "heading: # Reviewer anchors") {
		t.Fatalf("prompt section missing heading context:\n%s", section)
	}
}

func TestValidateTopLevelLocationFlagsMissingContext(t *testing.T) {
	t.Parallel()
	if got := ValidateTopLevelLocation("This has concerns and should be improved."); !got.QualityFlagged {
		t.Fatalf("expected missing location to be quality flagged: %#v", got)
	}
	if got := ValidateTopLevelLocation("docs/spec.md section Reviewer anchors needs a validation example."); !got.Valid {
		t.Fatalf("expected exact location context to pass: %#v", got)
	}
}

package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/diffanchor"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestBuildReviewAnchorIndexUsesLocalPathDiffWhenRemoteIsTruncated(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, targetLine := seedLargePRRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})

	remoteCalls := 0
	index, source, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     repo,
		BaseSHA: baseSHA,
		HeadSHA: headSHA,
		Paths:   []string{"target/late.go"},
		RemoteDiff: func(context.Context) (string, error) {
			remoteCalls++
			return "", ErrLocalCaptureTruncated
		},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}
	if source != ReviewAnchorAuthorityLocalPathDiff {
		t.Fatalf("source = %q, want %q", source, ReviewAnchorAuthorityLocalPathDiff)
	}
	if remoteCalls != 0 {
		t.Fatalf("remote fallback calls = %d, want 0 when local path authority succeeds", remoteCalls)
	}
	if index == nil || !index.Validate(diffanchor.Anchor{Path: "target/late.go", Line: targetLine, Side: diffanchor.SideRight}).Valid {
		t.Fatalf("index did not validate target RIGHT line %d: %#v", targetLine, index)
	}
}

func TestBuildReviewAnchorIndexValidatesLeftDeletedLine(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, deletedLine := seedDeletedLineRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})

	index, source, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     repo,
		BaseSHA: baseSHA,
		HeadSHA: headSHA,
		Paths:   []string{"removed.go"},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}
	if source != ReviewAnchorAuthorityLocalPathDiff {
		t.Fatalf("source = %q, want local_path_diff", source)
	}
	if index == nil || !index.Validate(diffanchor.Anchor{Path: "removed.go", Line: deletedLine, Side: diffanchor.SideLeft}).Valid {
		t.Fatalf("LEFT deleted line %d should be valid: %#v", deletedLine, index)
	}
}

func TestBuildReviewAnchorIndexRejectsLocalBaseHeadMismatch(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, _ := seedLargePRRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})

	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     repo,
		BaseSHA: baseSHA,
		HeadSHA: strings.Repeat("0", 40),
		Paths:   []string{"target/late.go"},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) && !errors.Is(err, ErrReviewBaseHeadMismatch) {
		t.Fatalf("error = %v, want base/head mismatch wrapped as unavailable", err)
	}
	_ = headSHA
}

func TestBuildReviewAnchorIndexFallsBackToCompleteRemoteDiff(t *testing.T) {
	t.Parallel()

	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(context.Context, shell.Options) (shell.Result, error) {
			return shell.Result{ExitCode: 1, Stderr: "not a git repository"}, &shell.CommandExecutionError{Message: "not a git repository"}
		},
	})
	remoteDiff := "diff --git a/app.go b/app.go\n@@ -1 +1 @@\n-old\n+new\n"
	index, source, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Paths:   []string{"app.go"},
		RemoteDiff: func(context.Context) (string, error) {
			return remoteDiff, nil
		},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}
	if source != ReviewAnchorAuthorityRemotePRDiff {
		t.Fatalf("source = %q, want remote_pr_diff", source)
	}
	if index == nil || !index.Validate(diffanchor.Anchor{Path: "app.go", Line: 1, Side: diffanchor.SideRight}).Valid {
		t.Fatalf("remote fallback index invalid: %#v", index)
	}
}

func TestBuildReviewAnchorIndexDoesNotParseTruncatedRemoteDiff(t *testing.T) {
	t.Parallel()

	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(context.Context, shell.Options) (shell.Result, error) {
			return shell.Result{ExitCode: 1, Stderr: "missing objects"}, &shell.CommandExecutionError{Message: "missing objects"}
		},
	})
	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Paths:   []string{"app.go"},
		RemoteDiff: func(context.Context) (string, error) {
			return strings.Repeat("x", 100), ErrLocalCaptureTruncated
		},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) {
		t.Fatalf("error = %v, want ErrAnchorValidationUnavailable", err)
	}
	if !strings.Contains(err.Error(), DiffTruncationReasonLocalCapture) {
		t.Fatalf("error = %v, want local_capture_truncated diagnostic", err)
	}
}

func TestBuildReviewAnchorIndexSurfacesGitHubOversizedSeparately(t *testing.T) {
	t.Parallel()

	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(context.Context, shell.Options) (shell.Result, error) {
			return shell.Result{ExitCode: 1, Stderr: "missing objects"}, &shell.CommandExecutionError{Message: "missing objects"}
		},
	})
	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Paths:   []string{"app.go"},
		RemoteDiff: func(context.Context) (string, error) {
			return "", ErrDiffTooLarge
		},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) {
		t.Fatalf("error = %v, want ErrAnchorValidationUnavailable", err)
	}
	if !strings.Contains(err.Error(), DiffTruncationReasonGitHubTooLarge) {
		t.Fatalf("error = %v, want github_diff_too_large diagnostic", err)
	}
}

func TestBuildReviewAnchorIndexFailsClosedWhenAllInlinePathsEmpty(t *testing.T) {
	t.Parallel()

	gateway := New(Options{GHPath: "gh", GitPath: "git"})
	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		// Inline comment slots present, but every path trims empty — not "no comments".
		Paths: []string{"", "   ", "\t"},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) {
		t.Fatalf("error = %v, want ErrAnchorValidationUnavailable for empty paths", err)
	}
	if !strings.Contains(err.Error(), "non-empty paths") {
		t.Fatalf("error = %v, want non-empty paths detail", err)
	}

	// No path slots remains success with nil authority (body-only callers).
	index, source, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("empty Paths error = %v, want nil", err)
	}
	if index != nil || source != "" {
		t.Fatalf("empty Paths = (%v, %q), want (nil, \"\")", index, source)
	}
}

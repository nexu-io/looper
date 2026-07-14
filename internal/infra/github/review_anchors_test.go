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

func TestBuildReviewAnchorIndexTreatsPathspecMagicFilenamesLiterally(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, magicPath, targetLine := seedPathspecMagicFilenameRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})

	// Remote is unavailable so local path-targeted authority must succeed with
	// literal pathspecs; without --literal-pathspecs git rejects ":(foo).txt".
	index, source, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     repo,
		BaseSHA: baseSHA,
		HeadSHA: headSHA,
		Paths:   []string{magicPath},
		RemoteDiff: func(context.Context) (string, error) {
			return "", ErrLocalCaptureTruncated
		},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v, want success for pathspec-magic filename", err)
	}
	if source != ReviewAnchorAuthorityLocalPathDiff {
		t.Fatalf("source = %q, want %q", source, ReviewAnchorAuthorityLocalPathDiff)
	}
	if index == nil || !index.Validate(diffanchor.Anchor{Path: magicPath, Line: targetLine, Side: diffanchor.SideRight}).Valid {
		t.Fatalf("index did not validate RIGHT line %d on %q: %#v", targetLine, magicPath, index)
	}
}

func TestBuildLocalPathAnchorIndexPassesLiteralPathspecs(t *testing.T) {
	t.Parallel()

	baseSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	magicPath := ":(foo).txt"
	var sawDiffArgs []string
	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			if len(options.Args) >= 1 && options.Args[0] == "rev-parse" {
				sha := baseSHA
				for _, arg := range options.Args {
					if strings.HasPrefix(arg, headSHA) {
						sha = headSHA
						break
					}
				}
				return shell.Result{Stdout: sha + "\n"}, nil
			}
			sawDiffArgs = append([]string(nil), options.Args...)
			return shell.Result{
				Stdout: "diff --git a/:(foo).txt b/:(foo).txt\n@@ -1 +1,2 @@\n line1\n+line2\n",
			}, nil
		},
	})
	_, err := gateway.buildLocalPathAnchorIndex(context.Background(), t.TempDir(), baseSHA, headSHA, []string{magicPath})
	if err != nil {
		t.Fatalf("buildLocalPathAnchorIndex() error = %v", err)
	}
	if len(sawDiffArgs) == 0 || sawDiffArgs[0] != "--literal-pathspecs" {
		t.Fatalf("diff argv = %v, want leading --literal-pathspecs", sawDiffArgs)
	}
	if !strings.Contains(strings.Join(sawDiffArgs, "\x00"), magicPath) {
		t.Fatalf("diff argv = %v, want magic path %q", sawDiffArgs, magicPath)
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

func TestBuildReviewAnchorIndexRedactsPathspecsFromReturnedErrors(t *testing.T) {
	t.Parallel()

	secretPath := ":(SERVICE_TOKEN=secret-value-should-not-leak)"
	baseSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			// Succeed object verification so failure occurs on path-targeted diff argv.
			if len(options.Args) >= 1 && options.Args[0] == "rev-parse" {
				sha := baseSHA
				for _, arg := range options.Args {
					if strings.HasPrefix(arg, headSHA) {
						sha = headSHA
						break
					}
				}
				return shell.Result{Stdout: sha + "\n"}, nil
			}
			// Simulate git rejecting a secret-shaped pathspec.
			return shell.Result{ExitCode: 128, Stderr: "fatal: invalid pathspec"}, &shell.CommandExecutionError{
				Message: "fatal: invalid pathspec",
				Result:  shell.Result{ExitCode: 128, Stderr: "fatal: invalid pathspec"},
			}
		},
	})
	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: baseSHA,
		HeadSHA: headSHA,
		Paths:   []string{secretPath},
		RemoteDiff: func(context.Context) (string, error) {
			return "", ErrDiffTooLarge
		},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) {
		t.Fatalf("error = %v, want ErrAnchorValidationUnavailable", err)
	}
	if strings.Contains(err.Error(), secretPath) || strings.Contains(err.Error(), "SERVICE_TOKEN") {
		t.Fatalf("error leaked secret-shaped pathspec: %v", err)
	}
	if !strings.Contains(err.Error(), DiffTruncationReasonGitHubTooLarge) {
		t.Fatalf("error = %v, want github_diff_too_large reason", err)
	}
	if !strings.Contains(err.Error(), "local_path_diff_failed") {
		t.Fatalf("error = %v, want sanitized local_path_diff_failed reason", err)
	}
}

func TestReviewAnchorGitCommandSummaryOmitsPathspecs(t *testing.T) {
	t.Parallel()

	summary := reviewAnchorGitCommandSummary([]string{
		"diff", "--no-ext-diff", "--no-color", "aaa...bbb", "--", ":(SERVICE_TOKEN=secret)",
	})
	if strings.Contains(summary, "SERVICE_TOKEN") || strings.Contains(summary, ":(") {
		t.Fatalf("summary leaked pathspec: %q", summary)
	}
	if !strings.Contains(summary, "<1 paths>") {
		t.Fatalf("summary = %q, want path count placeholder", summary)
	}
	if got := reviewAnchorGitCommandSummary([]string{"rev-parse", "--verify", "abc^{commit}"}); got != "rev-parse --verify abc^{commit}" {
		t.Fatalf("rev-parse summary = %q", got)
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

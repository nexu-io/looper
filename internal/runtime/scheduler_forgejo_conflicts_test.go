package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/loops/failureclass"
	"github.com/nexu-io/looper/internal/reviewer"
)

func TestForgejoConflictProcessLaunchFailuresPreserveRetryPolicy(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"inspect commits for merge conflict check", "check merge conflicts"} {
		for _, tc := range []struct {
			name     string
			errno    syscall.Errno
			boundary failureclass.Boundary
			kind     failureclass.Kind
		}{
			{"EAGAIN", syscall.EAGAIN, failureclass.BoundaryGitRemote, failureclass.RetryableTransient},
			{"ENOMEM", syscall.ENOMEM, failureclass.BoundaryGitRemote, failureclass.RetryableTransient},
			{"EMFILE", syscall.EMFILE, failureclass.BoundaryGitRemote, failureclass.RetryableTransient},
			{"ENFILE", syscall.ENFILE, failureclass.BoundaryGitRemote, failureclass.RetryableTransient},
			{"ETXTBSY", syscall.ETXTBSY, failureclass.BoundaryGitRemote, failureclass.RetryableTransient},
			{"ENOENT", syscall.ENOENT, failureclass.BoundaryGitLocal, failureclass.NonRetryable},
			{"EACCES", syscall.EACCES, failureclass.BoundaryGitLocal, failureclass.NonRetryable},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				// Match shell.Run's wrapped start error, retaining the underlying
				// errno through the cat-file / merge-tree operation context.
				err := withForgejoConflictBoundary(fmt.Errorf("%s: %w", operation, fmt.Errorf("start command: %w", &os.PathError{Op: "fork/exec", Path: "/configured/git", Err: tc.errno})))
				var boundaryErr *failureclass.BoundaryError
				if !errors.As(err, &boundaryErr) || boundaryErr.Boundary != tc.boundary {
					t.Fatalf("boundary = %v; want %s", err, tc.boundary)
				}
				for _, runner := range []failureclass.RunnerKind{failureclass.RunnerReviewer, failureclass.RunnerFixer} {
					if got := failureclass.Classify(err, failureclass.Context{Runner: runner, Boundary: failureclass.BoundaryGitHubAPI}); got != tc.kind {
						t.Fatalf("%s launch classification = %s, want %s", runner, got, tc.kind)
					}
				}
			})
		}
	}
}

func TestForgejoConflictAdapterFailuresKeepOperationBoundary(t *testing.T) {
	t.Setenv("FORGEJO_CONFLICT_TOKEN", "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/42"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "mergeable": false, "head": map[string]any{"sha": strings.Repeat("a", 40)}, "base": map[string]any{"sha": strings.Repeat("b", 40)}})
		case strings.HasSuffix(r.URL.Path, ".diff"):
			_, _ = w.Write([]byte("diff --git a/app.go b/app.go\n"))
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, script string
		boundary     failureclass.Boundary
		kind         failureclass.Kind
	}{
		{"remote fetch closes", "#!/bin/sh\ncase \"$1\" in\ncat-file) while read object; do echo \"$object missing\"; done;;\nfetch) echo 'Connection closed by 172.16.1.8 port 22' >&2; exit 128;;\n*) exit 129;;\nesac\n", failureclass.BoundaryGitRemote, failureclass.RetryableTransient},
		{"missing configured Git", "", failureclass.BoundaryGitLocal, failureclass.NonRetryable},
		{"unsupported merge-tree", "#!/bin/sh\ncase \"$1\" in\ncat-file) while read object; do echo \"$object commit\"; done;;\nmerge-tree) echo 'unknown option write-tree' >&2; exit 129;;\n*) exit 129;;\nesac\n", failureclass.BoundaryGitLocal, failureclass.NonRetryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitPath := filepath.Join(t.TempDir(), "missing-git")
			if tc.script != "" {
				gitPath = forgejoMergeTestWriteFakeGit(t, tc.script)
			}
			cwd := t.TempDir()
			cfg := config.Config{
				Tools:     config.ToolPathsConfig{GitPath: &gitPath},
				Providers: []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_CONFLICT_TOKEN")}},
				Projects:  []config.ProjectRefConfig{{ID: "project", Provider: "forgejo", Repo: "acme/looper", RepoPath: cwd}},
			}
			for _, runner := range []failureclass.RunnerKind{failureclass.RunnerReviewer, failureclass.RunnerFixer} {
				t.Run(string(runner), func(t *testing.T) {
					var err error
					if runner == failureclass.RunnerReviewer {
						_, err = (reviewerGitHubAdapter{config: &cfg}).ViewPullRequest(context.Background(), reviewer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd})
					} else {
						_, err = (fixerGitHubAdapter{config: &cfg}).ViewPullRequest(context.Background(), fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd})
					}
					var boundaryErr *failureclass.BoundaryError
					if !errors.As(err, &boundaryErr) || boundaryErr.Boundary != tc.boundary {
						t.Fatalf("adapter error = %v; want explicit %s boundary", err, tc.boundary)
					}
					if got := failureclass.Classify(err, failureclass.Context{Runner: runner, Step: "discover_pr", Boundary: failureclass.BoundaryGitHubAPI}); got != tc.kind {
						t.Fatalf("adapter→failureclass = %s, want %s: %v", got, tc.kind, err)
					}
				})
			}
		})
	}
}

func TestForgejoConflictChecksRespectGitConfigurationAndUnknownState(t *testing.T) {
	t.Parallel()
	cwd, base, head, _ := forgejoConflictRepo(t)
	missingGit := filepath.Join(t.TempDir(), "missing-configured-git")
	cfg := config.Config{Tools: config.ToolPathsConfig{GitPath: &missingGit}, Projects: []config.ProjectRefConfig{{Repo: "acme/looper", RepoPath: cwd}}}
	no, yes := false, true
	for _, mergeable := range []*bool{nil, &yes} {
		got, err := forgejoPullRequestHasConflicts(context.Background(), &cfg, "acme/looper", "", forge.PullRequest{State: "open", Mergeable: mergeable})
		if err != nil || got {
			t.Fatalf("unknown/true mergeability = %v, %v; must not invoke Git", got, err)
		}
	}
	pr := forge.PullRequest{State: "open", Mergeable: &no, Head: forge.BranchRef{SHA: head}, Base: forge.BranchRef{SHA: base}}
	if got, err := forgejoPullRequestHasConflicts(context.Background(), &cfg, "acme/looper", "", pr); err == nil || got || !strings.Contains(err.Error(), missingGit) {
		t.Fatalf("configured Git unavailable = %v, %v; want configured-path error, not conflict", got, err)
	}
	cfg.Tools.GitPath = nil
	if got, err := forgejoPullRequestHasConflicts(context.Background(), &cfg, "acme/looper", "", pr); err != nil || !got {
		t.Fatalf("project CWD fallback = %v, %v; want confirmed conflict", got, err)
	}
}

func TestForgejoListPullRequestsDefersExactConflictChecks(t *testing.T) {
	t.Setenv("FORGEJO_CONFLICT_TOKEN", "test-token")
	missingGit := filepath.Join(t.TempDir(), "missing-configured-git")
	requestedReviewer := map[string]any{"id": 7, "login": "reviewer"}
	draft := map[string]any{
		"number": 1, "title": "WIP", "state": "open", "draft": true, "mergeable": false,
		"head":                map[string]any{"sha": strings.Repeat("a", 40), "ref": "wip"},
		"base":                map[string]any{"sha": strings.Repeat("b", 40), "ref": "main"},
		"user":                map[string]any{"login": "alice"},
		"requested_reviewers": []map[string]any{requestedReviewer},
	}
	ready := map[string]any{
		"number": 2, "title": "Ready", "state": "open", "mergeable": true,
		"head":                map[string]any{"sha": strings.Repeat("c", 40), "ref": "ready"},
		"base":                map[string]any{"sha": strings.Repeat("b", 40), "ref": "main"},
		"user":                map[string]any{"login": "bob"},
		"requested_reviewers": []map[string]any{requestedReviewer},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}},"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]any{draft, ready})
		default:
			_ = json.NewEncoder(w).Encode([]any{})
		}
	}))
	defer server.Close()
	cwd := t.TempDir()
	cfg := config.Config{
		Tools:     config.ToolPathsConfig{GitPath: &missingGit},
		Providers: []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_CONFLICT_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project", Provider: "forgejo", Repo: "acme/looper", RepoPath: cwd}},
	}
	adapter := reviewerGitHubAdapter{config: &cfg}
	ctx := context.Background()
	assertListed := func(t *testing.T, prs []reviewer.PullRequestSummary, err error) {
		t.Helper()
		if err != nil || len(prs) != 2 || prs[0].Number != 1 || !prs[0].IsDraft || prs[1].Number != 2 || prs[0].HasConflicts || prs[1].HasConflicts {
			t.Fatalf("listed = %#v, %v; want both PRs without aborting on the mergeable=false draft", prs, err)
		}
	}
	open, err := adapter.ListOpenPullRequests(ctx, reviewer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: cwd})
	assertListed(t, open, err)
	requested, err := adapter.ListReviewRequestedPullRequests(ctx, reviewer.ListReviewRequestedPullRequestsInput{Repo: "acme/looper", CWD: cwd, Reviewer: "reviewer"})
	assertListed(t, requested, err)
}

func forgejoConflictRepo(t *testing.T) (cwd, base, conflictedHead, cleanHead string) {
	t.Helper()
	cwd = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Looper Test")
	git("config", "user.email", "test@example.com")
	git("config", "commit.gpgsign", "false")
	write("README.md", "original\n")
	git("add", ".")
	git("commit", "-m", "initial")
	initial := git("rev-parse", "HEAD")
	git("checkout", "-b", "feature")
	write("README.md", "feature\n")
	git("add", ".")
	git("commit", "-m", "feature")
	conflictedHead = git("rev-parse", "HEAD")
	git("checkout", "-b", "clean", initial)
	write("new.txt", "independent change\n")
	git("add", ".")
	git("commit", "-m", "clean feature")
	cleanHead = git("rev-parse", "HEAD")
	git("checkout", "main")
	write("README.md", "main\n")
	git("add", ".")
	git("commit", "-m", "base advances")
	base = git("rev-parse", "HEAD")
	return cwd, base, conflictedHead, cleanHead
}

package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/loops/failureclass"
)

func TestForgejoCompareCommitsUsesAncestryInsteadOfCompareCount(t *testing.T) {
	t.Setenv("FORGEJO_COMPARE_TOKEN", "test")
	cwd, mainHead, fixedHead, _ := forgejoConflictRepo(t)
	initial := strings.TrimSpace(forgejoMergeTestRunGit(t, cwd, "merge-base", mainHead, fixedHead))
	tree := strings.TrimSpace(forgejoMergeTestRunGit(t, cwd, "rev-parse", fixedHead+"^{tree}"))
	descendant := strings.TrimSpace(forgejoMergeTestRunGit(t, cwd, "commit-tree", tree, "-p", fixedHead, "-m", "unrelated follow-up"))
	apiCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		// Real Forgejo Compare schema cannot distinguish ahead from diverged.
		_, _ = w.Write([]byte(`{"commits":[{"sha":"commit"}],"files":[],"total_commits":1}`))
	}))
	defer server.Close()
	cfg := config.Config{Providers: []config.ProviderConfig{{ID: "forge", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_COMPARE_TOKEN")}}, Projects: []config.ProjectRefConfig{{ID: "p", Provider: "forge", Repo: "core/looper", RepoPath: cwd}}}
	adapter := fixerGitHubAdapter{config: &cfg}
	for _, tc := range []struct{ name, base, head, want string }{
		{"same head", fixedHead, fixedHead, "identical"},
		{"ahead", initial, fixedHead, "ahead"},
		{"unrelated later commit retains fix", fixedHead, descendant, "ahead"},
		{"rewound before fix", fixedHead, initial, "behind"},
		{"force-push removes fix", fixedHead, mainHead, "diverged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := adapter.CompareCommits(context.Background(), fixer.CompareCommitsInput{Repo: "core/looper", CWD: cwd, Base: tc.base, Head: tc.head})
			if err != nil || result.Status != tc.want {
				t.Fatalf("compare = %#v, %v; want %s", result, err, tc.want)
			}
		})
	}
	if apiCalls != 0 {
		t.Fatalf("Compare API calls = %d; total_commits is not ancestry evidence", apiCalls)
	}
}

func TestForgejoCompareCommitsFetchesWithoutChangingCallerAndRejectsShallowGuess(t *testing.T) {
	t.Parallel()
	source, mainHead, feature, _ := forgejoConflictRepo(t)
	initial := strings.TrimSpace(forgejoMergeTestRunGit(t, source, "merge-base", mainHead, feature))
	for _, shallow := range []bool{false, true} {
		root := t.TempDir()
		caller := filepath.Join(root, "caller")
		if shallow {
			forgejoMergeTestRunGit(t, root, "clone", "--depth=1", "file://"+source, caller)
		} else {
			forgejoMergeTestMustMkdirAll(t, caller)
			forgejoMergeTestRunGit(t, caller, "init", "-b", "main")
			forgejoMergeTestRunGit(t, caller, "remote", "add", "origin", source)
			forgejoMergeTestRunGit(t, caller, "fetch", "origin", initial)
			forgejoMergeTestRunGit(t, caller, "reset", "--hard", initial)
		}
		forgejoMergeTestWriteFile(t, filepath.Join(caller, "README.md"), "staged caller change\n")
		forgejoMergeTestRunGit(t, caller, "add", "README.md")
		forgejoMergeTestWriteFile(t, filepath.Join(caller, "README.md"), "unstaged caller change\n")
		forgejoMergeTestWriteFile(t, filepath.Join(caller, "untracked.txt"), "keep me\n")
		forgejoMergeTestWriteFile(t, filepath.Join(caller, ".git", "FETCH_HEAD"), "keep fetch marker\n")
		before := mergeConflictCallerSnapshot(t, caller)
		cfg := config.Config{Projects: []config.ProjectRefConfig{{Repo: "core/looper", RepoPath: caller}}}
		status, err := forgejoCompareCommits(context.Background(), &cfg, "core/looper", "", initial, mainHead)
		if shallow {
			if err == nil || status != "" || !strings.Contains(err.Error(), "shallow repository") {
				t.Fatalf("shallow result = %q, %v; must not falsely report divergence", status, err)
			}
		} else if err != nil || status != "ahead" {
			t.Fatalf("fetched ancestry = %q, %v", status, err)
		}
		if after := mergeConflictCallerSnapshot(t, caller); after != before {
			t.Fatalf("ancestry changed caller: before=%s, after=%s", before, after)
		}
		for _, sha := range []string{initial, mainHead} {
			forgejoMergeTestRunGit(t, caller, "cat-file", "-e", sha+"^{commit}")
		}
	}
}

func TestForgejoCompareCommitsPreservesLocalAndFetchFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, script string
		want         failureclass.Kind
	}{
		{"SSH connection closes", "#!/bin/sh\ncase \"$1\" in\ncat-file) while read object; do echo \"$object missing\"; done;;\nfetch) echo 'Connection closed by 172.16.1.8 port 22' >&2; exit 128;;\nesac\n", failureclass.RetryableTransient},
		{"missing configured git", "", failureclass.NonRetryable},
		{"merge-base fails", "#!/bin/sh\ncase \"$1\" in\ncat-file) while read object; do echo \"$object commit\"; done;;\n*) echo 'unsupported Git operation' >&2; exit 129;;\nesac\n", failureclass.NonRetryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitPath := filepath.Join(t.TempDir(), "missing-git")
			if tc.script != "" {
				gitPath = forgejoMergeTestWriteFakeGit(t, tc.script)
			}
			cfg := config.Config{Tools: config.ToolPathsConfig{GitPath: &gitPath}}
			status, err := forgejoCompareCommits(context.Background(), &cfg, "core/looper", t.TempDir(), strings.Repeat("a", 40), strings.Repeat("b", 40))
			var boundaryErr *failureclass.BoundaryError
			if status != "" || !errors.As(err, &boundaryErr) || failureclass.Classify(err, failureclass.Context{Runner: failureclass.RunnerFixer, Boundary: failureclass.BoundaryGitHubAPI}) != tc.want {
				t.Fatalf("ancestry failure = %q, %v; want %s", status, err, tc.want)
			}
		})
	}
}

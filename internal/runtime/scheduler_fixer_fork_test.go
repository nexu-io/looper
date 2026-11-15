package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/fixer"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestFixerAdapterPreservesCrossRepositoryAuthority(t *testing.T) {
	t.Parallel()
	for _, fork := range []bool{false, true} {
		t.Run(fmt.Sprint(fork), func(t *testing.T) {
			gateway := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
				args := strings.Join(options.Args, " ")
				switch {
				case args == "api user --jq .login":
					return shell.Result{Stdout: "looper\n"}, nil
				case strings.HasPrefix(args, "pr view 42 "):
					if !strings.Contains(args, "isCrossRepository") {
						t.Fatal("fixer did not request head/base repository identity")
					}
					return shell.Result{Stdout: fmt.Sprintf(`{"number":42,"state":"OPEN","headRefOid":"head-42","headRefName":"feature","isCrossRepository":%t}`, fork)}, nil
				case strings.HasPrefix(args, "api graphql "):
					return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}},"comments":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`}, nil
				case strings.HasPrefix(args, "api --paginate repos/acme/looper/issues/42/comments "):
					return shell.Result{}, nil
				default:
					t.Fatalf("unexpected gh call: %s", args)
					return shell.Result{}, nil
				}
			}})
			detail, err := (fixerGitHubAdapter{gateway: gateway}).ViewPullRequest(context.Background(), fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: t.TempDir()})
			if err != nil || detail.IsCrossRepository != fork {
				t.Fatalf("detail = %#v, err = %v, want IsCrossRepository=%t", detail, err, fork)
			}
		})
	}
}

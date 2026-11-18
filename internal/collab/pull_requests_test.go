package collab

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
)

type collabTeaRunner func(context.Context, string, []string, string, time.Duration) (shell.Result, error)

func (f collabTeaRunner) Run(ctx context.Context, path string, args []string, stdin string, timeout time.Duration) (shell.Result, error) {
	return f(ctx, path, args, stdin, timeout)
}

func TestCollabForgejoUsesURLAndTeaLoginWithoutProjectBinding(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tea"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	logins := `[{"name":"unrelated-default","url":"https://other.example","default":true},{"name":"company","url":"https://code.example/team","default":false}]`
	o := pullRequestObserver{cfg: &config.Config{}, teaRunner: collabTeaRunner(func(_ context.Context, _ string, args []string, _ string, _ time.Duration) (shell.Result, error) {
		if strings.Join(args, " ") != "logins list -o json" {
			t.Fatalf("discovery attempted credentials/API operation: %v", args)
		}
		return shell.Result{Stdout: logins}, nil
	})}
	target := func(link string) (config.ProviderConfig, error) {
		// Exercise view's provider selection separately from network reads.
		u, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		return o.forgejoProvider(context.Background(), u)
	}
	provider, err := target("https://code.example/team/core/app/pulls/12")
	if err != nil || provider.TeaLogin == nil || *provider.TeaLogin != "company" {
		t.Fatalf("provider=%+v %v", provider, err)
	}
	if _, err = target("https://code.example.evil/team/core/app/pulls/12"); err == nil {
		t.Fatal("matched a different host")
	}
	if _, err = target("https://code.example/team-other/core/app/pulls/12"); err == nil {
		t.Fatal("matched a different base path")
	}
	logins = `[{"name":"personal","url":"https://code.example/team"},{"name":"bot","url":"https://code.example/team","default":true}]`
	if _, err = target("https://code.example/team/core/app/pulls/12"); err == nil {
		t.Fatal("silently chose an ambiguous identity")
	}
	o.cfg.Providers = []config.ProviderConfig{provider}
	if _, err = target("https://code.example/team/core/another/pulls/34"); err != nil {
		t.Fatal(err)
	}
}

func TestPullRequestSignalKeepsExternalFeedbackAndIgnoresOwnProgress(t *testing.T) {
	pr := githubinfra.PullRequestDetail{State: "OPEN", HeadSHA: "head", Author: "author", Comments: []map[string]any{{"id": "inline1", "body": "fix fallback", "author": "reviewer"}}}
	before := pullRequestSignal(pr)
	pr.UpdatedAt = "later"
	pr.CommentCount = 7
	pr.IssueComments = []githubinfra.CommentInfo{{ID: 1, Author: "author", Body: "fixed"}}
	pr.Comments = append(pr.Comments, map[string]any{"id": "reply1", "body": "fixed", "author": "author"})
	if got := pullRequestSignal(pr); got != before {
		t.Fatal("own progress changed the wake signal")
	}
	pr.Reviews = []map[string]any{{"id": 42, "state": "COMMENTED", "author": map[string]any{"login": "author"}, "body": "fixed"}}
	if got := pullRequestSignal(pr); got != before {
		t.Fatal("own progress review changed the wake signal")
	}
	pr.Checks = []map[string]any{{"id": 1, "state": "SUCCESS", "name": "external"}}
	checkSignal := pullRequestSignal(pr)
	pr.Checks[0]["id"] = 2
	if got := pullRequestSignal(pr); got != checkSignal {
		t.Fatal("re-published identical status changed the wake signal")
	}
	pr.Checks = nil

	pr.Comments = append(pr.Comments, map[string]any{"id": "reply2", "body": "still broken", "author": "reviewer"})
	if got := pullRequestSignal(pr); got == before {
		t.Fatal("external inline reply did not change the wake signal")
	}
}

// Opt-in read against the exact native provider and installed tea login. No
// comments, review requests, writes or user-identity credentials are required.
func TestCollabForgejoLiveObservation(t *testing.T) {
	link := os.Getenv("LOOPER_E2E_COLLAB_FORGEJO_PR")
	if link == "" {
		t.Skip("set LOOPER_E2E_COLLAB_FORGEJO_PR for a read-only native snapshot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	o := pullRequestObserver{cfg: &config.Config{}}
	pr, err := o.view(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number <= 0 || pr.State == "" || pr.HeadSHA == "" {
		t.Fatalf("incomplete PR metadata: %d %s", pr.Number, pr.State)
	}
	raw, _ := json.Marshal(pr)
	if want := os.Getenv("LOOPER_E2E_COLLAB_FORGEJO_REVIEW_TEXT"); want != "" && !strings.Contains(string(raw), want) {
		t.Fatalf("snapshot missing expected native feedback %q", want)
	}
	t.Logf("native PR=%d state=%s head=%s reviews=%d issueComments=%d checks=%d signal=%s", pr.Number, pr.State, pr.HeadSHA, len(pr.Reviews), len(pr.IssueComments), len(pr.Checks), pullRequestSignal(pr))
}

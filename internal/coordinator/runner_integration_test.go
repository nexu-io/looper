package coordinator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/e2e/harness"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

func TestCoordinatorHappyPathWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{"issue list": {"number", "title", "body", "url", "state", "updatedAt", "author", "assignees", "labels"}}})
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{
		Commands: map[string]any{"label create": map[string]any{"stdout": json.RawMessage(`{}`)}},
		Routes: map[string]any{
			"repos/acme/looper/issues/1":          json.RawMessage(`{"number":1,"title":"Coordinator bug","body":"triage me","html_url":"https://example.test/issues/1","state":"open","created_at":"2026-05-14T12:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[]}`),
			"repos/acme/looper/issues/1/comments": json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/1/timeline": json.RawMessage(`[[]]`),
		},
	})

	coord, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "coordinator.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coord.Close() })
	if _, err := coord.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coord.DB())
	repoPath := t.TempDir()
	now := time.Date(2026, time.May, 14, 12, 0, 0, 0, time.UTC)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "demo", Name: "Demo", RepoPath: repoPath, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Disclosure.Enabled = true
	cfg.Disclosure.Channels.IssueComment = true
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	logText := string(logBytes)
	assertOrderedText(t, logText,
		`"argv":["issue","list"`,
		`"argv":["label","create","kind/bug"`,
		`"argv":["api","repos/acme/looper/issues/1/labels","--method","POST","-f","labels[]=kind/bug","-f","labels[]=area/coordinator","-f","labels[]=complexity/m","-f","labels[]=dispatch/plan"`,
		`"argv":["api","repos/acme/looper/issues/1/comments","--method","POST"`,
		`"argv":["api","repos/acme/looper/issues/1/labels","--method","POST","-f","labels[]=triaged"`,
	)
	if !(strings.Contains(logText, `<!-- looper:stamp v=1 -->`) || strings.Contains(logText, `\u003c!-- looper:stamp v=1 --\u003e`)) || !strings.Contains(logText, `runner=coordinator`) {
		t.Fatal("invocation log missing stamped coordinator comment body")
	}
}

func assertOrderedText(t *testing.T, text string, parts ...string) {
	t.Helper()
	index := 0
	for _, part := range parts {
		next := strings.Index(text[index:], part)
		if next < 0 {
			t.Fatalf("text missing ordered part %q\n%s", part, text)
		}
		index += next + len(part)
	}
}

var _ triage.LLM = stubCoordinatorLLM{}

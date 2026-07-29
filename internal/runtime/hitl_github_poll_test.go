package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func newEnqueueTestRepos(t *testing.T) *storage.Repositories {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "hitl.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	return storage.NewRepositories(coordinator.DB())
}

func TestDetectGitHubHITLAnswer(t *testing.T) {
	comments := []githubAnswerComment{
		{ID: 100, Author: "lefarcen", Body: "<!-- looper:hitl:ask v=1 --> which one?"},
		{ID: 101, Author: "lefarcen", Body: "<!-- looper:stamp --> still working"},
		{ID: 105, Author: "lefarcen", Body: "用 A,改 resize handle"},
		{ID: 110, Author: "someoneelse", Body: "later comment"},
	}
	if got := detectGitHubHITLAnswer(comments, 100, nil); got != "用 A,改 resize handle" {
		t.Fatalf("answer = %q, want the first human reply", got)
	}
	if got := detectGitHubHITLAnswer(comments[:2], 100, nil); got != "" {
		t.Fatalf("answer = %q, want empty", got)
	}
	if got := detectGitHubHITLAnswer(comments, 100, []string{"someoneelse"}); got != "later comment" {
		t.Fatalf("answer = %q, want allowlisted comment", got)
	}
}

func TestPollGitHubHITLAnswersOnce(t *testing.T) {
	commentsByPR := map[int64][]githubAnswerComment{
		42: {{ID: 500, Author: "lefarcen", Body: "<!-- looper:hitl:ask --> ask"}, {ID: 501, Author: "lefarcen", Body: "go with A"}},
		43: {{ID: 600, Author: "lefarcen", Body: "<!-- looper:hitl:ask --> ask"}},
	}
	var deliveredTo []string
	var cleared []int64
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, pr int64, _ string) ([]githubAnswerComment, error) {
			return commentsByPR[pr], nil
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			deliveredTo = append(deliveredTo, loopID+"="+answer)
			return nil
		},
		clearAwaiting: func(_ contextType, _ string, pr int64, _ string) { cleared = append(cleared, pr) },
		projectCWD:    func(string) string { return "/tmp/repo" },
	}
	awaiting := []githubHITLAwaitingLoop{
		{ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500},
		{ID: "loop-b", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 43, AskCommentID: 600},
	}
	if n := pollGitHubHITLAnswersOnce(context.Background(), awaiting, deps); n != 1 {
		t.Fatalf("delivered = %d, want 1", n)
	}
	if len(deliveredTo) != 1 || deliveredTo[0] != "loop-a=go with A" || len(cleared) != 1 || cleared[0] != 42 {
		t.Fatalf("delivered=%v cleared=%v", deliveredTo, cleared)
	}
}

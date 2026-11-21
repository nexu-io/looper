package worker

import (
	"context"
	"testing"
)

func TestExplicitDispatchRejectsWrongRepoAndIsAtomic(t *testing.T) {
	t.Parallel()
	f := newRunnerFixture(t)
	r := New(Options{DB: f.coordinator.DB(), Repos: f.repos, Now: f.now})
	ctx := context.Background()
	input := EnqueueInput{ProjectID: "project_1", Repo: "acme/other", IssueNumber: 99}
	if _, err := r.Enqueue(ctx, input); err == nil {
		t.Fatal("wrong repository was accepted")
	}
	input.Repo = "acme/looper"
	if _, err := f.coordinator.DB().Exec(`CREATE TRIGGER refuse_new_queue BEFORE INSERT ON queue_items BEGIN SELECT RAISE(FAIL, 'forced queue failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Enqueue(ctx, input); err == nil {
		t.Fatal("queue write failure was ignored")
	}
	items, err := f.repos.Loops.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("failed dispatch left orphan loop: %+v", items)
	}
	if _, err := f.coordinator.DB().Exec(`DROP TRIGGER refuse_new_queue`); err != nil {
		t.Fatal(err)
	}
	item, err := r.Enqueue(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := f.repos.Queue.GetLatestByLoopID(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queue == nil || queue.Type != "worker" || queue.TargetID != "issue:acme/looper:99" || queue.Status != "queued" {
		t.Fatalf("dispatch did not use the worker queue: %+v", queue)
	}
	if _, err := r.Enqueue(ctx, input); err == nil {
		t.Fatal("duplicate active task was accepted")
	}
}

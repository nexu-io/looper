package engine

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

type testPlugin struct {
	executed []string
	blockAt  string
	failAt   string
}

func (p *testPlugin) Steps() []string                  { return []string{"prepare", "act", "publish"} }
func (p *testPlugin) BoundaryFor(step string) Boundary { return Boundary(step) }
func (p *testPlugin) Classify(err error, boundary Boundary) *Failure {
	return &Failure{Class: "retryable", Boundary: boundary, Retryable: true, Err: err}
}
func (p *testPlugin) ExecuteStep(_ context.Context, step string, checkpoint int) (StepResult[int], error) {
	p.executed = append(p.executed, step)
	if step == p.failAt {
		return StepResult[int]{}, errors.New("boom")
	}
	result := StepResult[int]{Checkpoint: checkpoint + 1}
	if step == p.blockAt {
		result.Blocked = &Blocked{Condition: "human_answered"}
	}
	return result, nil
}

type testStore struct {
	checkpoint int
	last       string
	blocked    *Blocked
	done       bool
}

func (s *testStore) Load(context.Context) (int, string, error)      { return s.checkpoint, s.last, nil }
func (s *testStore) StepStarted(context.Context, string, int) error { return nil }
func (s *testStore) StepCompleted(_ context.Context, step string, checkpoint int) error {
	s.last, s.checkpoint = step, checkpoint
	return nil
}
func (s *testStore) Blocked(_ context.Context, _ string, checkpoint int, blocked Blocked) error {
	s.checkpoint, s.blocked = checkpoint, &blocked
	return nil
}
func (s *testStore) Done(_ context.Context, checkpoint int) error {
	s.checkpoint, s.done = checkpoint, true
	return nil
}

type testLease struct{ acquired, released bool }

func (l *testLease) Acquire(context.Context) (bool, error) { l.acquired = true; return true, nil }
func (l *testLease) Release(context.Context) error         { l.released = true; return nil }

func TestEngineResumesPersistsAndLeases(t *testing.T) {
	plugin := &testPlugin{}
	store := &testStore{checkpoint: 1, last: "prepare"}
	lease := &testLease{}
	result, err := (Engine[int]{Plugin: plugin, Store: store, Lease: lease}).Run(context.Background())
	if err != nil || result != 3 || !store.done || !lease.acquired || !lease.released || !reflect.DeepEqual(plugin.executed, []string{"act", "publish"}) {
		t.Fatalf("result=%d err=%v store=%#v lease=%#v steps=%v", result, err, store, lease, plugin.executed)
	}
}

func TestEnginePersistsBlockedCondition(t *testing.T) {
	plugin := &testPlugin{blockAt: "act"}
	store := &testStore{}
	result, err := (Engine[int]{Plugin: plugin, Store: store}).Run(context.Background())
	if err != nil || result != 2 || store.blocked == nil || store.blocked.Condition != "human_answered" || store.done {
		t.Fatalf("result=%d err=%v store=%#v", result, err, store)
	}
}

func TestEngineClassifiesAtStepBoundary(t *testing.T) {
	plugin := &testPlugin{failAt: "act"}
	_, err := (Engine[int]{Plugin: plugin, Store: &testStore{}}).Run(context.Background())
	var failure *Failure
	if !errors.As(err, &failure) || failure.Boundary != "act" || !failure.Retryable {
		t.Fatalf("error = %#v", err)
	}
}

func TestStateCollapsesLegacyStatusesIntoFourPhases(t *testing.T) {
	cases := map[string]Phase{"running": PhaseRunning, "queued": PhaseRunning, "awaiting_human": PhaseBlocked, "paused": PhaseBlocked, "completed": PhaseDone, "terminated": PhaseDone, "failed": PhaseDead, "interrupted": PhaseDead}
	for status, want := range cases {
		if got := FromLegacy(status, "", "now").Phase; got != want {
			t.Fatalf("FromLegacy(%q).Phase = %q, want %q", status, got, want)
		}
	}
}

func TestStorageLeaseEnforcesSingleOwnerAndReleases(t *testing.T) {
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	repos.Locks.SetNow(func() time.Time { return now })
	first := StorageLease{Locks: repos.Locks, Key: "lifecycle:loop-1", Owner: "one", Now: func() time.Time { return now }}
	second := StorageLease{Locks: repos.Locks, Key: "lifecycle:loop-1", Owner: "two", Now: func() time.Time { return now }}
	if acquired, err := first.Acquire(context.Background()); err != nil || !acquired {
		t.Fatalf("first acquire = %v, %v", acquired, err)
	}
	if acquired, err := second.Acquire(context.Background()); err != nil || acquired {
		t.Fatalf("second acquire = %v, %v; want held", acquired, err)
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if acquired, err := second.Acquire(context.Background()); err != nil || !acquired {
		t.Fatalf("acquire after release = %v, %v", acquired, err)
	}
}

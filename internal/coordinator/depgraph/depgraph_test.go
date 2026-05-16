package depgraph

import (
	"reflect"
	"testing"
)

func TestReadySet(t *testing.T) {
	t.Parallel()
	repo := "acme/looper"
	blocker := IssueRef{Repo: repo, Number: 2}
	tests := []struct {
		name     string
		tracked  []IssueRef
		snapshot Snapshot
		want     []IssueRef
	}{
		{name: "empty input", want: nil},
		{name: "single issue with no blockers", tracked: []IssueRef{{Repo: repo, Number: 1}}, want: []IssueRef{{Repo: repo, Number: 1}}},
		{
			name:     "open blocker is not ready",
			tracked:  []IssueRef{{Repo: repo, Number: 1}},
			snapshot: Snapshot{BlockedBy: map[IssueRef][]IssueRef{{Repo: repo, Number: 1}: {blocker}}, Issues: map[IssueRef]IssueState{blocker: {State: "open"}}},
			want:     nil,
		},
		{
			name:     "closed completed blocker is ready",
			tracked:  []IssueRef{{Repo: repo, Number: 1}},
			snapshot: Snapshot{BlockedBy: map[IssueRef][]IssueRef{{Repo: repo, Number: 1}: {blocker}}, Issues: map[IssueRef]IssueState{blocker: {State: "closed", StateReason: "completed"}}},
			want:     []IssueRef{{Repo: repo, Number: 1}},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			graph := Build(tt.tracked, tt.snapshot)
			if got := graph.ReadySet(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ReadySet() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCycles(t *testing.T) {
	t.Parallel()
	repo := "acme/looper"
	tests := []struct {
		name     string
		tracked  []IssueRef
		snapshot Snapshot
		want     []Cycle
	}{
		{
			name:     "two node cycle",
			tracked:  []IssueRef{{Repo: repo, Number: 1}, {Repo: repo, Number: 2}},
			snapshot: Snapshot{BlockedBy: map[IssueRef][]IssueRef{{Repo: repo, Number: 1}: {{Repo: repo, Number: 2}}, {Repo: repo, Number: 2}: {{Repo: repo, Number: 1}}}, Issues: map[IssueRef]IssueState{{Repo: repo, Number: 1}: {State: "open"}, {Repo: repo, Number: 2}: {State: "open"}}},
			want:     []Cycle{{{Repo: repo, Number: 1}, {Repo: repo, Number: 2}, {Repo: repo, Number: 1}}},
		},
		{
			name:     "three node cycle",
			tracked:  []IssueRef{{Repo: repo, Number: 1}, {Repo: repo, Number: 2}, {Repo: repo, Number: 3}},
			snapshot: Snapshot{BlockedBy: map[IssueRef][]IssueRef{{Repo: repo, Number: 1}: {{Repo: repo, Number: 2}}, {Repo: repo, Number: 2}: {{Repo: repo, Number: 3}}, {Repo: repo, Number: 3}: {{Repo: repo, Number: 1}}}, Issues: map[IssueRef]IssueState{{Repo: repo, Number: 1}: {State: "open"}, {Repo: repo, Number: 2}: {State: "open"}, {Repo: repo, Number: 3}: {State: "open"}}},
			want:     []Cycle{{{Repo: repo, Number: 1}, {Repo: repo, Number: 2}, {Repo: repo, Number: 3}, {Repo: repo, Number: 1}}},
		},
		{
			name:     "self loop",
			tracked:  []IssueRef{{Repo: repo, Number: 1}},
			snapshot: Snapshot{BlockedBy: map[IssueRef][]IssueRef{{Repo: repo, Number: 1}: {{Repo: repo, Number: 1}}}, Issues: map[IssueRef]IssueState{{Repo: repo, Number: 1}: {State: "open"}}},
			want:     []Cycle{{{Repo: repo, Number: 1}, {Repo: repo, Number: 1}}},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			graph := Build(tt.tracked, tt.snapshot)
			if got := graph.Cycles(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Cycles() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestUnreachableDeps(t *testing.T) {
	t.Parallel()
	repo := "acme/looper"
	graph := Build(
		[]IssueRef{{Repo: repo, Number: 1}},
		Snapshot{
			BlockedBy: map[IssueRef][]IssueRef{{Repo: repo, Number: 1}: {{Repo: repo, Number: 2}, {Repo: "other/repo", Number: 9}}},
			Issues:    map[IssueRef]IssueState{},
		},
	)
	want := []IssueRef{{Repo: repo, Number: 2}, {Repo: "other/repo", Number: 9}}
	if got := graph.UnreachableDeps(); !reflect.DeepEqual(got, want) {
		t.Fatalf("UnreachableDeps() = %#v, want %#v", got, want)
	}
}

func TestClassifyBlockerState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		state             IssueState
		wantSatisfied     bool
		wantRequiresRetry bool
	}{
		{name: "completed", state: IssueState{State: "closed", StateReason: "completed"}, wantSatisfied: true},
		{name: "not planned", state: IssueState{State: "closed", StateReason: "not_planned"}, wantRequiresRetry: true},
		{name: "duplicate", state: IssueState{State: "closed", StateReason: "duplicate"}, wantRequiresRetry: true},
		{name: "open", state: IssueState{State: "open"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyBlockerState(tt.state)
			if got.Satisfied != tt.wantSatisfied || got.RequiresReTriage != tt.wantRequiresRetry {
				t.Fatalf("classifyBlockerState(%#v) = %#v, want satisfied=%v requiresReTriage=%v", tt.state, got, tt.wantSatisfied, tt.wantRequiresRetry)
			}
		})
	}
}

func TestBlockersOfReturnsOnlyUnsatisfiedBlockers(t *testing.T) {
	t.Parallel()
	repo := "acme/looper"
	graph := Build(
		[]IssueRef{{Repo: repo, Number: 1}},
		Snapshot{
			BlockedBy: map[IssueRef][]IssueRef{{Repo: repo, Number: 1}: {{Repo: repo, Number: 2}, {Repo: repo, Number: 3}, {Repo: repo, Number: 4}}},
			Issues: map[IssueRef]IssueState{
				{Repo: repo, Number: 2}: {State: "closed", StateReason: "completed"},
				{Repo: repo, Number: 3}: {State: "open"},
				{Repo: repo, Number: 4}: {State: "closed", StateReason: "not_planned"},
			},
		},
	)
	got := graph.BlockersOf(IssueRef{Repo: repo, Number: 1})
	want := []Blocker{
		{Issue: IssueRef{Repo: repo, Number: 3}, State: "open"},
		{Issue: IssueRef{Repo: repo, Number: 4}, State: "closed", StateReason: "not_planned", RequiresReTriage: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BlockersOf() = %#v, want %#v", got, want)
	}
}

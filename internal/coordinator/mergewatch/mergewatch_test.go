package mergewatch

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 20, 12, 0, 0, 0, time.UTC)
	mergeable := true
	conflicted := false
	beforeDeadline := now.Add(-10 * time.Minute)
	afterDeadline := now.Add(-16 * time.Minute)

	for _, tc := range []struct {
		name         string
		snapshot     PRSnapshot
		prior        *PriorWatchMarker
		budget       RetryBudget
		wantKind     WatchActionKind
		wantUnknown  *time.Time
		wantDeadline bool
		wantRetries  int
		wantDelay    time.Duration
		wantExhaust  bool
	}{
		{name: "merged", snapshot: PRSnapshot{PRNumber: 42, Merged: true}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionMerged},
		{name: "still pending", snapshot: PRSnapshot{PRNumber: 42, Mergeable: &mergeable, MergeableState: "blocked", RequiredChecks: RequiredCheckSummary{Pending: []string{"ci"}}}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionStillPending},
		{name: "indeterminate before deadline", snapshot: PRSnapshot{PRNumber: 42, HeadSHA: "abc", AutoMergeEnabled: true, MergeableState: "unknown"}, prior: &PriorWatchMarker{PRNumber: 42, HeadSHA: "abc", FirstUnknownAt: &beforeDeadline}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionIndeterminate, wantUnknown: &beforeDeadline},
		{name: "indeterminate after deadline", snapshot: PRSnapshot{PRNumber: 42, HeadSHA: "abc", AutoMergeEnabled: true, Mergeable: &conflicted, MergeableState: "unknown"}, prior: &PriorWatchMarker{PRNumber: 42, HeadSHA: "abc", FirstUnknownAt: &afterDeadline}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionBranchProtectionChanged, wantUnknown: &afterDeadline, wantDeadline: true},
		{name: "conflict", snapshot: PRSnapshot{PRNumber: 42, Mergeable: &conflicted, MergeableState: "dirty"}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionConflict},
		{name: "red ci", snapshot: PRSnapshot{PRNumber: 42, Mergeable: &mergeable, MergeableState: "unstable", RequiredChecks: RequiredCheckSummary{Failed: []string{"ci"}}}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionRedCI},
		{name: "human disabled auto merge", snapshot: PRSnapshot{PRNumber: 42, AutoMergeEnabled: false, Mergeable: &mergeable, MergeableState: "clean"}, prior: &PriorWatchMarker{PRNumber: 42, HeadSHA: "abc"}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionHumanDisabledAutoMerge},
		{name: "transient error first observation", snapshot: PRSnapshot{PRNumber: 42, HeadSHA: "abc", TemporaryError: &TemporaryError{SuggestedDelay: time.Minute}}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionTransientError, wantRetries: 3, wantDelay: time.Minute},
		{name: "transient exhausted", snapshot: PRSnapshot{PRNumber: 42, HeadSHA: "abc", TemporaryError: &TemporaryError{SuggestedDelay: 2 * time.Minute}}, prior: &PriorWatchMarker{PRNumber: 42, HeadSHA: "abc", Retries: 1}, budget: RetryBudget{Now: now, TransientRetries: 3, MaxIndeterminateDuration: 15 * time.Minute}, wantKind: ActionTransientError, wantDelay: 2 * time.Minute, wantExhaust: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Classify(tc.snapshot, tc.prior, tc.budget)
			if got.Kind != tc.wantKind || got.DeadlineExceeded != tc.wantDeadline || got.RetriesLeft != tc.wantRetries || got.SuggestedDelay != tc.wantDelay || got.Exhausted != tc.wantExhaust {
				t.Fatalf("Classify() = %#v", got)
			}
			switch {
			case tc.wantUnknown == nil && got.FirstUnknownAt != nil:
				t.Fatalf("FirstUnknownAt = %v, want nil", got.FirstUnknownAt)
			case tc.wantUnknown != nil && (got.FirstUnknownAt == nil || !got.FirstUnknownAt.Equal(*tc.wantUnknown)):
				t.Fatalf("FirstUnknownAt = %v, want %v", got.FirstUnknownAt, tc.wantUnknown)
			}
		})
	}
}

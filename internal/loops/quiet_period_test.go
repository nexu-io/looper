package loops

import (
	"testing"
	"time"
)

func TestDebounceScheduleQuietOffReturnsNow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	existing := now.Add(5 * time.Minute)

	if got := DebounceSchedule(now, 0, time.Time{}); !got.Equal(now) {
		t.Fatalf("quiet=0 zero existing = %v, want %v", got, now)
	}
	// Quiet off does not preserve a later existing time; caller composes backoff separately.
	if got := DebounceSchedule(now, -1, existing); !got.Equal(now) {
		t.Fatalf("quiet<0 with existing = %v, want %v", got, now)
	}
}

func TestDebounceScheduleExtendsNeverShortens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	quiet := 60

	got := DebounceSchedule(now, quiet, time.Time{})
	want := now.Add(60 * time.Second)
	if !got.Equal(want) {
		t.Fatalf("new signal = %v, want %v", got, want)
	}

	// Existing later than now+quiet is preserved.
	existingLater := now.Add(5 * time.Minute)
	got = DebounceSchedule(now, quiet, existingLater)
	if !got.Equal(existingLater) {
		t.Fatalf("existing later = %v, want %v", got, existingLater)
	}

	// Existing earlier than now+quiet is extended (reset).
	existingEarlier := now.Add(10 * time.Second)
	got = DebounceSchedule(now, quiet, existingEarlier)
	if !got.Equal(want) {
		t.Fatalf("existing earlier = %v, want extended %v", got, want)
	}
}

func TestMaxTime(t *testing.T) {
	t.Parallel()
	a := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := a.Add(time.Hour)

	if got := MaxTime(time.Time{}, b); !got.Equal(b) {
		t.Fatalf("zero left = %v, want %v", got, b)
	}
	if got := MaxTime(a, time.Time{}); !got.Equal(a) {
		t.Fatalf("zero right = %v, want %v", got, a)
	}
	if got := MaxTime(a, b); !got.Equal(b) {
		t.Fatalf("max = %v, want %v", got, b)
	}
	if got := MaxTime(b, a); !got.Equal(b) {
		t.Fatalf("max swapped = %v, want %v", got, b)
	}
}

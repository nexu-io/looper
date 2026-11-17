package loops

import "time"

// DebounceSchedule computes the next eligible time for quiet-period debounce.
//
// Quiet period settles new actionable signals before work starts:
//   - quietSeconds <= 0 → eligible immediately (now)
//   - otherwise next = max(existingAvailableAt, now+quiet)
//
// Existing AvailableAt is never shortened by debounce (extend/reset only).
// Retry backoff and no-op follow-up delays remain separate constraints; callers
// that need both should take the max of DebounceSchedule and those times.
func DebounceSchedule(now time.Time, quietSeconds int, existingAvailableAt time.Time) time.Time {
	if quietSeconds <= 0 {
		return now
	}
	candidate := now.Add(time.Duration(quietSeconds) * time.Second)
	if !existingAvailableAt.IsZero() && existingAvailableAt.After(candidate) {
		return existingAvailableAt
	}
	return candidate
}

// MaxTime returns the later of left and right, treating zero as absent.
func MaxTime(left, right time.Time) time.Time {
	if left.IsZero() {
		return right
	}
	if right.IsZero() {
		return left
	}
	if right.After(left) {
		return right
	}
	return left
}

// Package processcontainment provides the process containment handle used by
// the Execution Supervisor ownership program (ADR-0015 / issue #574).
//
// A Handle is the Authority for stop delivery and release of an owned process
// group. It owns:
//
//   - process-group configuration at spawn (Configure)
//   - signal delivery (TERM then escalation)
//   - exactly-once wait/reap of the leader
//   - descendant drain after leader exit
//   - confirmed-dead reporting
//
// Signal delivery alone is never success. Kill/Drain succeed only when the
// owned containment is confirmed non-runnable and the leader is reaped, or
// they return an explicit failure/timeout.
//
// This package is additive until producers migrate (#576/#577). Do not remove
// live PID fallback from production paths in this slice.
package processcontainment

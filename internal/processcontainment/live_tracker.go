package processcontainment

// LiveTracker is the optional Supervisor hook for non-agent containment handles
// (ADR-0015 / #577). Supervisor-owned spawn boundaries (validation/shell,
// trusted review-submit children) register live handles so daemon shutdown can
// wait for confirmed drain and record Kill/Drain failures for retain-storage.
//
// Independently lifecycle-owned shell users (git/gh/tea gateways, osascript)
// leave Tracker nil.
type LiveTracker interface {
	// Track registers a live handle until release is called. release is
	// idempotent and safe after Kill/Drain completes (success or failure).
	Track(handle *Handle) (release func())
	// ReportDrainFailure records a Kill/Drain failure observed on the caller's
	// ownership path so Runtime.Stop can retain SQLite instead of reporting a
	// clean stop with undrained ownership.
	ReportDrainFailure(err error)
}

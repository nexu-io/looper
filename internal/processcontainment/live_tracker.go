package processcontainment

import (
	"errors"
	"os/exec"
)

// ErrTrackerAdmissionClosed is returned by LiveTracker.BeginTrack when the
// Supervisor has already closed spawn admission (BeginShutdown). Callers must
// not Start/Bind a process after this error — there is no shutdown-wait slot.
var ErrTrackerAdmissionClosed = errors.New("containment tracker: admission closed")

// LiveTracker is the optional Supervisor hook for non-agent containment handles
// (ADR-0015 / #577). Supervisor-owned spawn boundaries (validation/shell,
// trusted review-submit children, model-catalog probes) register live handles
// so daemon shutdown can wait for confirmed drain and record Kill/Drain
// failures for retain-storage.
//
// Independently lifecycle-owned shell users (git/gh/tea gateways, osascript)
// leave Tracker nil.
//
// Spawn boundary contract (closes Start→Track vs BeginShutdown races):
//
//  1. BeginTrack before processcontainment.Start/Bind — reserves a wait slot
//     so BeginShutdown cannot return while Start→Track is in flight.
//  2. On Start/Bind failure, call the end func from BeginTrack.
//  3. On success, Track the handle, then call end (Track does not end the
//     reservation; callers own end so refuse-path Kill can finish first).
//  4. Track after admission is closed confirmed-drains the handle (like agent
//     BindHandle refuse) and returns a no-op release.
//
// Use StartTracked / BindTracked helpers to enforce this order.
type LiveTracker interface {
	// BeginTrack reserves a shutdown-wait slot before Start/Bind. Returns
	// ErrTrackerAdmissionClosed when admission is already closed. The end
	// func is idempotent and must be called when the window closes (after
	// Track, or on Start/Bind failure without Track).
	BeginTrack() (end func(), err error)
	// Track registers a live handle until release is called. release is
	// idempotent and safe after Kill/Drain completes (success or failure).
	// When admission is closed, Track confirmed-drains the handle and returns
	// a no-op release so a just-started process cannot outlive shutdown.
	Track(handle *Handle) (release func())
	// ReportDrainFailure records a Kill/Drain failure observed on the caller's
	// ownership path so Runtime.Stop can retain SQLite instead of reporting a
	// clean stop with undrained ownership.
	ReportDrainFailure(err error)
}

// StartTracked admits a tracker window (when tracker != nil), Starts the
// command, Tracks the handle, then ends the admit window. On Start failure the
// admit window is ended without Track; Start itself kills/reaps the process
// group when Bind fails after a successful cmd.Start, so the child cannot
// outlive the admission window without a tracked Handle. When tracker is nil
// this is Start with a no-op release.
func StartTracked(tracker LiveTracker, cmd *exec.Cmd, opts Options) (*Handle, func(), error) {
	end, err := beginTrackOptional(tracker)
	if err != nil {
		return nil, nil, err
	}
	handle, err := Start(cmd, opts)
	if err != nil {
		// Start already cleaned up any orphaned process group on Bind failure.
		end()
		return nil, nil, err
	}
	release := trackOptional(tracker, handle)
	end()
	return handle, release, nil
}

// BindTracked is StartTracked for an already-started command (Configure+Start
// done by the caller). On Bind failure the admit window is ended without Track.
func BindTracked(tracker LiveTracker, cmd *exec.Cmd, opts Options) (*Handle, func(), error) {
	end, err := beginTrackOptional(tracker)
	if err != nil {
		return nil, nil, err
	}
	handle, err := Bind(cmd, opts)
	if err != nil {
		end()
		return nil, nil, err
	}
	release := trackOptional(tracker, handle)
	end()
	return handle, release, nil
}

func beginTrackOptional(tracker LiveTracker) (end func(), err error) {
	if tracker == nil {
		return func() {}, nil
	}
	end, err = tracker.BeginTrack()
	if err != nil {
		return nil, err
	}
	if end == nil {
		return func() {}, nil
	}
	return end, nil
}

func trackOptional(tracker LiveTracker, handle *Handle) func() {
	if tracker == nil {
		return func() {}
	}
	release := tracker.Track(handle)
	if release == nil {
		return func() {}
	}
	return release
}

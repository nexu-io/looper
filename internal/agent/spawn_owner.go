package agent

import (
	"context"
	"errors"

	"github.com/nexu-io/looper/internal/processcontainment"
)

// ErrSpawnAdmissionClosed is returned when a spawn is refused because the
// Execution Supervisor admission is closed (daemon shutdown or degraded).
var ErrSpawnAdmissionClosed = errors.New("agent spawn admission is closed")

// ErrSpawnLoopStopping is returned when a spawn is refused because the target
// loop is already stopping.
var ErrSpawnLoopStopping = errors.New("agent spawn refused: loop is stopping")

// ErrSpawnStoppedDuringBind is returned when stop/shutdown races with spawn
// after the process started: the containment handle was killed and confirmed
// drained before Start returns, so callers never observe an unowned live process.
var ErrSpawnStoppedDuringBind = errors.New("agent spawn stopped during containment bind")

// SpawnMeta identifies one agent execution admitted at the common executor boundary.
type SpawnMeta struct {
	LoopID      string
	RunID       string
	ExecutionID string
}

// SoftKillFunc notifies a running agent execution of an external stop so status
// transitions (killed) stay consistent while the Supervisor confirmed-drains
// the bound containment handle.
type SoftKillFunc func(reason string) error

// SpawnLease is held from before cmd.Start until the execution fully ends.
// It is the Supervisor ownership token for one in-scope agent process.
type SpawnLease interface {
	// Context is cancelled when stop/shutdown races the in-flight spawn or run.
	// Cancellation must prevent native-resume fallback from starting another process.
	Context() context.Context
	// BindHandle binds the containment handle immediately after spawn and
	// attaches softKill for status updates. If stop already closed admission
	// for this lease, BindHandle kills and confirmed-drains the handle, then
	// returns ErrSpawnStoppedDuringBind (or a wrap). Start must not return
	// success after a non-nil BindHandle error.
	BindHandle(handle *processcontainment.Handle, softKill SoftKillFunc) error
	// Release drops the lease after the execution is fully done (success or fail).
	Release()
}

// SpawnOwner admits agent spawns under the Execution Supervisor (ADR-0015 / #576).
// Every in-scope daemon agent producer must pass through AdmitSpawn at the
// common executor boundary before cmd.Start.
type SpawnOwner interface {
	AdmitSpawn(ctx context.Context, meta SpawnMeta) (SpawnLease, error)
}

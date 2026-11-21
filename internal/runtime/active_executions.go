package runtime

import (
	"time"

	"github.com/nexu-io/looper/internal/executionhost"
)

// Compatibility names keep the daemon on the shared execution owner.
type ActiveExecutionRegistry = executionhost.ActiveExecutionRegistry
type OperationMeta = executionhost.OperationMeta
type OperationPermit = executionhost.OperationPermit
type OperationLease = executionhost.OperationLease

var ErrAgentLiveHandleMissing = executionhost.ErrAgentLiveHandleMissing
var ErrOperationAdmissionClosed = executionhost.ErrOperationAdmissionClosed
var ErrOperationLeaseCancelled = executionhost.ErrOperationLeaseCancelled
var ErrOperationFinalizeFailed = executionhost.ErrOperationFinalizeFailed

func NewActiveExecutionRegistry(timeout ...time.Duration) *ActiveExecutionRegistry {
	return executionhost.NewActiveExecutionRegistry(timeout...)
}

var errShutdownDrainTimeout = executionhost.ErrShutdownDrainTimeout

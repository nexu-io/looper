package runtime

import "github.com/nexu-io/looper/internal/executionhost"

// Keep both entrypoints on the same recovery implementation.
func (r *Runtime) recoveryHost() *executionhost.Recovery {
	return &executionhost.Recovery{Config: r.Config, Logger: r.logger, ReadProcessCommand: r.readProcessCommand, ActiveExecutions: r.activeExecutions}
}

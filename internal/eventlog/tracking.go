package eventlog

import (
	"context"
	"github.com/nexu-io/looper/internal/storage"
)

// Track records metadata after an observed transition. A failed tracking write
// must not replay a task or a remote side effect; callers report its error.
func Track(ctx context.Context, repos *storage.Repositories, input AppendInput) error {
	input.Once = true
	input.CorrelationID = input.LoopID
	input.ActorID = stringPointer("looper")
	return Append(ctx, repos, input)
}

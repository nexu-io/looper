package executionhost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

// finalizeContext keeps durable queue cleanup alive after a host stop, bounded
// so a blocked database cannot hold shutdown indefinitely.
func finalizeContext(parent context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, 5*time.Second)
}

// RecoverCancelledClaim finds a durable claim lost during an UPDATE...RETURNING
// deadline. Existing live owners are excluded, including earlier batches with
// the same timestamp. Multiple unowned matches cannot bind to a single lease.
func RecoverCancelledClaim(ctx context.Context, repos *storage.Repositories, claimedBy, nowISO string, isOwned func(string) bool) (*storage.QueueItemRecord, error) {
	if repos == nil || repos.Queue == nil {
		return nil, errors.New("queue repository is not configured for ambiguous claim recovery")
	}
	// Bound recovery: WithoutCancel alone would wait indefinitely on blocked
	// SQLite while the lease stays pending and shutdown times out retaining storage.
	recoverCtx, cancel := finalizeContext(ctx)
	defer cancel()
	items, err := repos.Queue.ListRunningClaimedBy(recoverCtx, claimedBy, nowISO)
	if err != nil {
		return nil, err
	}
	var found *storage.QueueItemRecord
	for i := range items {
		id := items[i].ID
		if isOwned != nil && isOwned(id) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: multiple unowned running claims after cancelled ClaimNext", ErrOperationFinalizeFailed)
		}
		item := items[i]
		found = &item
	}
	return found, nil
}

// FinalizeCancelledClaim requeues a claim after cancellation before releasing
// its owner. The running-status guard preserves a concurrent pause/termination;
// a bounded, detached context allows cleanup after the host is cancelled.
func FinalizeCancelledClaim(ctx context.Context, repos *storage.Repositories, item storage.QueueItemRecord, now func() time.Time) error {
	if repos == nil || repos.Queue == nil {
		return errors.New("queue repository is not configured for cancelled-claim finalize")
	}
	// Bound finalization: detach cancel/deadline then apply a fresh timeout so
	// shutdown/timed ticks still requeue without unbounded DB waits under a
	// retained operation lease.
	ctx, cancel := finalizeContext(ctx)
	defer cancel()
	if now == nil {
		now = time.Now
	}
	nowISO := eventlog.FormatJavaScriptISOString(now().UTC())
	msg := "operation lease cancelled before queue processor start"
	// Atomic status-guarded requeue: only updates while still running. Zero rows
	// (already cancelled/completed/failed) is success — do not resurrect.
	if err := repos.Queue.MarkRetryIfRunning(ctx, storage.QueueMarkRetryInput{
		ID:           item.ID,
		AvailableAt:  nowISO,
		Attempts:     item.Attempts,
		ErrorMessage: &msg,
		ErrorKind:    "retryable_transient",
		UpdatedAt:    nowISO,
	}); err != nil {
		return err
	}
	// Verify durable observation: still-running after guarded requeue means
	// the write did not take effect (e.g. archived project) and ownership must
	// not pretend finalize succeeded.
	got, err := repos.Queue.GetByID(ctx, item.ID)
	if err != nil {
		return err
	}
	if got == nil || got.Status == "running" {
		return fmt.Errorf("%w: claim %s still running after requeue", ErrOperationFinalizeFailed, item.ID)
	}
	return nil
}

// EnsureClaimFinalized guarantees typed durable finalization after a runner
// returns. Happy-path runners already Complete/Fail; this closes the gap where
// a runner error leaves status=running under a about-to-be-released lease.
// Returns nil when the claim is no longer running (already finalized).
func EnsureClaimFinalized(ctx context.Context, repos *storage.Repositories, item storage.QueueItemRecord, runErr error, now func() time.Time) error {
	ctx, cancel := finalizeContext(ctx)
	defer cancel()
	if repos == nil || repos.Queue == nil {
		if runErr != nil {
			return errors.New("queue repository is not configured for finalize")
		}
		return nil
	}
	got, err := repos.Queue.GetByID(ctx, item.ID)
	if err != nil {
		return err
	}
	if got == nil || got.Status != "running" {
		return nil
	}
	// Still running after processor return: typed durable finalization.
	if now == nil {
		now = time.Now
	}
	nowISO := eventlog.FormatJavaScriptISOString(now().UTC())
	msg := "queue processor returned without durable finalize"
	if runErr != nil {
		msg = runErr.Error()
	}
	kind := "retryable_transient"
	if runErr == nil {
		kind = "non_retryable"
	}
	if err := repos.Queue.Fail(ctx, storage.QueueFailInput{
		ID:           item.ID,
		Attempts:     got.Attempts,
		FinishedAt:   nowISO,
		ErrorMessage: &msg,
		ErrorKind:    kind,
		UpdatedAt:    nowISO,
	}); err != nil {
		return err
	}
	after, err := repos.Queue.GetByID(ctx, item.ID)
	if err != nil {
		return err
	}
	if after == nil || after.Status == "running" {
		return fmt.Errorf("%w: claim %s still running after fail", ErrOperationFinalizeFailed, item.ID)
	}
	return nil
}

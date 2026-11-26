package executionhost

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestBindClaimRefusesAfterShutdownWithoutStartingPermit(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}

	// Mark admission closed without BeginShutdown wait (bound finalizer drain).
	// BindClaim must return an explicit refuse and retain ownership until Release.
	reg.mu.Lock()
	reg.admissionClosed = true
	reg.shutdownReason = "drain"
	reg.mu.Unlock()

	loopID := "loop-bind-refuse"
	item := storage.QueueItemRecord{ID: "qi-bind-refuse", Type: "worker", LoopID: &loopID, Status: "running"}
	permit, err := lease.BindClaim(item)
	if !errors.Is(err, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim error = %v, want ErrOperationLeaseCancelled", err)
	}
	if permit.Valid() {
		t.Fatal("BindClaim must not return a valid permit after cancel")
	}
	if !reg.OwnsQueueClaim(item.ID) {
		t.Fatal("cancelled bind must retain ownership until durable finalize + Release")
	}
	lease.Release()
	if reg.OwnsQueueClaim(item.ID) {
		t.Fatal("Release must drop ownership")
	}
}

func TestBindClaimRefusesWhenLoopStopping(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	release, err := reg.BeginLoopStop("loop-stop", "halt")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()

	loopID := "loop-stop"
	item := storage.QueueItemRecord{ID: "qi-loop-stop", Type: "worker", LoopID: &loopID, Status: "running"}
	permit, err := lease.BindClaim(item)
	if !errors.Is(err, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim error = %v, want ErrOperationLeaseCancelled", err)
	}
	if permit.Valid() {
		t.Fatal("permit must be invalid when loop is stopping")
	}
	if !lease.Owns(item.ID) {
		t.Fatal("lease must own claim until Release after cancel-bind")
	}
	lease.Release()
}

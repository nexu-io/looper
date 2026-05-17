package runtime

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonLockRejectsSecondHolderAndReacquiresAfterRelease(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "looperd.lock")
	first, err := acquireDaemonLock(path, "first", time.Date(2026, time.May, 17, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("first acquireDaemonLock() error = %v", err)
	}
	second, err := acquireDaemonLock(path, "second", time.Now())
	if err == nil {
		_ = second.Release()
		t.Fatal("second acquireDaemonLock() error = nil, want lock failure")
	}
	if !strings.Contains(err.Error(), "first") {
		t.Fatalf("second acquire error = %q, want existing holder detail", err.Error())
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	third, err := acquireDaemonLock(path, "third", time.Now())
	if err != nil {
		t.Fatalf("third acquireDaemonLock() error = %v", err)
	}
	_ = third.Release()
}

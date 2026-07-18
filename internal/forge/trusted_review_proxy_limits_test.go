package forge

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestTrustedReviewProxyRejectsOversizedStdinBeforeStartingChild(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	marker := filepath.Join(dir, "child-ran")
	if err := os.WriteFile(realLooper, []byte(trustedReviewProxyStubScript("touch \""+marker+"\"\n")), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}
	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, nil, "acme/looper#1", dir, config.Config{}, testTrustedReviewPolicy(), nil)
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv(TrustedReviewSockEnv, sockPath)
	t.Setenv(trustedReviewProxySkipEnv, "")
	err = ProxyReviewSubmit(
		[]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"},
		make([]byte, maxTrustedReviewProxyStdinBytes+1),
		dir,
	)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("ProxyReviewSubmit(oversized) error = %v, want size-limit rejection", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("oversized request started child: %v", err)
	}
}

func TestTrustedReviewProxyDrainsBackgroundChildAfterLeaderExitsWithPipes(t *testing.T) {
	// Child leader exits 0 after spawning a same-group background sleeper that
	// inherits stdout. Wait would hang on pipe copy; proxy must Drain and return.
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	childPIDPath := filepath.Join(dir, "child.pid")
	script := trustedReviewProxyStubScript(`
(sleep 60) &
echo $! > "` + childPIDPath + `"
exit 0
`)
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}
	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, nil, "acme/looper#1", dir, config.Config{}, testTrustedReviewPolicy(), nil)
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv(TrustedReviewSockEnv, sockPath)
	t.Setenv(trustedReviewProxySkipEnv, "")

	done := make(chan error, 1)
	go func() {
		done <- ProxyReviewSubmit(
			[]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"},
			nil,
			dir,
		)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProxyReviewSubmit() error = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ProxyReviewSubmit hung after leader exit with background pipe-holding child")
	}
}

func TestTrustedReviewProxyCleanupClosesPartialConnectionsAndKillsChildGroup(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	started := filepath.Join(dir, "started")
	script := trustedReviewProxyStubScript("touch \"" + started + "\"\nsleep 30\n")
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}
	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, nil, "acme/looper#1", dir, config.Config{}, testTrustedReviewPolicy(), nil)
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)

	// One handler reaches cmd.Wait while another remains blocked on a partial
	// request. Cleanup must cancel both instead of waiting for either deadline.
	active, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("Dial(active) error = %v", err)
	}
	if err := json.NewEncoder(active).Encode(trustedReviewProxyRequest{Argv: []string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}}); err != nil {
		t.Fatalf("Encode(active) error = %v", err)
	}
	partial, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("Dial(partial) error = %v", err)
	}
	if _, err := partial.Write([]byte("{")); err != nil {
		t.Fatalf("Write(partial) error = %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("trusted review child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup blocked on active child or partial connection")
	}
	_ = active.Close()
	_ = partial.Close()
}

func TestTrustedReviewBoundedBufferTruncatesWithoutBackpressuringChild(t *testing.T) {
	buffer := newTrustedReviewBoundedBuffer(4)
	written, err := buffer.Write([]byte("abcdef"))
	if err != nil || written != 6 {
		t.Fatalf("Write() = (%d, %v), want (6, nil)", written, err)
	}
	if got := buffer.String(); got != "abcd" || !buffer.Truncated() {
		t.Fatalf("bounded buffer = (%q, %t), want (abcd, true)", got, buffer.Truncated())
	}
}

// TestTrustedReviewBoundedBufferConcurrentWriteAndRead covers the proxy path
// where waitCtx cancel unblocks handle.Wait while cmd stdout/stderr copy
// goroutines may still Write; response assembly must read safely under race.
func TestTrustedReviewBoundedBufferConcurrentWriteAndRead(t *testing.T) {
	buffer := newTrustedReviewBoundedBuffer(1 << 20)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chunk := []byte("concurrent-write-chunk\n")
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = buffer.Write(chunk)
				}
			}
		}()
	}
	// Interleave String/Truncated with writers the way response assembly does
	// after early wait cancellation on the containment-failure path.
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = buffer.String()
		_ = buffer.Truncated()
	}
	close(stop)
	wg.Wait()
	// Final snapshot must be consistent (no panic / no race under -race).
	got := buffer.String()
	if len(got) == 0 {
		t.Fatal("String() empty after concurrent writes")
	}
}

func TestMergeTrustedReviewTruncationErrorPreservesContainment(t *testing.T) {
	t.Parallel()
	if got := mergeTrustedReviewTruncationError(""); got != trustedReviewOutputTruncatedMsg {
		t.Fatalf("empty existing = %q, want truncation-only", got)
	}
	existing := "process containment: not confirmed dead"
	got := mergeTrustedReviewTruncationError(existing)
	if !strings.Contains(got, existing) {
		t.Fatalf("merged = %q, want to keep containment error", got)
	}
	if !strings.Contains(got, trustedReviewOutputTruncatedMsg) {
		t.Fatalf("merged = %q, want truncation message", got)
	}
}

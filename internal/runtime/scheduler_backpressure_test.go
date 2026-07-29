package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/disk"
)

func backpressureConfig(enabled bool, path string, high, hard float64) *config.Config {
	cfg := &config.Config{}
	cfg.Daemon.DiskBackpressure = config.DiskBackpressureConfig{
		Enabled:              enabled,
		Path:                 path,
		HighWatermarkPercent: high,
		HardStopPercent:      hard,
	}
	return cfg
}

// stubDiskUsage overrides the capacity seam for the duration of a test.
func stubDiskUsage(t *testing.T, usedPercent float64, err error) {
	t.Helper()
	original := diskUsageStat
	resetDiskBackpressureLogStates()
	diskUsageStat = func(path string) (disk.Usage, error) {
		if err != nil {
			return disk.Usage{}, err
		}
		return disk.Usage{Path: path, TotalBytes: 100 << 30, AvailBytes: 10 << 30, UsedPercent: usedPercent}, nil
	}
	t.Cleanup(func() {
		diskUsageStat = original
		resetDiskBackpressureLogStates()
	})
}

func resetDiskBackpressureLogStates() {
	diskBackpressureLogMu.Lock()
	defer diskBackpressureLogMu.Unlock()
	diskBackpressureLogStates = map[string]diskBackpressureLogState{}
}

func TestDiskBackpressureClampAllowsWhenDisabled(t *testing.T) {
	stubDiskUsage(t, 99, nil) // full disk, but backpressure off
	cfg := backpressureConfig(false, "/wt", 85, 93)
	if got := diskBackpressureClamp(5, cfg, nil); got != 5 {
		t.Fatalf("disabled backpressure must not clamp; got %d", got)
	}
}

func TestDiskBackpressureClampNoConfigOrNoSlots(t *testing.T) {
	stubDiskUsage(t, 99, nil)
	if got := diskBackpressureClamp(0, backpressureConfig(true, "/wt", 85, 93), nil); got != 0 {
		t.Fatalf("zero slots must stay zero; got %d", got)
	}
	if got := diskBackpressureClamp(3, nil, nil); got != 3 {
		t.Fatalf("nil config must not clamp; got %d", got)
	}
}

func TestDiskBackpressureClampAllowsBelowHighWatermark(t *testing.T) {
	stubDiskUsage(t, 70, nil)
	cfg := backpressureConfig(true, "/wt", 85, 93)
	if got := diskBackpressureClamp(6, cfg, nil); got != 6 {
		t.Fatalf("below high watermark must not clamp; got %d", got)
	}
}

func TestDiskBackpressureClampBlocksAtHighWatermark(t *testing.T) {
	stubDiskUsage(t, 87, nil) // >= high(85), < hard(93) => warn-level pause
	logger := &capturingSchedulerLogger{}
	cfg := backpressureConfig(true, "/wt", 85, 93)
	if got := diskBackpressureClamp(4, cfg, logger); got != 0 {
		t.Fatalf("at/above high watermark must clamp to 0; got %d", got)
	}
	logger.requireMessage(t, "disk backpressure: above high watermark — pausing new run claims")
}

func TestDiskBackpressureClampHardStopSignalsEmergency(t *testing.T) {
	stubDiskUsage(t, 95, nil) // >= hard(93) => error-level emergency
	logger := &capturingSchedulerLogger{}
	cfg := backpressureConfig(true, "/wt", 85, 93)
	if got := diskBackpressureClamp(4, cfg, logger); got != 0 {
		t.Fatalf("at/above hard stop must clamp to 0; got %d", got)
	}
	logger.requireMessage(t, "disk backpressure: hard stop — refusing to claim new runs (disk emergency)")
}

func TestDiskBackpressureClampRateLimitsRepeatedWarningsAndReportsRecovery(t *testing.T) {
	usedPercent := 87.0
	original := diskUsageStat
	diskUsageStat = func(path string) (disk.Usage, error) {
		return disk.Usage{Path: path, TotalBytes: 100 << 30, AvailBytes: 13 << 30, UsedPercent: usedPercent}, nil
	}
	now := time.Date(2026, time.July, 16, 11, 0, 0, 0, time.UTC)
	originalNow := diskBackpressureNow
	diskBackpressureNow = func() time.Time { return now }
	resetDiskBackpressureLogStates()
	t.Cleanup(func() {
		diskUsageStat = original
		diskBackpressureNow = originalNow
		resetDiskBackpressureLogStates()
	})

	logger := &capturingSchedulerLogger{}
	cfg := backpressureConfig(true, "/wt", 85, 93)
	for range 20 {
		if got := diskBackpressureClamp(1, cfg, logger); got != 0 {
			t.Fatalf("blocked clamp = %d, want 0", got)
		}
	}
	if got := countSchedulerLogMessages(logger, "disk backpressure: above high watermark — pausing new run claims"); got != 1 {
		t.Fatalf("warning count = %d, want 1 within throttle interval", got)
	}

	now = now.Add(diskBackpressureLogInterval)
	diskBackpressureClamp(1, cfg, logger)
	if got := countSchedulerLogMessages(logger, "disk backpressure: above high watermark — pausing new run claims"); got != 2 {
		t.Fatalf("warning count after interval = %d, want 2", got)
	}

	usedPercent = 80
	if got := diskBackpressureClamp(1, cfg, logger); got != 1 {
		t.Fatalf("recovered clamp = %d, want 1", got)
	}
	if got := countSchedulerLogMessages(logger, "disk backpressure: recovered below high watermark — resuming new run claims"); got != 1 {
		t.Fatalf("recovery count = %d, want 1", got)
	}
}

func countSchedulerLogMessages(logger *capturingSchedulerLogger, message string) int {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	count := 0
	for _, entry := range logger.entries {
		if entry.message == message {
			count++
		}
	}
	return count
}

func TestDiskBackpressureClampFailsOpenOnStatError(t *testing.T) {
	// A disk the daemon cannot measure must never wedge the scheduler.
	stubDiskUsage(t, 0, errors.New("statfs blew up"))
	cfg := backpressureConfig(true, "/wt", 85, 93)
	if got := diskBackpressureClamp(7, cfg, nil); got != 7 {
		t.Fatalf("a stat error must fail open; got %d", got)
	}
}

func TestDiskBackpressureClampFailsOpenOnUnsupportedPlatform(t *testing.T) {
	stubDiskUsage(t, 0, disk.ErrUnsupported)
	cfg := backpressureConfig(true, "/wt", 85, 93)
	if got := diskBackpressureClamp(7, cfg, nil); got != 7 {
		t.Fatalf("unsupported platform must fail open; got %d", got)
	}
}

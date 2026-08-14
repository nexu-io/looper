package config

import (
	"strings"
	"testing"
)

func TestPiAndOmpVendorValidation(t *testing.T) {
	for _, vendor := range []AgentVendor{AgentVendorPi, AgentVendorOmp} {
		t.Run(string(vendor), func(t *testing.T) {
			cfg, err := DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			cfg.Daemon.LogDir = t.TempDir()
			cfg.Daemon.WorkingDirectory = t.TempDir()
			v := vendor
			cfg.Agent.Vendor = &v
			if err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
				t.Fatalf("ValidateWithOptions() error = %v", err)
			}
		})
	}

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Daemon.LogDir = t.TempDir()
	cfg.Daemon.WorkingDirectory = t.TempDir()
	invalid := AgentVendor("invalid")
	cfg.Agent.Vendor = &invalid
	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "pi") || !strings.Contains(err.Error(), "omp") {
		t.Fatalf("invalid vendor error = %v, want pi and omp in supported-vendor copy", err)
	}
}

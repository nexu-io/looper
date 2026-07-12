package config

import (
	"strings"
	"testing"
)

func TestGrokBuildVendorValidation(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Daemon.LogDir = t.TempDir()
	cfg.Daemon.WorkingDirectory = t.TempDir()
	vendor := AgentVendorGrokBuild
	cfg.Agent.Vendor = &vendor
	if err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
		t.Fatalf("ValidateWithOptions() error = %v", err)
	}

	invalid := AgentVendor("invalid")
	cfg.Agent.Vendor = &invalid
	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "grok-build") {
		t.Fatalf("invalid vendor error = %v, want grok-build in supported-vendor copy", err)
	}
}

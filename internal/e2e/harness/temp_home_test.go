package harness

import (
	"os"
	"testing"
)

func TestNewTempHomeCreatesIsolatedRuntimePaths(t *testing.T) {
	home := NewTempHome(t)
	for _, path := range []string{home.HomeDir, home.LooperHome, home.ArtifactsDir, home.LogDir, home.BackupDir, home.WorktreeRoot, home.WorkingDir} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("expected directory %s: info=%v err=%v", path, info, err)
		}
	}
	if got := home.EnvMap()["HOME"]; got != home.HomeDir {
		t.Fatalf("HOME env = %q, want %q", got, home.HomeDir)
	}
}

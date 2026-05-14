package harness

import (
	"os"
	"path/filepath"
	"testing"
)

type TempHome struct {
	Root         string
	HomeDir      string
	LooperHome   string
	ArtifactsDir string
	LogDir       string
	BackupDir    string
	WorktreeRoot string
	WorkingDir   string
	DBPath       string
	ConfigPath   string
}

func NewTempHome(tb testing.TB) TempHome {
	tb.Helper()
	root := artifactTempDir(tb, "temp-home")
	homeDir := filepath.Join(root, "home")
	looperHome := filepath.Join(homeDir, ".looper")
	artifactsDir := filepath.Join(root, "artifacts")
	logDir := filepath.Join(looperHome, "logs")
	backupDir := filepath.Join(looperHome, "backups")
	worktreeRoot := filepath.Join(looperHome, "worktrees")
	workingDir := filepath.Join(root, "working")
	paths := []string{homeDir, looperHome, artifactsDir, logDir, backupDir, worktreeRoot, workingDir}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			tb.Fatalf("mkdir %s: %v", path, err)
		}
	}
	return TempHome{Root: root, HomeDir: homeDir, LooperHome: looperHome, ArtifactsDir: artifactsDir, LogDir: logDir, BackupDir: backupDir, WorktreeRoot: worktreeRoot, WorkingDir: workingDir, DBPath: filepath.Join(looperHome, "looper.sqlite"), ConfigPath: filepath.Join(looperHome, "config.json")}
}

func (h TempHome) EnvMap() map[string]string { return map[string]string{"HOME": h.HomeDir} }
func (h TempHome) EnvSlice() []string        { return []string{"HOME=" + h.HomeDir} }

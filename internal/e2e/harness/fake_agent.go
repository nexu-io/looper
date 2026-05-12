package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

const (
	envFakeAgentMode        = "LOOPER_E2E_FAKE_AGENT_MODE"
	envFakeAgentArtifactDir = "LOOPER_E2E_FAKE_AGENT_ARTIFACT_DIR"
	envFakeAgentStatePath   = "LOOPER_E2E_FAKE_AGENT_STATE_PATH"
	envFakeAgentWriteFile   = "LOOPER_E2E_FAKE_AGENT_WRITE_FILE"
	envFakeAgentModifyFile  = "LOOPER_E2E_FAKE_AGENT_MODIFY_FILE"
	envFakeAgentSleepMS     = "LOOPER_E2E_FAKE_AGENT_SLEEP_MS"
	envFakeAgentGitPath     = "LOOPER_E2E_FAKE_AGENT_GIT_PATH"
)

type FakeAgent struct {
	Path        string
	ArtifactDir string
	StatePath   string
}

func NewFakeAgent(tb testing.TB, bins BuiltBinaries) FakeAgent {
	tb.Helper()
	artifactDir := filepath.Join(artifactTempDir(tb, "fake-agent"), "fake-agent")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		tb.Fatalf("mkdir fake agent artifact dir: %v", err)
	}
	statePath := filepath.Join(artifactDir, "state.json")
	return FakeAgent{Path: bins.FakeAgentPath, ArtifactDir: artifactDir, StatePath: statePath}
}

func (f FakeAgent) AgentConfig(mode string, gitPath string) (*config.AgentVendor, string, map[string]string) {
	vendor := config.AgentVendorCodex
	env := map[string]string{
		envFakeAgentMode:        mode,
		envFakeAgentArtifactDir: f.ArtifactDir,
		envFakeAgentStatePath:   f.StatePath,
	}
	if gitPath != "" {
		env[envFakeAgentGitPath] = gitPath
	}
	return &vendor, f.Path, env
}

func (f FakeAgent) EvidencePath() string {
	return filepath.Join(f.ArtifactDir, "cwd-evidence.json")
}

package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeAgentWritesEvidenceAndCompletion(t *testing.T) {
	bins := MustBinaries(t)
	agent := NewFakeAgent(t, bins)
	workDir := t.TempDir()
	cmd := exec.Command(agent.Path, "exec", "prompt")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"LOOPER_COMPLETION_MARKER=__LOOPER_RESULT__=",
		envFakeAgentMode+"=write-file",
		envFakeAgentArtifactDir+"="+agent.ArtifactDir,
		envFakeAgentStatePath+"="+agent.StatePath,
		envFakeAgentWriteFile+"=nested/output.txt",
	)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run fake agent: %v", err)
	}
	if !strings.Contains(string(output), `__LOOPER_RESULT__={"changedFiles":["nested/output.txt"],"summary":"fake agent wrote file"}`) &&
		!strings.Contains(string(output), `__LOOPER_RESULT__={"summary":"fake agent wrote file","changedFiles":["nested/output.txt"]}`) {
		t.Fatalf("fake agent output = %q, want completion marker", string(output))
	}
	evidence := LoadCWDEvidence(t, agent.EvidencePath())
	if mustEvalPath(t, evidence.CWD) != mustEvalPath(t, workDir) {
		t.Fatalf("evidence cwd = %q, want %q", evidence.CWD, workDir)
	}
	if _, err := os.Stat(filepath.Join(workDir, "nested", "output.txt")); err != nil {
		t.Fatalf("expected fake agent output file: %v", err)
	}
}

func TestFakeAgentTransientFailure(t *testing.T) {
	bins := MustBinaries(t)
	agent := NewFakeAgent(t, bins)
	cmd := exec.Command(agent.Path)
	cmd.Env = append(os.Environ(),
		envFakeAgentMode+"=transient-failure",
		envFakeAgentArtifactDir+"="+agent.ArtifactDir,
		envFakeAgentStatePath+"="+agent.StatePath,
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected first transient-failure run to fail")
	}
	cmd = exec.Command(agent.Path)
	cmd.Env = append(os.Environ(),
		envFakeAgentMode+"=transient-failure",
		envFakeAgentArtifactDir+"="+agent.ArtifactDir,
		envFakeAgentStatePath+"="+agent.StatePath,
		"LOOPER_COMPLETION_MARKER=__LOOPER_RESULT__=",
	)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("second transient-failure run: %v", err)
	}
	if !strings.Contains(string(output), "fake agent recovered") {
		t.Fatalf("second run output = %q, want recovery completion", string(output))
	}
}

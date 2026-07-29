package worker

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestBugModePromptForKindBug(t *testing.T) {
	cfg := config.Config{}
	// kind/bug → reproduce → locate → fix instruction present
	bug := workerInput{Title: "crash on login", Repo: "o/r", BaseBranch: "main", ExecutionMode: "create-pr", Labels: []string{"kind/bug", "looper:worker-ready"}}
	prompt, _, err := buildWorkerPromptWithInstructions("/tmp", "proj", cfg, bug, nil, false, config.DefaultDisclosureConfig(), "codex", "gpt-5.4")
	if err != nil {
		t.Fatalf("build error = %v", err)
	}
	if !strings.Contains(prompt, "This is a BUG") || !strings.Contains(prompt, "REPRODUCE") || !strings.Contains(prompt, "ROOT CAUSE") {
		t.Fatalf("bug prompt missing reproduce/root-cause steering:\n%s", prompt)
	}
	// a feature → no bug-mode block
	feat := workerInput{Title: "add export", Repo: "o/r", BaseBranch: "main", ExecutionMode: "create-pr", Labels: []string{"kind/feature"}}
	fp, _, _ := buildWorkerPromptWithInstructions("/tmp", "proj", cfg, feat, nil, false, config.DefaultDisclosureConfig(), "codex", "gpt-5.4")
	if strings.Contains(fp, "This is a BUG") {
		t.Fatalf("feature prompt should not have bug-mode block:\n%s", fp)
	}
}

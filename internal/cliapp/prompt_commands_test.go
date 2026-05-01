package cliapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptPreviewOmitsRepositoryContextForNonPlannerRoles(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoPath, "AGENTS.md"), []byte("repo instructions"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	configPath := writeEditableCLIConfigWithPayload(t, promptPreviewConfigPayload(repoPath, true))

	exitCode, stdout, stderr := runApp(t, "prompt", "preview", "--json", "--project", "project_1", "--role", "fixer", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(prompt preview fixer) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("unmarshal prompt preview: %v\nraw=%q", err, stdout)
	}
	prompt, _ := decoded["prompt"].(string)
	if strings.Contains(prompt, "Repository context / AGENTS.md") || strings.Contains(prompt, "repo instructions") {
		t.Fatalf("fixer prompt preview included planner-only repository context:\n%s", prompt)
	}
	for _, entry := range decoded["order"].([]any) {
		if entry == "repository context / AGENTS.md" {
			t.Fatalf("fixer preview order included planner-only repository context: %#v", decoded["order"])
		}
	}
}

func TestPromptPreviewIncludesRepositoryContextForPlanner(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoPath, "AGENTS.md"), []byte("repo instructions"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	configPath := writeEditableCLIConfigWithPayload(t, promptPreviewConfigPayload(repoPath, true))

	exitCode, stdout, stderr := runApp(t, "prompt", "preview", "--project", "project_1", "--role", "planner", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(prompt preview planner) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "Repository context / AGENTS.md") || !strings.Contains(stdout, "repo instructions") {
		t.Fatalf("planner prompt preview omitted AGENTS.md context:\n%s", stdout)
	}
}

func TestPromptPreviewLifecycleReflectsDisabledRemoteActions(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, promptPreviewConfigPayload(t.TempDir(), false))

	exitCode, stdout, stderr := runApp(t, "prompt", "preview", "--project", "project_1", "--role", "fixer", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(prompt preview fixer no remote) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	for _, want := range []string{"remote actions disabled by Looper configuration", "expectPush=false", "expectPR=false"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("prompt preview = %q, want to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "Commit and push") || strings.Contains(stdout, "expectPush=true") {
		t.Fatalf("prompt preview advertised enabled remote actions despite defaults.allowAutoPush=false:\n%s", stdout)
	}
}

func TestPromptPreviewFixerLifecycleMatchesRuntimeBranchFields(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, promptPreviewConfigPayload(t.TempDir(), true))

	exitCode, stdout, stderr := runApp(t, "prompt", "preview", "--project", "project_1", "--role", "fixer", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(prompt preview fixer) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	for _, want := range []string{"branch=\"\"", "baseBranch=\"\"", "expectPush=true", "expectPR=false"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("prompt preview = %q, want to contain %q", stdout, want)
		}
	}
	for _, notWant := range []string{"branch=\"<branch>\"", "baseBranch=\"main\""} {
		if strings.Contains(stdout, notWant) {
			t.Fatalf("fixer prompt preview used lifecycle fields that runtime fixer prompt does not use %q:\n%s", notWant, stdout)
		}
	}
}

func TestPromptPreviewWorkerHonorsManualOpenPRStrategy(t *testing.T) {
	t.Parallel()

	payload := promptPreviewConfigPayload(t.TempDir(), true)
	payload["defaults"].(map[string]any)["openPrStrategy"] = "manual"
	configPath := writeEditableCLIConfigWithPayload(t, payload)

	exitCode, stdout, stderr := runApp(t, "prompt", "preview", "--project", "project_1", "--role", "worker", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(prompt preview worker manual openPrStrategy) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	for _, want := range []string{"remote actions disabled by Looper configuration", "expectPush=false", "expectPR=false"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("prompt preview = %q, want to contain %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "expectPush=true") || strings.Contains(stdout, "expectPR=true") {
		t.Fatalf("worker prompt preview advertised remote actions despite defaults.openPrStrategy=manual:\n%s", stdout)
	}
}

func TestPromptPreviewReviewerUsesReviewSubmitContract(t *testing.T) {
	t.Parallel()

	payload := promptPreviewConfigPayload(t.TempDir(), true)
	payload["tools"] = map[string]any{"looperPath": "/opt/looper/bin/looper"}
	configPath := writeEditableCLIConfigWithPayload(t, payload)

	exitCode, stdout, stderr := runApp(t, "prompt", "preview", "--project", "project_1", "--role", "reviewer", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(prompt preview reviewer) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	for _, want := range []string{
		"trusted `looper review submit` wrapper",
		"GitHub operation contract: submit exactly one PR review",
		"looper:review id=... head=... outcome=clean|actionable",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("prompt preview = %q, want to contain %q", stdout, want)
		}
	}
	for _, notWant := range []string{"git_pr_lifecycle", "commit only relevant non-secret changes", "create or adopt an open pull request"} {
		if strings.Contains(stdout, notWant) {
			t.Fatalf("reviewer prompt preview included generic git/PR lifecycle text %q:\n%s", notWant, stdout)
		}
	}
}

func TestPromptPreviewReviewerReflectsMissingTrustedWrapper(t *testing.T) {
	t.Parallel()

	configPath := writeEditableCLIConfigWithPayload(t, promptPreviewConfigPayload(t.TempDir(), true))
	missingLookPath := func(string) (string, error) { return "", os.ErrNotExist }

	exitCode, stdout, stderr := runAppWithLookPath(t, missingLookPath, "prompt", "preview", "--project", "project_1", "--role", "reviewer", "--config", configPath)
	if exitCode != 0 {
		t.Fatalf("Run(prompt preview reviewer missing looper) exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	for _, want := range []string{
		"trusted Looper CLI path was not detected",
		"trusted looper review submit wrapper unavailable",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("prompt preview = %q, want to contain %q", stdout, want)
		}
	}
	for _, notWant := range []string{
		"Use Looper's trusted `looper review submit` wrapper",
		"submit exactly one PR review for the run through the trusted Looper CLI review-submit wrapper",
	} {
		if strings.Contains(stdout, notWant) {
			t.Fatalf("reviewer prompt preview advertised available submit wrapper %q despite missing looper path:\n%s", notWant, stdout)
		}
	}
}

func promptPreviewConfigPayload(repoPath string, allowAutoPush bool) map[string]any {
	return map[string]any{
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
		"defaults": map[string]any{
			"allowAutoPush": allowAutoPush,
		},
		"projects": []map[string]any{
			{
				"id":       "project_1",
				"name":     "Project",
				"repoPath": repoPath,
			},
		},
	}
}

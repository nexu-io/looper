package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsInvalidRoutedProjectPrerequisites(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Planner.AutoDiscovery = true
	cfg.Roles.Fixer.AutoDiscovery = true
	cfg.Projects = []ProjectRefConfig{{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), Network: ProjectNetworkConfig{Mode: NetworkModeRouted}}}

	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want routed validation failure")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("err = %T, want *ConfigValidationError", err)
	}
	joined := validationErr.Error()
	if !strings.Contains(joined, "config validation failed") {
		t.Fatalf("error = %q, want validation prefix", joined)
	}
	if len(validationErr.Issues) < 4 {
		t.Fatalf("issues = %#v, want multiple routed prerequisite failures", validationErr.Issues)
	}
}

func TestValidateAcceptsRoutedProjectWithExplicitNetworkAndDisabledPlannerFixer(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Planner.AutoDiscovery = false
	cfg.Roles.Fixer.AutoDiscovery = false
	cfg.Network = NetworkConfig{Enrolled: true, LoopernetBaseURL: "https://loopernet.example.com", NodeName: "red", GitHubLogin: "worker", GitHubUserID: 42}
	cfg.Projects = []ProjectRefConfig{{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), Network: ProjectNetworkConfig{Mode: NetworkModeRouted}}}

	if err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
		t.Fatalf("ValidateWithOptions() error = %v", err)
	}
}

func TestValidateReportsInvalidProjectNetworkModeOnce(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []ProjectRefConfig{{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), Network: ProjectNetworkConfig{Mode: NetworkMode("invalid")}}}

	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want invalid network mode failure")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("err = %T, want *ConfigValidationError", err)
	}
	count := 0
	for _, issue := range validationErr.Issues {
		if issue.Path == "projects[0].network.mode" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("network.mode issue count = %d, want 1; issues=%#v", count, validationErr.Issues)
	}
}

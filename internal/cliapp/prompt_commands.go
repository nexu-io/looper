package cliapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/powerformer/looper/internal/agent"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/lifecycle"
	"github.com/spf13/cobra"
)

func (r *commandRuntime) promptPreview(cmd *cobra.Command, args []string) error {
	projectID := strings.TrimSpace(getStringFlag(cmd, "project"))
	role := strings.TrimSpace(getStringFlag(cmd, "role"))
	if projectID == "" {
		return fmt.Errorf("--project is required")
	}
	if role == "" {
		return fmt.Errorf("--role is required")
	}
	if !isPreviewInstructionRole(role) {
		return fmt.Errorf("--role must be one of: planner, worker, reviewer, fixer")
	}
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	project, err := configuredProjectByID(loaded.Config.Projects, projectID)
	if err != nil {
		return err
	}
	block := config.BuildCustomInstructionBlock(loaded.Config, projectID, role)
	sections := []string{
		"Looper base role prompt\n" + previewBaseRole(role),
	}
	if repoContext := previewRepositoryContext(project.RepoPath); repoContext != "" {
		sections = append(sections, "Repository context / AGENTS.md\n"+repoContext)
	}
	if block.Text != "" {
		sections = append(sections, previewInstructionSources(block)+"\n\n"+block.Text)
	} else {
		sections = append(sections, "Custom instructions\nSources: none\n(no custom instructions applied)")
	}
	sections = append(sections,
		"Lifecycle / safety constraints\n"+previewLifecycleSafety(role, project, loaded.Config),
		"Completion / output contract\n"+agent.AppendCompletionInstruction("<role prompt assembled above>"),
	)
	prompt := strings.Join(sections, "\n\n---\n\n")
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"project": projectID, "role": role, "order": []string{"Looper base role prompt", "repository context / AGENTS.md", "custom instructions", "lifecycle / safety constraints", "completion / output contract"}, "customInstructions": block, "prompt": prompt})
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), prompt)
	return err
}

func isPreviewInstructionRole(role string) bool {
	switch role {
	case "planner", "worker", "reviewer", "fixer":
		return true
	default:
		return false
	}
}

func previewInstructionSources(block config.CustomInstructionBlock) string {
	if len(block.Sources) == 0 {
		return "Custom instruction sources: none"
	}
	lines := []string{"Custom instruction sources:"}
	for _, source := range block.Sources {
		lines = append(lines, fmt.Sprintf("- %s: %s", source.Kind, source.Path))
	}
	return strings.Join(lines, "\n")
}

func previewLifecycleSafety(role string, project config.ProjectRefConfig, cfg config.Config) string {
	branch := "<branch>"
	baseBranch := promptBaseBranch(project.BaseBranch, cfg.Defaults.BaseBranch)
	switch role {
	case "reviewer":
		return "Use Looper's trusted `looper review submit` wrapper for review submission. Do not bypass approval, publication, or disclosure policy.\n\n" + lifecycle.PromptInstruction(role, branch, baseBranch, true, true, cfg.Disclosure, promptDerefString(cfg.Agent.Model))
	case "fixer":
		return "Only repair Looper-provided fix items; do not change remote pull request state unless lifecycle policy allows it.\n\n" + lifecycle.PromptInstruction(role, branch, baseBranch, true, false, cfg.Disclosure, promptDerefString(cfg.Agent.Model))
	default:
		return lifecycle.PromptInstruction(role, branch, baseBranch, true, true, cfg.Disclosure, promptDerefString(cfg.Agent.Model))
	}
}

func configuredProjectByID(projects []config.ProjectRefConfig, id string) (config.ProjectRefConfig, error) {
	for _, project := range projects {
		if project.ID == id {
			return project, nil
		}
	}
	return config.ProjectRefConfig{}, fmt.Errorf("project not found: %s", id)
}

func previewBaseRole(role string) string {
	switch role {
	case "planner":
		return "Write a planning spec for the target GitHub issue."
	case "worker":
		return "Implement the requested work and prepare a pull request when policy allows."
	case "reviewer":
		return "Review the target pull request through Looper's trusted review workflow."
	case "fixer":
		return "Repair only the Looper-provided review findings."
	default:
		return "Unknown role; config validation accepts planner, worker, reviewer, and fixer."
	}
}

func previewRepositoryContext(repoPath string) string {
	if strings.TrimSpace(repoPath) == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(repoPath, "AGENTS.md"))
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		return "Repository path: " + repoPath
	}
	return "Repository path: " + repoPath + "\n\nAGENTS.md:\n" + strings.TrimSpace(string(raw))
}

func promptBaseBranch(projectBase *string, defaultBase string) string {
	if projectBase != nil && strings.TrimSpace(*projectBase) != "" {
		return *projectBase
	}
	return defaultBase
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func promptDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

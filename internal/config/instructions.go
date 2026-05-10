package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type CustomInstructionSource struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type CustomInstructionBlock struct {
	Enabled     bool                      `json:"enabled"`
	Role        string                    `json:"role"`
	ProjectID   string                    `json:"projectId,omitempty"`
	Sources     []CustomInstructionSource `json:"sources"`
	Text        string                    `json:"-"`
	PromptHash  string                    `json:"promptHash,omitempty"`
	PromptBytes int                       `json:"promptBytes"`
}

func BuildCustomInstructionBlock(cfg Config, projectID, role string) CustomInstructionBlock {
	block := CustomInstructionBlock{Enabled: cfg.Instructions.Enabled, Role: role, ProjectID: projectID, Sources: []CustomInstructionSource{}}
	if !cfg.Instructions.Enabled || strings.TrimSpace(role) == "" {
		return block
	}
	sections := make([]string, 0, 3)
	globalInstructions := strings.TrimSpace(roleInstructionText(cfg.Roles, role))
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil || !projectRoleInstructionsConfigured(project.Roles, role) {
		if globalInstructions == "" {
			// No global role instructions are configured for this role.
		} else {
			sections = append(sections, fmt.Sprintf("Global %s instructions:\n%s", role, globalInstructions))
			block.Sources = append(block.Sources, CustomInstructionSource{Kind: "global-role", Path: "roles." + role + ".instructions"})
		}
	}
	if project != nil {
		if projectRoleInstructionsConfigured(project.Roles, role) {
			if text := strings.TrimSpace(roleInstructionText(ProjectRoleConfigs(cfg, projectID), role)); text != "" {
				sections = append(sections, fmt.Sprintf("Project %s %s role instructions:\n%s", project.ID, role, text))
				block.Sources = append(block.Sources, CustomInstructionSource{Kind: "project-role", Path: "projects." + project.ID + ".roles." + role + ".instructions"})
			}
		}
		if text := strings.TrimSpace(project.Instructions[role]); text != "" {
			sections = append(sections, fmt.Sprintf("Project %s %s instructions:\n%s", project.ID, role, text))
			block.Sources = append(block.Sources, CustomInstructionSource{Kind: "project-role", Path: "projects." + project.ID + ".instructions." + role})
		}
	}
	if len(sections) == 0 {
		return block
	}
	block.Text = "Custom instructions (supplemental, lower priority than Looper lifecycle, safety, disclosure, and output contracts):\n" + strings.Join(sections, "\n\n")
	block.PromptBytes = len([]byte(block.Text))
	sum := sha256.Sum256([]byte(block.Text))
	block.PromptHash = hex.EncodeToString(sum[:])
	return block
}

func projectRoleInstructionsConfigured(roles *PartialRoleConfigs, role string) bool {
	if roles == nil {
		return false
	}
	switch role {
	case "planner":
		return roles.Planner != nil && roles.Planner.Instructions != nil
	case "worker":
		return roles.Worker != nil && roles.Worker.Instructions != nil
	case "reviewer":
		return roles.Reviewer != nil && roles.Reviewer.Instructions != nil
	case "fixer":
		return roles.Fixer != nil && roles.Fixer.Instructions != nil
	case "sweeper":
		return roles.Sweeper != nil && roles.Sweeper.Instructions != nil
	default:
		return false
	}
}

func CustomInstructionMetadata(block CustomInstructionBlock, finalPrompt string) map[string]any {
	sum := sha256.Sum256([]byte(finalPrompt))
	return map[string]any{
		"customInstructionsEnabled":   block.Enabled,
		"customInstructionSources":    block.Sources,
		"customInstructionPromptHash": block.PromptHash,
		"promptHash":                  hex.EncodeToString(sum[:]),
		"promptBytes":                 len([]byte(finalPrompt)),
	}
}

func findConfiguredProject(projects []ProjectRefConfig, projectID string) *ProjectRefConfig {
	for index := range projects {
		if projects[index].ID == projectID {
			return &projects[index]
		}
	}
	return nil
}

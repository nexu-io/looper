package config

// ProjectRoleConfigs returns the effective role configuration for a project.
// Global role configuration is used as the base and projects[id].roles may
// override supported role fields. Unknown project IDs fall back to global roles.
//
// Project-level agent bindings are never applied (ADR-0012): agent vendor/model
// resolution is global-only. Even if a project partial carries agent fields in
// memory, they are stripped before merge.
func ProjectRoleConfigs(cfg Config, projectID string) RoleConfigs {
	roles := cfg.Roles
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil {
		return roles
	}
	if project.Roles != nil {
		stripped := stripRoleAgentBindings(*project.Roles)
		mergeRoleConfigs(&roles, stripped)
	}
	return roles
}

// stripRoleAgentBindings returns a copy of partial with Agent nilled on coding roles.
func stripRoleAgentBindings(partial PartialRoleConfigs) PartialRoleConfigs {
	stripped := partial
	if partial.Planner != nil {
		planner := *partial.Planner
		planner.Agent = nil
		stripped.Planner = &planner
	}
	if partial.Worker != nil {
		worker := *partial.Worker
		worker.Agent = nil
		stripped.Worker = &worker
	}
	if partial.Reviewer != nil {
		reviewer := *partial.Reviewer
		reviewer.Agent = nil
		stripped.Reviewer = &reviewer
	}
	if partial.Fixer != nil {
		fixer := *partial.Fixer
		fixer.Agent = nil
		stripped.Fixer = &fixer
	}
	return stripped
}

// ProjectProviderKind resolves the task-source provider kind for a project by
// id (github / forgejo / plane). Unknown ids fall back to the GitHub default.
func ProjectProviderKind(cfg Config, projectID string) ProviderKind {
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil {
		return ProviderKindGitHub
	}
	return resolvedProjectProviderKind(cfg, *project)
}

func ProjectRoleAutoDiscoveryEnabled(cfg Config, projectID, role string) bool {
	roles := ProjectRoleConfigs(cfg, projectID)
	switch role {
	case "coordinator":
		return roles.Coordinator.Enabled
	case "planner":
		return roles.Planner.AutoDiscovery
	case "reviewer":
		return roles.Reviewer.Discovery.AutoDiscovery
	case "fixer":
		return roles.Fixer.AutoDiscovery
	case "worker":
		return roles.Worker.AutoDiscovery
	default:
		return false
	}
}

func AnyProjectRoleAutoDiscoveryEnabled(cfg Config, role string) bool {
	if ProjectRoleAutoDiscoveryEnabled(cfg, "", role) {
		return true
	}
	for _, project := range cfg.Projects {
		if ProjectRoleAutoDiscoveryEnabled(cfg, project.ID, role) {
			return true
		}
	}
	return false
}

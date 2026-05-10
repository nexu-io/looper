package config

// ProjectRoleConfigs returns the effective role configuration for a project.
// Global role configuration is used as the base and projects[id].roles may
// override supported role fields. Unknown project IDs fall back to global roles.
func ProjectRoleConfigs(cfg Config, projectID string) RoleConfigs {
	roles := cfg.Roles
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil {
		return roles
	}
	if project.Roles != nil {
		mergeRoleConfigs(&roles, *project.Roles)
	}
	return roles
}

func ProjectRoleAutoDiscoveryEnabled(cfg Config, projectID, role string) bool {
	roles := ProjectRoleConfigs(cfg, projectID)
	switch role {
	case "planner":
		return roles.Planner.AutoDiscovery
	case "reviewer":
		return roles.Reviewer.AutoDiscovery
	case "fixer":
		return roles.Fixer.AutoDiscovery
	case "worker":
		return roles.Worker.AutoDiscovery
	case "sweeper":
		return roles.Sweeper.AutoDiscovery
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

package config

func collectMixedSchemaWarnings(partial PartialConfig) []string {
	deprecated := []deprecatedSurface{}
	seen := map[string]struct{}{}
	add := func(path string, replacement string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		deprecated = append(deprecated, deprecatedSurface{kind: deprecatedSurfaceConfigPath, legacy: path, replacement: replacement})
	}

	if partial.LegacyReviewer != nil {
		add("reviewer", "roles.reviewer.behavior")
	}
	if partial.Defaults != nil {
		if partial.Defaults.AllowAutoApprove != nil {
			add("defaults.allowAutoApprove", "roles.reviewer.behavior.reviewEvents.clean")
		}
		if partial.Defaults.FixAllPullRequests != nil {
			add("defaults.fixAllPullRequests", "roles.fixer.triggers.authorFilter")
		}
	}
	if partial.Roles != nil && partial.Roles.Reviewer != nil {
		reviewer := partial.Roles.Reviewer
		if reviewer.AutoDiscovery != nil {
			add("roles.reviewer.autoDiscovery", "roles.reviewer.discovery.autoDiscovery")
		}
		if reviewer.Triggers != nil {
			add("roles.reviewer.triggers", "roles.reviewer.discovery.triggers")
		}
		if reviewer.SpecReview != nil {
			add("roles.reviewer.specReview", "roles.reviewer.discovery.specReview")
		}
	}
	if partial.Projects != nil {
		for _, project := range *partial.Projects {
			if len(project.Instructions) > 0 {
				add("projects[].instructions", "projects[].roles.<role>.instructions")
			}
			if project.Roles != nil && project.Roles.Reviewer != nil {
				reviewer := project.Roles.Reviewer
				if reviewer.AutoDiscovery != nil {
					add("projects[].roles.reviewer.autoDiscovery", "projects[].roles.reviewer.discovery.autoDiscovery")
				}
				if reviewer.Triggers != nil {
					add("projects[].roles.reviewer.triggers", "projects[].roles.reviewer.discovery.triggers")
				}
				if reviewer.SpecReview != nil {
					add("projects[].roles.reviewer.specReview", "projects[].roles.reviewer.discovery.specReview")
				}
			}
		}
	}

	return dedupeDeprecationWarnings(deprecated)
}

func dedupeWarnings(groups ...[]string) []string {
	merged := []string{}
	seen := map[string]struct{}{}
	for _, group := range groups {
		for _, warning := range group {
			if _, ok := seen[warning]; ok {
				continue
			}
			seen[warning] = struct{}{}
			merged = append(merged, warning)
		}
	}
	return merged
}

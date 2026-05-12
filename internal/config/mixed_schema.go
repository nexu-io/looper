package config

import "fmt"

func collectMixedSchemaWarnings(partial PartialConfig) []string {
	warnings := []string{}
	seen := map[string]struct{}{}
	add := func(path string, replacement string) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		warnings = append(warnings, fmt.Sprintf("deprecated config path %q is accepted for now; use %q instead", path, replacement))
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

	return warnings
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

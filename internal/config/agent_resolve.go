package config

import "strings"

// Coding role names used by agent resolution.
const (
	CodingRolePlanner  = "planner"
	CodingRoleWorker   = "worker"
	CodingRoleReviewer = "reviewer"
	CodingRoleFixer    = "fixer"
)

// ResolvedAgent is the effective vendor/model for a coding role after overlay.
type ResolvedAgent struct {
	Vendor AgentVendor
	// Model is the post-overlay model binding:
	//   nil            — unset (params-only --model/-m may still apply)
	//   non-nil empty  — explicit suppress to vendor default (strip params model flags)
	//   non-empty      — explicit model
	Model     *string
	ProfileID string // profile selected by the role binding; empty if none
}

// ResolveAgent overlays global agent vendor/model with the role binding.
// projectID is reserved; project layers are no-op in v1 (agent is global-only).
// ok=false when role is not a coding role or vendor is unset after overlay.
func ResolveAgent(cfg Config, projectID string, role string) (ResolvedAgent, bool) {
	_ = projectID
	vendor, model, profileID, ok := overlayAgentIdentity(cfg, role)
	if !ok {
		return ResolvedAgent{}, false
	}
	if vendor == nil {
		return ResolvedAgent{ProfileID: profileID}, false
	}
	return ResolvedAgent{
		Vendor:    *vendor,
		Model:     model,
		ProfileID: profileID,
	}, true
}

// overlayAgentIdentity returns the post-overlay vendor/model/profile for a coding
// role without requiring vendor to be set. ok=false only for non-coding roles.
// Explicit empty-string model suppresses inherited model but stays a non-nil
// empty pointer so params filtering can strip --model/-m (nil means unset).
// Used by ResolveAgent and hot-reload restart guards.
func overlayAgentIdentity(cfg Config, role string) (vendor *AgentVendor, model *string, profileID string, ok bool) {
	if !isCodingRole(role) {
		return nil, nil, "", false
	}

	if cfg.Agent.Vendor != nil {
		v := *cfg.Agent.Vendor
		vendor = &v
	}
	if cfg.Agent.Model != nil {
		model = stringPtr(*cfg.Agent.Model)
	}

	binding := codingRoleAgentBinding(cfg.Roles, role)
	if binding != nil && binding.Profile != nil {
		profileID = strings.TrimSpace(*binding.Profile)
		if profileID != "" {
			if profile, ok := cfg.Agent.Profiles[profileID]; ok {
				if profile.Vendor != nil {
					v := *profile.Vendor
					vendor = &v
				}
				if profile.Model != nil {
					model = stringPtr(*profile.Model)
				}
			}
		}
	}

	if binding != nil {
		if binding.Vendor != nil {
			v := *binding.Vendor
			vendor = &v
		}
		if binding.Model != nil {
			model = stringPtr(*binding.Model)
		}
	}

	// Explicit empty string suppresses inherited model and is left as a non-nil
	// empty pointer (not collapsed to nil) so ParamsForRoleVendor can strip
	// params model flags and force the vendor default. nil means unset.
	return vendor, model, profileID, true
}

func isCodingRole(role string) bool {
	switch role {
	case CodingRolePlanner, CodingRoleWorker, CodingRoleReviewer, CodingRoleFixer:
		return true
	default:
		return false
	}
}

// AnyCodingRoleAgentConfigured returns true if any coding role resolves with a vendor.
func AnyCodingRoleAgentConfigured(cfg Config) bool {
	for _, role := range []string{CodingRolePlanner, CodingRoleWorker, CodingRoleReviewer, CodingRoleFixer} {
		if CodingRoleAgentConfigured(cfg, role) {
			return true
		}
	}
	return false
}

// CodingRoleAgentConfigured reports whether the given coding role resolves with a vendor.
func CodingRoleAgentConfigured(cfg Config, role string) bool {
	_, ok := ResolveAgent(cfg, "", role)
	return ok
}

func codingRoleAgentBinding(roles RoleConfigs, role string) *RoleAgentConfig {
	switch role {
	case CodingRolePlanner:
		return roles.Planner.Agent
	case CodingRoleWorker:
		return roles.Worker.Agent
	case CodingRoleReviewer:
		return roles.Reviewer.Agent
	case CodingRoleFixer:
		return roles.Fixer.Agent
	default:
		return nil
	}
}

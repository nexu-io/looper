package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

var hotEditablePaths = map[string]struct{}{
	"agent.vendor": {},
	"agent.model":  {},
	"agent.env":    {},
	"agent.timeouts.plannerIdleTimeoutSeconds":  {},
	"agent.timeouts.plannerMaxRuntimeSeconds":   {},
	"agent.timeouts.workerIdleTimeoutSeconds":   {},
	"agent.timeouts.workerMaxRuntimeSeconds":    {},
	"agent.timeouts.reviewerIdleTimeoutSeconds": {},
	"agent.timeouts.reviewerMaxRuntimeSeconds":  {},
	"agent.timeouts.fixerIdleTimeoutSeconds":    {},
	"agent.timeouts.fixerMaxRuntimeSeconds":     {},

	"scheduler.maxConcurrentRuns":       {},
	"scheduler.slowLaneWarnThresholdMs": {},

	"notifications.inApp":                           {},
	"notifications.osascript.enabled":               {},
	"notifications.osascript.soundForLevels":        {},
	"notifications.osascript.throttleWindowSeconds": {},
	"disclosure.enabled":                            {},
	"disclosure.includeAgent":                       {},
	"disclosure.includeOS":                          {},
	"disclosure.channels.gitCommit":                 {},
	"disclosure.channels.pullRequest":               {},
	"disclosure.channels.issueComment":              {},
	"disclosure.channels.reviewComment":             {},
	"disclosure.channels.inlineCommentVisible":      {},
	"defaults.allowAutoCommit":                      {},
	"defaults.allowAutoPush":                        {},
	"defaults.allowRiskyFixes":                      {},
	"defaults.openPrStrategy":                       {},
	"defaults.addSnapshotMode":                      {},
	"instructions.enabled":                          {},

	"roles.planner.autoDiscovery":                                          {},
	"roles.planner.triggers.labels":                                        {},
	"roles.planner.triggers.labelMode":                                     {},
	"roles.planner.triggers.requireAssigneeCurrentUser":                    {},
	"roles.planner.instructions":                                           {},
	"roles.worker.autoDiscovery":                                           {},
	"roles.worker.triggers.labels":                                         {},
	"roles.worker.triggers.labelMode":                                      {},
	"roles.worker.triggers.requireAssigneeCurrentUser":                     {},
	"roles.worker.triggers.planeAssigneeId":                                {},
	"roles.worker.instructions":                                            {},
	"roles.fixer.autoDiscovery":                                            {},
	"roles.fixer.triggers.includeDrafts":                                   {},
	"roles.fixer.triggers.authorFilter":                                    {},
	"roles.fixer.triggers.labels":                                          {},
	"roles.fixer.triggers.labelMode":                                       {},
	"roles.fixer.instructions":                                             {},
	"roles.reviewer.discovery.autoDiscovery":                               {},
	"roles.reviewer.discovery.triggers.includeDrafts":                      {},
	"roles.reviewer.discovery.triggers.requireReviewRequest":               {},
	"roles.reviewer.discovery.triggers.enableSelfReview":                   {},
	"roles.reviewer.discovery.triggers.labels":                             {},
	"roles.reviewer.discovery.triggers.labelMode":                          {},
	"roles.reviewer.discovery.specReview.includeReviewingLabel":            {},
	"roles.reviewer.discovery.specReview.reviewingLabel":                   {},
	"roles.reviewer.behavior.loop.enabledByDefault":                        {},
	"roles.reviewer.behavior.loop.maxIterationsPerPR":                      {},
	"roles.reviewer.behavior.loop.maxIterationsPerHead":                    {},
	"roles.reviewer.behavior.loop.maxWallClockSeconds":                     {},
	"roles.reviewer.behavior.loop.maxConsecutiveFailures":                  {},
	"roles.reviewer.behavior.loop.maxAgentExecutionsPerPR":                 {},
	"roles.reviewer.behavior.loop.stopOnApproved":                          {},
	"roles.reviewer.behavior.loop.stopOnReadyLabel":                        {},
	"roles.reviewer.behavior.loop.stopOnIdenticalOutput":                   {},
	"roles.reviewer.behavior.retry.enhancedTransientClassification":        {},
	"roles.reviewer.behavior.retry.extraTransientErrorPatterns":            {},
	"roles.reviewer.behavior.retry.recoverExistingMatchedFailures":         {},
	"roles.reviewer.behavior.retry.autoRecoveryMaxAttempts":                {},
	"roles.reviewer.behavior.scope":                                        {},
	"roles.reviewer.behavior.publishMode":                                  {},
	"roles.reviewer.behavior.reviewEvents.clean":                           {},
	"roles.reviewer.behavior.reviewEvents.blocking":                        {},
	"roles.reviewer.behavior.detectDuplicateFindings":                      {},
	"roles.reviewer.behavior.nativeResume.onHeadChange":                    {},
	"roles.reviewer.behavior.nativeResume.reReviewPromptOnHeadChange":      {},
	"roles.reviewer.behavior.threadResolution.enabled":                     {},
	"roles.reviewer.behavior.threadResolution.mode":                        {},
	"roles.reviewer.behavior.threadResolution.scope":                       {},
	"roles.reviewer.behavior.threadResolution.autoResolve":                 {},
	"roles.reviewer.behavior.threadResolution.requireAuditComment":         {},
	"roles.reviewer.behavior.threadResolution.requireNewHeadSinceThread":   {},
	"roles.reviewer.behavior.threadResolution.requireCurrentReviewRequest": {},
	"roles.reviewer.behavior.threadResolution.maxThreadsPerRun":            {},
	"roles.reviewer.instructions":                                          {},
	"roles.coordinator.pollInterval":                                       {},
	"roles.coordinator.triage.triagedLabel":                                {},
	"roles.coordinator.triage.maxIssueAgeDays":                             {},
	"roles.coordinator.triage.maxPerTick":                                  {},
	"roles.coordinator.triage.disposition.outOfScopeLabel":                 {},
	"roles.coordinator.triage.disposition.unclearLabel":                    {},
	"roles.coordinator.triage.disposition.reTriageOnAuthorReply":           {},
	"roles.coordinator.dispatch.mode":                                      {},
	"roles.coordinator.dispatch.humanGate.slashCommands":                   {},
	"roles.coordinator.dispatch.humanGate.allowedUsers":                    {},
	"roles.coordinator.dispatch.autonomous.delayMinutes":                   {},
	"roles.coordinator.dispatch.autonomous.holdLabel":                      {},
	"roles.coordinator.dispatch.assignTo":                                  {},
	"roles.coordinator.mergeWatch.maxIndeterminateDuration":                {},

	"tools.looperPath":    {},
	"tools.osascriptPath": {},
}

// hotReloadCompatibilityPaths are deprecated representations normalized into
// curated hot fields. They are accepted by the file watcher so changing or
// removing an alias alongside its canonical field does not make that safe
// policy edit spuriously restart-bound. They remain absent from
// IsHotEditablePath, so the dashboard/API can only write canonical names.
var hotReloadCompatibilityPaths = map[string]struct{}{
	"agent.timeouts.plannerSeconds":  {},
	"agent.timeouts.workerSeconds":   {},
	"agent.timeouts.reviewerSeconds": {},
	"agent.timeouts.fixerSeconds":    {},
	"defaults.allowAutoApprove":      {},
	"defaults.fixAllPullRequests":    {},
}

// IsHotEditablePath reports whether path belongs to the deliberately small
// configuration surface that can change without replacing process-owned
// resources. Paths use canonical JSON field names joined with dots.
func IsHotEditablePath(path string) bool {
	if path == "" || path != strings.TrimSpace(path) || hasEmptyPathSegment(path) {
		return false
	}

	if _, ok := hotEditablePaths[path]; ok {
		return true
	}
	if strings.HasPrefix(path, "agent.env.") && len(strings.TrimPrefix(path, "agent.env.")) > 0 {
		return true
	}
	if isHotAgentProfilePath(path) {
		return true
	}
	if isHotRoleAgentPath(path) {
		return true
	}
	return false
}

// isHotAgentProfilePath allows shape-aware leaves under agent.profiles:
//   - agent.profiles.<id>           (whole profile set/unset)
//   - agent.profiles.<id>.vendor
//   - agent.profiles.<id>.model
//
// Whole-map agent.profiles and unknown nested fields are rejected.
func isHotAgentProfilePath(path string) bool {
	segments := strings.Split(path, ".")
	if len(segments) < 3 || len(segments) > 4 {
		return false
	}
	if segments[0] != "agent" || segments[1] != "profiles" {
		return false
	}
	id := segments[2]
	if !agentProfileIDPattern.MatchString(id) {
		return false
	}
	if len(segments) == 3 {
		return true
	}
	switch segments[3] {
	case "vendor", "model":
		return true
	default:
		return false
	}
}

// isHotRoleAgentPath allows coding-role agent binding leaves:
// roles.{planner,worker,reviewer,fixer}.agent.{profile,vendor,model}
func isHotRoleAgentPath(path string) bool {
	segments := strings.Split(path, ".")
	if len(segments) != 4 {
		return false
	}
	if segments[0] != "roles" || segments[2] != "agent" {
		return false
	}
	if !isCodingRole(segments[1]) {
		return false
	}
	switch segments[3] {
	case "profile", "vendor", "model":
		return true
	default:
		return false
	}
}

func isHotReloadablePath(path string) bool {
	if IsHotEditablePath(path) {
		return true
	}
	_, ok := hotReloadCompatibilityPaths[path]
	return ok
}

// IsHotReloadCompatibilityPath reports deprecated file-only representations
// that the watcher normalizes into canonical hot fields. They are intentionally
// omitted from dashboard metadata; operators edit the canonical field instead.
func IsHotReloadCompatibilityPath(path string) bool {
	_, ok := hotReloadCompatibilityPaths[path]
	return ok
}

func hasEmptyPathSegment(path string) bool {
	for _, segment := range strings.Split(path, ".") {
		if segment == "" {
			return true
		}
	}
	return false
}

// RestartRequiredChanges returns sorted, concrete JSON paths whose effective
// values changed outside the hot-editable surface. Collection values are
// reported at their field path; object and map values are reported at their
// changed leaves.
func RestartRequiredChanges(oldConfig Config, newConfig Config) []string {
	oldValue := configJSONValue(oldConfig)
	newValue := configJSONValue(newConfig)
	changed := make([]string, 0)
	diffConfigJSON("", oldValue, newValue, &changed)

	restartRequired := make([]string, 0, len(changed))
	seen := make(map[string]struct{}, len(changed))
	for _, path := range changed {
		if path == "" || isHotReloadablePath(path) {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		restartRequired = append(restartRequired, path)
	}
	// Leaving a configured vendor is hot only when its vendor-specific companions
	// are unambiguous. Reusing a command/args map under another executable is
	// unsafe, and silently carrying the same explicit model is almost always
	// accidental. A paired model edit (including clearing it) is explicit.
	//
	// Guard the raw global agent.vendor leave/switch (coordinator triage +
	// agent.params ownership) and each coding role's *resolved* vendor
	// (global + profile + role overlay). The role loop alone is not enough:
	// when every coding role overrides vendor via profile/inline binding,
	// resolved vendors stay stable while a hot global vendor edit would still
	// rebind coordinator and params under a new CLI.
	appendResolvedVendorRestartGuards(oldConfig, newConfig, seen, &restartRequired)
	sort.Strings(restartRequired)
	return restartRequired
}

func appendResolvedVendorRestartGuards(oldConfig Config, newConfig Config, seen map[string]struct{}, restartRequired *[]string) {
	mark := func(path string) {
		if _, exists := seen[path]; exists {
			return
		}
		seen[path] = struct{}{}
		*restartRequired = append(*restartRequired, path)
	}

	// Global agent is independent of role-resolved identity: coordinator triage
	// still uses it, and agent.params fan out as owned by the global vendor.
	appendGlobalVendorLeaveSwitchGuard(oldConfig, newConfig, mark)

	for _, role := range []string{CodingRolePlanner, CodingRoleWorker, CodingRoleReviewer, CodingRoleFixer} {
		oldVendor, oldModel, _, oldOK := overlayAgentIdentity(oldConfig, role)
		if !oldOK || oldVendor == nil {
			// First activation (no prior vendor) remains hot, including prepared models.
			continue
		}
		newVendor, newModel, _, newOK := overlayAgentIdentity(newConfig, role)
		if !newOK {
			continue
		}
		if newVendor != nil && *oldVendor == *newVendor {
			continue
		}
		// Prior vendor left or changed (including unset). Global params fan out.
		if len(newConfig.Agent.Params) > 0 {
			mark("agent.params")
		}
		// Retaining the same non-empty model across a vendor leave/switch is almost
		// always accidental (and enables vendor-reset laundering: unset vendor,
		// then set a different vendor while keeping the old model). Non-nil empty
		// is explicit suppress-to-vendor-default, not a portable model value.
		// Report the binding that actually owns the retained model (role inline,
		// profile, or global) so PATCH rejection points at a field that changes
		// the resolved value — not always agent.model.
		if oldModel != nil && newModel != nil && *oldModel != "" && *oldModel == *newModel {
			if path := resolvedModelBindingPath(newConfig, role); path != "" {
				mark(path)
			}
		}
	}
}

// appendGlobalVendorLeaveSwitchGuard blocks hot leave/switch of agent.vendor when
// companion global fields would be reused under a different CLI. First activation
// (nil prior vendor) remains hot.
func appendGlobalVendorLeaveSwitchGuard(oldConfig Config, newConfig Config, mark func(string)) {
	if oldConfig.Agent.Vendor == nil {
		return
	}
	if newConfig.Agent.Vendor != nil && *oldConfig.Agent.Vendor == *newConfig.Agent.Vendor {
		return
	}
	if len(newConfig.Agent.Params) > 0 {
		mark("agent.params")
	}
	oldModel := oldConfig.Agent.Model
	newModel := newConfig.Agent.Model
	if oldModel != nil && newModel != nil && *oldModel != "" && *oldModel == *newModel {
		mark("agent.model")
	}
}

// resolvedModelBindingPath returns the config path that owns the post-overlay
// model for a coding role (last writer wins: role inline > profile > global).
// Empty when no model is bound after overlay.
func resolvedModelBindingPath(cfg Config, role string) string {
	path := ""
	if cfg.Agent.Model != nil {
		path = "agent.model"
	}
	binding := codingRoleAgentBinding(cfg.Roles, role)
	if binding != nil && binding.Profile != nil {
		profileID := strings.TrimSpace(*binding.Profile)
		if profileID != "" {
			if profile, ok := cfg.Agent.Profiles[profileID]; ok && profile.Model != nil {
				path = "agent.profiles." + profileID + ".model"
			}
		}
	}
	if binding != nil && binding.Model != nil {
		path = "roles." + role + ".agent.model"
	}
	return path
}

// CloneConfig returns a complete detached copy. Config is a JSON configuration
// model, so a non-JSON-representable value in an interface field is a
// programming error rather than a recoverable runtime condition.
func CloneConfig(source Config) Config {
	raw, err := json.Marshal(source)
	if err != nil {
		panic(fmt.Sprintf("clone config: %v", err))
	}
	var cloned Config
	if err := json.Unmarshal(raw, &cloned); err != nil {
		panic(fmt.Sprintf("clone config: %v", err))
	}
	return cloned
}

func configJSONValue(value Config) any {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("compare config: %v", err))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		panic(fmt.Sprintf("compare config: %v", err))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		panic("compare config: unexpected trailing JSON value")
	}
	return decoded
}

func diffConfigJSON(path string, oldValue any, newValue any, changed *[]string) {
	oldObject, oldIsObject := oldValue.(map[string]any)
	newObject, newIsObject := newValue.(map[string]any)
	if oldIsObject && newIsObject {
		keys := make([]string, 0, len(oldObject)+len(newObject))
		seen := make(map[string]struct{}, len(oldObject)+len(newObject))
		for key := range oldObject {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		for key := range newObject {
			if _, exists := seen[key]; exists {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			oldChild, oldExists := oldObject[key]
			newChild, newExists := newObject[key]
			childPath := joinConfigPath(path, key)
			switch {
			case !oldExists:
				appendConfigLeafPaths(childPath, newChild, changed)
			case !newExists:
				appendConfigLeafPaths(childPath, oldChild, changed)
			default:
				diffConfigJSON(childPath, oldChild, newChild, changed)
			}
		}
		return
	}

	if _, oldIsArray := oldValue.([]any); oldIsArray {
		if !reflect.DeepEqual(oldValue, newValue) {
			*changed = append(*changed, path)
		}
		return
	}
	if _, newIsArray := newValue.([]any); newIsArray {
		if !reflect.DeepEqual(oldValue, newValue) {
			*changed = append(*changed, path)
		}
		return
	}
	if !reflect.DeepEqual(oldValue, newValue) {
		*changed = append(*changed, path)
	}
}

func appendConfigLeafPaths(path string, value any, changed *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			*changed = append(*changed, path)
			return
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			appendConfigLeafPaths(joinConfigPath(path, key), typed[key], changed)
		}
	case []any:
		*changed = append(*changed, path)
	default:
		*changed = append(*changed, path)
	}
}

func joinConfigPath(parent string, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

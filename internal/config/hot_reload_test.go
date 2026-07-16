package config

import (
	"reflect"
	"testing"
)

func TestIsHotEditablePathUsesExplicitAllowlist(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"agent.vendor",
		"agent.model",
		"agent.env",
		"agent.env.OPENAI_API_KEY",
		"agent.timeouts.workerMaxRuntimeSeconds",
		"scheduler.maxConcurrentRuns",
		"scheduler.slowLaneWarnThresholdMs",
		"notifications.inApp",
		"disclosure.channels.gitCommit",
		"defaults.allowAutoPush",
		"instructions.enabled",
		"roles.worker.triggers.planeAssigneeId",
		"roles.reviewer.behavior.scope",
		"tools.looperPath",
		"tools.osascriptPath",
	}
	for _, path := range allowed {
		path := path
		t.Run("allowed_"+path, func(t *testing.T) {
			t.Parallel()
			if !IsHotEditablePath(path) {
				t.Fatalf("IsHotEditablePath(%q) = false, want true", path)
			}
		})
	}

	rejected := []string{
		"",
		" agent.model",
		"agent",
		"agent.params",
		"agent.params.temperature",
		"agent.nativeResume.enabled",
		"agent.timeouts.workerSeconds",
		"scheduler",
		"scheduler.pollIntervalSeconds",
		"scheduler.discoveryCacheTtlSeconds",
		"scheduler.retryMaxAttempts",
		"scheduler.retryBaseDelayMs",
		"tools",
		"tools.gitPath",
		"tools.ghPath",
		"server.port",
		"storage.dbPath",
		"providers",
		"projects",
		"logging.level",
		"daemon.mode",
		"package.autoUpgradeEnabled",
		"webhook.enabled",
		"network.enrolled",
		"hitl.github.mentionLogins",
		"notifications.webhook.verificationTokenEnv",
		"roles.reviewer.autoMerge.enabled",
		"roles.coordinator.enabled",
		"roles.coordinator.dependencies.enabled",
		"instructions.maxBytes",
		"defaults.allowAutoMerge",
		"defaults.baseBranch",
		"roles.reviewer.behavior.loop.minPublishIntervalSeconds",
		"roles.reviewer.behavior.loop.quietPeriodSeconds",
		"roles.reviewer.behavior.retry.maxDelayMs",
		"roles.planner.triggers.planeAssigneeId",
		"roles.coordinator.mergeWatch.transientRetries",
		"roles..planner",
	}
	for _, path := range rejected {
		path := path
		t.Run("rejected_"+path, func(t *testing.T) {
			t.Parallel()
			if IsHotEditablePath(path) {
				t.Fatalf("IsHotEditablePath(%q) = true, want false", path)
			}
		})
	}
}

func TestRestartRequiredChangesReturnsConcreteSortedPaths(t *testing.T) {
	t.Parallel()

	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	newConfig := CloneConfig(oldConfig)
	newConfig.Server.Port++
	newConfig.Scheduler.PollIntervalSeconds++
	newConfig.Scheduler.DiscoveryCacheTTLSeconds++
	newConfig.Scheduler.RetryMaxAttempts++
	newConfig.Scheduler.RetryBaseDelayMS++
	newConfig.Scheduler.MaxConcurrentRuns++
	newConfig.Agent.Model = stringPtr("hot-model")
	newConfig.Agent.Params["temperature"] = 0.5
	newConfig.Tools.GitPath = stringPtr("/restart/git")
	newConfig.Tools.LooperPath = stringPtr("/hot/looper")
	newConfig.Logging.Level = LogLevelDebug
	newConfig.Daemon.Environment["NEW_VAR"] = "value"
	newConfig.Roles.Planner.Triggers.Labels = []string{"hot-label"}
	newConfig.Roles.Planner.Triggers.PlaneAssigneeID = "restart-bound-planner-member"
	newConfig.Roles.Worker.Triggers.PlaneAssigneeID = "hot-worker-member"
	newConfig.Defaults.BaseBranch = "develop"
	newConfig.Roles.Coordinator.Enabled = !newConfig.Roles.Coordinator.Enabled
	newConfig.Providers = append(newConfig.Providers, ProviderConfig{ID: "forgejo", Kind: ProviderKindForgejo})
	newConfig.Projects = append(newConfig.Projects, ProjectRefConfig{ID: "import"})

	want := []string{
		"agent.params.temperature",
		"daemon.environment.NEW_VAR",
		"defaults.baseBranch",
		"logging.level",
		"projects",
		"providers",
		"roles.coordinator.enabled",
		"roles.planner.triggers.planeAssigneeId",
		"scheduler.discoveryCacheTtlSeconds",
		"scheduler.pollIntervalSeconds",
		"scheduler.retryBaseDelayMs",
		"scheduler.retryMaxAttempts",
		"server.port",
		"tools.gitPath",
	}
	if got := RestartRequiredChanges(oldConfig, newConfig); !reflect.DeepEqual(got, want) {
		t.Fatalf("RestartRequiredChanges() = %#v, want %#v", got, want)
	}
}

func TestRestartRequiredChangesIgnoresHotOnlyCandidate(t *testing.T) {
	t.Parallel()

	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	newConfig := CloneConfig(oldConfig)
	newConfig.Agent.Env["TOKEN"] = "new"
	newConfig.Notifications.InApp = !newConfig.Notifications.InApp
	newConfig.Defaults.AllowAutoPush = !newConfig.Defaults.AllowAutoPush
	newConfig.Disclosure.Enabled = !newConfig.Disclosure.Enabled
	newConfig.Tools.OsascriptPath = stringPtr("/hot/osascript")
	newConfig.Agent.Timeouts.WorkerMaxRuntimeSeconds++
	newConfig.Agent.Timeouts.WorkerSeconds = newConfig.Agent.Timeouts.WorkerMaxRuntimeSeconds
	newConfig.Defaults.AllowAutoApprove = true
	newConfig.Roles.Reviewer.Behavior.ReviewEvents.Clean = ReviewerReviewEventApprove
	newConfig.Defaults.FixAllPullRequests = true
	newConfig.Roles.Fixer.Triggers.AuthorFilter = FixerAuthorFilterAny

	if got := RestartRequiredChanges(oldConfig, newConfig); len(got) != 0 {
		t.Fatalf("RestartRequiredChanges() = %#v, want no restart-bound changes", got)
	}
}

func TestRestartRequiredChangesGuardsVendorSpecificCompanions(t *testing.T) {
	t.Parallel()

	base, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	oldVendor := AgentVendorCodex
	base.Agent.Vendor = &oldVendor

	t.Run("first vendor can activate a preconfigured profile", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Vendor = nil
		old.Agent.Model = stringPtr("gpt-5")
		old.Agent.Params["command"] = "/opt/codex-wrapper"
		candidate := CloneConfig(old)
		vendor := AgentVendorCodex
		candidate.Agent.Vendor = &vendor
		if got := RestartRequiredChanges(old, candidate); len(got) != 0 {
			t.Fatalf("RestartRequiredChanges() = %#v, want no blockers", got)
		}
	})

	t.Run("vendor alone is hot when model and params are implicit", func(t *testing.T) {
		candidate := CloneConfig(base)
		vendor := AgentVendorClaudeCode
		candidate.Agent.Vendor = &vendor
		if got := RestartRequiredChanges(base, candidate); len(got) != 0 {
			t.Fatalf("RestartRequiredChanges() = %#v, want no blockers", got)
		}
	})

	t.Run("unchanged explicit model blocks vendor-only edit", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Model = stringPtr("gpt-5")
		candidate := CloneConfig(old)
		vendor := AgentVendorClaudeCode
		candidate.Agent.Vendor = &vendor
		if got, want := RestartRequiredChanges(old, candidate), []string{"agent.model"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("RestartRequiredChanges() = %#v, want %#v", got, want)
		}
	})

	t.Run("paired model edit makes the vendor choice explicit", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Model = stringPtr("gpt-5")
		candidate := CloneConfig(old)
		vendor := AgentVendorClaudeCode
		candidate.Agent.Vendor = &vendor
		candidate.Agent.Model = stringPtr("claude-sonnet")
		if got := RestartRequiredChanges(old, candidate); len(got) != 0 {
			t.Fatalf("RestartRequiredChanges() = %#v, want no blockers", got)
		}
	})

	t.Run("configured command or args requires restart", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Params["command"] = "/opt/codex-wrapper"
		candidate := CloneConfig(old)
		vendor := AgentVendorClaudeCode
		candidate.Agent.Vendor = &vendor
		if got, want := RestartRequiredChanges(old, candidate), []string{"agent.params"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("RestartRequiredChanges() = %#v, want %#v", got, want)
		}
	})

	t.Run("vendor cannot be cleared to launder a retained profile", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Model = stringPtr("gpt-5")
		old.Agent.Params["command"] = "/opt/codex-wrapper"
		disabled := CloneConfig(old)
		disabled.Agent.Vendor = nil
		if got, want := RestartRequiredChanges(old, disabled), []string{"agent.model", "agent.params"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("RestartRequiredChanges(codex -> nil) = %#v, want %#v", got, want)
		}

		claude := CloneConfig(disabled)
		vendor := AgentVendorClaudeCode
		claude.Agent.Vendor = &vendor
		if got := RestartRequiredChanges(disabled, claude); len(got) != 0 {
			t.Fatalf("RestartRequiredChanges(nil -> claude) = %#v, want prepared-profile activation allowed", got)
		}
	})
}

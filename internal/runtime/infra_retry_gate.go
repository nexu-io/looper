package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	defaultInfraRetryBudget = time.Hour
	infraCheapGateDelay     = 30 * time.Second
)

func gateRecoverableInfraRetry(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput) (bool, error) {
	if derefString(item.LastErrorKind) != "recoverable_infra" || input.Repos == nil || input.Repos.Queue == nil {
		return false, nil
	}
	now := input.Now
	if now == nil {
		now = time.Now
	}
	nowTime := now().UTC()
	if infraRetryBudgetExceeded(item, input.Config, nowTime) {
		return true, blockInfraRetry(ctx, item, input, nowTime)
	}
	ready, err := recoverableInfraConditionCleared(ctx, input.Config, input.Repos, item.ProjectID, derefString(item.LastError))
	if err != nil && input.Logger != nil {
		input.Logger.Warn("recoverable infra cheap gate unavailable; delaying retry", map[string]any{"queueItemId": item.ID, "error": err.Error()})
	}
	if err == nil && ready {
		return false, nil
	}
	delay := infraCheapGateDelay
	if input.Config != nil {
		configured := time.Duration(input.Config.Scheduler.RetryBaseDelayMS) * time.Millisecond
		if configured > delay {
			delay = configured
		}
	}
	if remaining, ok := infraRetryBudgetRemaining(item, input.Config, nowTime); ok && remaining < delay {
		delay = remaining
	}
	nowISO := formatJavaScriptISOString(nowTime)
	retryAt := formatJavaScriptISOString(nowTime.Add(delay))
	message := derefString(item.LastError)
	err = input.Repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: item.ID, AvailableAt: retryAt, Attempts: item.Attempts, ErrorMessage: optionalString(message), ErrorKind: "recoverable_infra", UpdatedAt: nowISO})
	return true, err
}

func infraRetryBudgetExceeded(item storage.QueueItemRecord, cfg *config.Config, now time.Time) bool {
	remaining, ok := infraRetryBudgetRemaining(item, cfg, now)
	return ok && remaining <= 0
}

func infraRetryBudgetRemaining(item storage.QueueItemRecord, cfg *config.Config, now time.Time) (time.Duration, bool) {
	budget := defaultInfraRetryBudget
	if cfg != nil && cfg.Scheduler.InfraRetryBudgetSeconds > 0 {
		budget = time.Duration(cfg.Scheduler.InfraRetryBudgetSeconds) * time.Second
	}
	startedAt := item.CreatedAt
	if item.StartedAt != nil && strings.TrimSpace(*item.StartedAt) != "" {
		startedAt = *item.StartedAt
	}
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return 0, false
	}
	return started.Add(budget).Sub(now), true
}

func blockInfraRetry(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput, now time.Time) error {
	nowISO := formatJavaScriptISOString(now)
	message := derefString(item.LastError)
	if err := input.Repos.Queue.Fail(ctx, storage.QueueFailInput{ID: item.ID, Attempts: item.Attempts, FinishedAt: nowISO, ErrorMessage: optionalString(message), ErrorKind: "recoverable_infra", UpdatedAt: nowISO}); err != nil {
		return err
	}
	if item.LoopID == nil || input.Repos.Loops == nil {
		return nil
	}
	loop, err := input.Repos.Loops.GetByID(ctx, *item.LoopID)
	if err != nil || loop == nil {
		return err
	}
	since := item.CreatedAt
	if item.StartedAt != nil && strings.TrimSpace(*item.StartedAt) != "" {
		since = *item.StartedAt
	}
	metadata, err := loopcondition.Set(loop.MetadataJSON, loopcondition.Record{Kind: loopcondition.InfraRecovered, Since: since, Fingerprint: message})
	if err != nil {
		return err
	}
	loop.MetadataJSON = &metadata
	loop.Status = "paused"
	loop.NextRunAt = nil
	loop.UpdatedAt = nowISO
	if err := input.Repos.Loops.Upsert(ctx, *loop); err != nil {
		return err
	}
	payload := mustMarshalJSON(map[string]any{"queueItemId": item.ID, "condition": loopcondition.InfraRecovered, "blockedAt": nowISO, "error": message})
	if err := appendSystemEvent(ctx, input.Repos, storage.EventLogRecord{ID: newRuntimeEventID(), EventType: "loop.blocked.infra", ProjectID: item.ProjectID, LoopID: item.LoopID, EntityType: stringPtr("loop"), EntityID: item.LoopID, PayloadJSON: payload, CreatedAt: nowISO}); err != nil {
		return err
	}
	if input.Logger != nil {
		input.Logger.Error("recoverable infrastructure retry budget exhausted; loop blocked", map[string]any{"loopId": *item.LoopID, "queueItemId": item.ID, "error": message})
	}
	return nil
}

func recoverableInfraConditionCleared(ctx context.Context, cfg *config.Config, repositories *storage.Repositories, projectID *string, message string) (bool, error) {
	lower := strings.ToLower(message)
	if containsAnyInfraFragment(lower, "no space left on device", "disk quota exceeded", "too many open files", "cannot allocate memory", "resource temporarily unavailable") {
		return diskConditionCleared(cfg)
	}
	if strings.Contains(lower, "fork/exec") || strings.Contains(lower, "start agent command") {
		return agentCommandAvailable(cfg)
	}
	if strings.Contains(lower, "chdir") && strings.Contains(lower, "no such file or directory") {
		if repositories == nil || repositories.Projects == nil || projectID == nil {
			return false, nil
		}
		project, err := repositories.Projects.GetByID(ctx, *projectID)
		if err != nil || project == nil {
			return false, err
		}
		info, err := os.Stat(project.RepoPath)
		return err == nil && info.IsDir(), err
	}
	// Missing/stale worktrees are intentionally allowed through: the role's
	// prepare-worktree step is the cheap self-heal and runs before any agent.
	if containsAnyInfraFragment(lower, "worktree", "not a git repository") {
		return true, nil
	}
	return diskConditionCleared(cfg)
}

func agentCommandAvailable(cfg *config.Config) (bool, error) {
	if cfg == nil || cfg.Agent.Vendor == nil {
		return false, nil
	}
	command, _ := agent.ResolveSpawn(agent.ExecutorConfig{Vendor: *cfg.Agent.Vendor, Model: cfg.Agent.Model, Params: cfg.Agent.Params}, "", "")
	command = strings.TrimSpace(command)
	if command == "" {
		return false, nil
	}
	if filepath.IsAbs(command) || strings.ContainsRune(command, os.PathSeparator) {
		info, err := os.Stat(command)
		return err == nil && !info.IsDir() && info.Mode()&0o111 != 0, err
	}
	_, err := exec.LookPath(command)
	if err != nil {
		return false, err
	}
	return true, nil
}

func containsAnyInfraFragment(message string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func optionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

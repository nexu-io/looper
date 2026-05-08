package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/spf13/cobra"
)

const defaultRunStatsSince = "24h"

var runStatsRoleOrder = []string{"planner", "reviewer", "worker", "fixer"}

type runStatsOutput struct {
	Since      string                     `json:"since"`
	SinceAtISO string                     `json:"sinceAtIso"`
	UntilISO   string                     `json:"untilIso"`
	Roles      map[string]runRoleStats    `json:"roles"`
	Total      runRoleStats               `json:"total"`
	Scope      runStatsScopeDocumentation `json:"scope"`
}

type runStatsScopeDocumentation struct {
	Runs            string `json:"runs"`
	AgentExecutions string `json:"agentExecutions"`
	ReviewOutcomes  string `json:"reviewOutcomes"`
}

type runRoleStats struct {
	Success         int64               `json:"success"`
	Failure         int64               `json:"failure"`
	Skipped         int64               `json:"skipped"`
	Interrupted     int64               `json:"interrupted"`
	Requeued        int64               `json:"requeued"`
	Retried         int64               `json:"retried"`
	Running         int64               `json:"running"`
	Cancelled       int64               `json:"cancelled"`
	ParseFailed     int64               `json:"parseFailed"`
	Outcomes        reviewOutcomeStats  `json:"outcomes"`
	AgentExecutions agentExecutionStats `json:"agentExecutions"`
}

type reviewOutcomeStats struct {
	Approved         int64 `json:"approved"`
	Commented        int64 `json:"commented"`
	RequestedChanges int64 `json:"requested_changes"`
}

type agentExecutionStats struct {
	Success     int64            `json:"success"`
	Failure     int64            `json:"failure"`
	Interrupted int64            `json:"interrupted"`
	Running     int64            `json:"running"`
	Status      map[string]int64 `json:"status"`
}

func (r *commandRuntime) runStats(cmd *cobra.Command, args []string) error {
	_ = args
	sinceText := strings.TrimSpace(getStringFlag(cmd, "since"))
	if sinceText == "" {
		sinceText = defaultRunStatsSince
	}
	duration, err := parseRunStatsDuration(sinceText)
	if err != nil {
		return err
	}
	role := strings.ToLower(strings.TrimSpace(getStringFlag(cmd, "role")))
	if role != "" && !isKnownRunStatsRole(role) {
		return fmt.Errorf("unsupported --role %q; expected planner, reviewer, worker, or fixer", role)
	}
	loopFilter := strings.TrimSpace(getStringFlag(cmd, "loop"))

	return r.withQueueRepositories(cmd.Context(), func(repos *storage.Repositories) error {
		now := time.Now().UTC()
		sinceAt := now.Add(-duration)
		output, err := buildRunStatsOutput(cmd.Context(), repos, sinceText, sinceAt, now, role, loopFilter)
		if err != nil {
			return err
		}
		if getBoolFlag(cmd, "json") {
			return writeJSON(cmd.OutOrStdout(), output)
		}
		return writeHumanRunStats(cmd.OutOrStdout(), output, role)
	})
}

func parseRunStatsDuration(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		trimmed = defaultRunStatsSince
	}
	if strings.HasSuffix(trimmed, "d") || strings.HasSuffix(trimmed, "D") {
		daysText := strings.TrimSpace(trimmed[:len(trimmed)-1])
		days, err := time.ParseDuration(daysText + "h")
		if err != nil {
			return 0, fmt.Errorf("invalid --since duration %q", value)
		}
		duration := days * 24
		if duration <= 0 {
			return 0, fmt.Errorf("--since must be positive")
		}
		return duration, nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid --since duration %q", value)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("--since must be positive")
	}
	return duration, nil
}

func buildRunStatsOutput(ctx context.Context, repos *storage.Repositories, sinceText string, sinceAt time.Time, until time.Time, roleFilter string, loopFilter string) (runStatsOutput, error) {
	output := runStatsOutput{
		Since:      sinceText,
		SinceAtISO: eventlog.FormatJavaScriptISOString(sinceAt),
		UntilISO:   eventlog.FormatJavaScriptISOString(until),
		Roles:      initialRunStatsRoles(roleFilter),
		Scope: runStatsScopeDocumentation{
			Runs:            "Persisted run records joined to loop roles are included.",
			AgentExecutions: "Persisted agent execution records linked to matching loops are included in each role's agentExecutions breakdown.",
			ReviewOutcomes:  "Reviewer outcomes are inferred from persisted pr.review.posted events when available.",
		},
	}
	sinceISO := eventlog.FormatJavaScriptISOString(sinceAt)

	loops, err := repos.Loops.List(ctx)
	if err != nil {
		return output, err
	}
	loopRoles := make(map[string]string, len(loops))
	for _, loop := range loops {
		if loopFilter != "" && loop.ID != loopFilter {
			continue
		}
		loopRoles[loop.ID] = loop.Type
	}

	runs, err := repos.Runs.ListSince(ctx, sinceISO)
	if err != nil {
		return output, err
	}
	for _, run := range runs {
		startedAt, ok := parseRunStatsTime(run.StartedAt)
		if !ok || startedAt.Before(sinceAt) {
			continue
		}
		role := loopRoles[run.LoopID]
		if !includeRunStatsRole(role, roleFilter) {
			continue
		}
		stats := output.Roles[role]
		addRunToStats(&stats, run)
		output.Roles[role] = stats
	}

	agents, err := repos.AgentExecutions.ListSince(ctx, sinceISO)
	if err != nil {
		return output, err
	}
	for _, agent := range agents {
		startedAt, ok := parseRunStatsTime(agent.StartedAt)
		if !ok || startedAt.Before(sinceAt) || agent.LoopID == nil {
			continue
		}
		role := loopRoles[*agent.LoopID]
		if !includeRunStatsRole(role, roleFilter) {
			continue
		}
		stats := output.Roles[role]
		addAgentExecutionToStats(&stats.AgentExecutions, agent)
		output.Roles[role] = stats
	}

	events, err := repos.Events.ListSince(ctx, sinceISO)
	if err != nil {
		return output, err
	}
	for _, event := range events {
		createdAt, ok := parseRunStatsTime(event.CreatedAt)
		if !ok || createdAt.Before(sinceAt) || event.LoopID == nil {
			continue
		}
		role := loopRoles[*event.LoopID]
		if !includeRunStatsRole(role, roleFilter) {
			continue
		}
		stats := output.Roles[role]
		addEventToStats(&stats, event)
		output.Roles[role] = stats
	}
	queues, err := repos.Queue.List(ctx)
	if err != nil {
		return output, err
	}
	for _, item := range queues {
		updatedAt, ok := parseRunStatsTime(item.UpdatedAt)
		if !ok || updatedAt.Before(sinceAt) || !queueItemHasRecentRetry(item) {
			continue
		}
		role := item.Type
		if item.LoopID != nil {
			if loopFilter != "" && *item.LoopID != loopFilter {
				continue
			}
			if mappedRole := loopRoles[*item.LoopID]; mappedRole != "" {
				role = mappedRole
			}
		} else if loopFilter != "" {
			continue
		}
		if !includeRunStatsRole(role, roleFilter) {
			continue
		}
		stats := output.Roles[role]
		stats.Retried += item.Attempts
		output.Roles[role] = stats
	}
	for _, stats := range output.Roles {
		addRoleStats(&output.Total, stats)
	}
	return output, nil
}

func initialRunStatsRoles(roleFilter string) map[string]runRoleStats {
	roles := map[string]runRoleStats{}
	for _, role := range runStatsRoleOrder {
		if roleFilter == "" || role == roleFilter {
			roles[role] = newRunRoleStats()
		}
	}
	return roles
}

func newRunRoleStats() runRoleStats {
	return runRoleStats{AgentExecutions: agentExecutionStats{Status: map[string]int64{}}}
}

func isKnownRunStatsRole(role string) bool {
	for _, known := range runStatsRoleOrder {
		if role == known {
			return true
		}
	}
	return false
}

func includeRunStatsRole(role string, filter string) bool {
	if !isKnownRunStatsRole(role) {
		return false
	}
	return filter == "" || role == filter
}

func parseRunStatsTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func addRunToStats(stats *runRoleStats, run storage.RunRecord) {
	if runWasSkipped(run) {
		stats.Skipped++
	} else {
		switch run.Status {
		case "success", "completed":
			stats.Success++
		case "failed":
			stats.Failure++
		case "interrupted":
			stats.Interrupted++
		case "running", "queued":
			stats.Running++
		case "cancelled", "canceled":
			stats.Cancelled++
		case "parse_failed":
			stats.ParseFailed++
		}
	}
}

func runWasSkipped(run storage.RunRecord) bool {
	if run.CheckpointJSON == nil {
		return false
	}
	var checkpoint struct {
		SkipReason string `json:"skipReason"`
		SkipKind   string `json:"skipKind"`
	}
	if err := json.Unmarshal([]byte(*run.CheckpointJSON), &checkpoint); err != nil {
		return false
	}
	return strings.TrimSpace(checkpoint.SkipReason) != "" || strings.TrimSpace(checkpoint.SkipKind) != ""
}

func normalizeReviewEvent(event string) string {
	switch strings.ToUpper(strings.TrimSpace(event)) {
	case "APPROVE", "APPROVED":
		return "approved"
	case "COMMENT", "COMMENTED":
		return "commented"
	case "REQUEST_CHANGES", "CHANGES_REQUESTED", "REQUESTED_CHANGES":
		return "requested_changes"
	default:
		return ""
	}
}

func addReviewOutcome(stats *reviewOutcomeStats, outcome string) {
	switch outcome {
	case "approved":
		stats.Approved++
	case "commented":
		stats.Commented++
	case "requested_changes":
		stats.RequestedChanges++
	}
}

func addAgentExecutionToStats(stats *agentExecutionStats, agent storage.AgentExecutionRecord) {
	status := strings.ToLower(strings.TrimSpace(agent.Status))
	if status == "" {
		status = "unknown"
	}
	if stats.Status == nil {
		stats.Status = map[string]int64{}
	}
	stats.Status[status]++
	switch status {
	case "completed", "success", "succeeded":
		stats.Success++
	case "failed", "error", "parse_failed", "timeout", "timed_out":
		stats.Failure++
	case "interrupted", "cancelled", "canceled", "killed":
		stats.Interrupted++
	case "running", "starting", "cancelling", "queued":
		stats.Running++
	}
}

func addEventToStats(stats *runRoleStats, event storage.EventLogRecord) {
	eventType := strings.ToLower(event.EventType)
	if eventType == "pr.review.posted" {
		var payload struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err == nil {
			addReviewOutcome(&stats.Outcomes, normalizeReviewEvent(payload.Event))
		}
		return
	}
	switch {
	case strings.Contains(eventType, "requeued"):
		stats.Requeued++
	}
}

func queueItemHasRecentRetry(item storage.QueueItemRecord) bool {
	if item.Attempts <= 0 {
		return false
	}
	switch item.Status {
	case "queued", "running", "completed", "failed", "cancelled", "manual_intervention":
	default:
		return false
	}
	return true
}

func addRoleStats(total *runRoleStats, stats runRoleStats) {
	total.Success += stats.Success
	total.Failure += stats.Failure
	total.Skipped += stats.Skipped
	total.Interrupted += stats.Interrupted
	total.Requeued += stats.Requeued
	total.Retried += stats.Retried
	total.Running += stats.Running
	total.Cancelled += stats.Cancelled
	total.ParseFailed += stats.ParseFailed
	total.Outcomes.Approved += stats.Outcomes.Approved
	total.Outcomes.Commented += stats.Outcomes.Commented
	total.Outcomes.RequestedChanges += stats.Outcomes.RequestedChanges
	total.AgentExecutions.Success += stats.AgentExecutions.Success
	total.AgentExecutions.Failure += stats.AgentExecutions.Failure
	total.AgentExecutions.Interrupted += stats.AgentExecutions.Interrupted
	total.AgentExecutions.Running += stats.AgentExecutions.Running
	if total.AgentExecutions.Status == nil {
		total.AgentExecutions.Status = map[string]int64{}
	}
	for status, count := range stats.AgentExecutions.Status {
		total.AgentExecutions.Status[status] += count
	}
}

func writeHumanRunStats(w io.Writer, output runStatsOutput, roleFilter string) error {
	if runRoleStatsEmpty(output.Total) {
		_, err := fmt.Fprintf(w, "No matching recent actions found since %s (%s).\n", output.Since, output.SinceAtISO)
		return err
	}
	if _, err := fmt.Fprintf(w, "Recent run stats since %s (%s):\n", output.Since, output.SinceAtISO); err != nil {
		return err
	}
	for _, role := range orderedRunStatsRoles(output.Roles, roleFilter) {
		stats := output.Roles[role]
		if _, err := fmt.Fprintf(w, "\n%s\n", role); err != nil {
			return err
		}
		rows := [][2]any{{"success", stats.Success}, {"failure", stats.Failure}, {"skipped", stats.Skipped}, {"interrupted", stats.Interrupted}, {"requeued", stats.Requeued}, {"retried", stats.Retried}, {"running", stats.Running}, {"cancelled", stats.Cancelled}, {"parseFailed", stats.ParseFailed}, {"outcomes.approved", stats.Outcomes.Approved}, {"outcomes.commented", stats.Outcomes.Commented}, {"outcomes.requested_changes", stats.Outcomes.RequestedChanges}, {"agentExecutions.success", stats.AgentExecutions.Success}, {"agentExecutions.failure", stats.AgentExecutions.Failure}, {"agentExecutions.interrupted", stats.AgentExecutions.Interrupted}, {"agentExecutions.running", stats.AgentExecutions.Running}}
		for _, row := range rows {
			if _, err := fmt.Fprintf(w, "  %-32s %v\n", row[0], row[1]); err != nil {
				return err
			}
		}
		if len(stats.AgentExecutions.Status) > 0 {
			statuses := make([]string, 0, len(stats.AgentExecutions.Status))
			for status := range stats.AgentExecutions.Status {
				statuses = append(statuses, status)
			}
			sort.Strings(statuses)
			for _, status := range statuses {
				if _, err := fmt.Fprintf(w, "  %-32s %v\n", "agentExecutions.status."+status, stats.AgentExecutions.Status[status]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func orderedRunStatsRoles(roles map[string]runRoleStats, roleFilter string) []string {
	ordered := make([]string, 0, len(roles))
	for _, role := range runStatsRoleOrder {
		if _, ok := roles[role]; ok && (roleFilter == "" || role == roleFilter) {
			ordered = append(ordered, role)
		}
	}
	return ordered
}

func runRoleStatsEmpty(stats runRoleStats) bool {
	return stats.Success == 0 && stats.Failure == 0 && stats.Skipped == 0 && stats.Interrupted == 0 && stats.Requeued == 0 && stats.Retried == 0 && stats.Running == 0 && stats.Cancelled == 0 && stats.ParseFailed == 0 && stats.Outcomes.Approved == 0 && stats.Outcomes.Commented == 0 && stats.Outcomes.RequestedChanges == 0 && stats.AgentExecutions.Success == 0 && stats.AgentExecutions.Failure == 0 && stats.AgentExecutions.Interrupted == 0 && stats.AgentExecutions.Running == 0 && len(stats.AgentExecutions.Status) == 0
}

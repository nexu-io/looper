package cliapp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/spf13/cobra"
)

type queueStatsOutput struct {
	NowISO                           string `json:"nowIso"`
	TotalQueued                      int64  `json:"totalQueued"`
	EligibleQueued                   int64  `json:"eligibleQueued"`
	BlockedByTerminalOrPausedLoop    int64  `json:"blockedByTerminalOrPausedLoop"`
	BlockedByLockKey                 int64  `json:"blockedByLockKey"`
	BlockedByReviewerFixerDependency int64  `json:"blockedByReviewerFixerDependency"`
	ScheduledForFuture               int64  `json:"scheduledForFuture"`
	StaleQueued                      int64  `json:"staleQueued"`
}

type queueListOutput struct {
	NowISO string                   `json:"nowIso"`
	Items  []queueItemCommandOutput `json:"items"`
}

type queueItemCommandOutput struct {
	ID            string  `json:"id"`
	ProjectID     *string `json:"projectId,omitempty"`
	LoopID        *string `json:"loopId,omitempty"`
	Type          string  `json:"type"`
	TargetType    string  `json:"targetType"`
	TargetID      string  `json:"targetId"`
	Repo          *string `json:"repo,omitempty"`
	PRNumber      *int64  `json:"prNumber,omitempty"`
	Priority      int64   `json:"priority"`
	Status        string  `json:"status"`
	AvailableAt   string  `json:"availableAt"`
	Attempts      int64   `json:"attempts"`
	MaxAttempts   int64   `json:"maxAttempts"`
	LockKey       *string `json:"lockKey,omitempty"`
	LastError     *string `json:"lastError,omitempty"`
	LastErrorKind *string `json:"lastErrorKind,omitempty"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

type queueCleanupOutput struct {
	Stale   bool  `json:"stale"`
	Cleaned int64 `json:"cleaned"`
}

func (r *commandRuntime) queueStats(cmd *cobra.Command, args []string) error {
	_ = args
	return r.withQueueRepositories(cmd.Context(), func(repos *storage.Repositories) error {
		nowISO := eventlog.FormatJavaScriptISOString(time.Now().UTC())
		stats, err := repos.Queue.Stats(cmd.Context(), nowISO)
		if err != nil {
			return err
		}
		output := queueStatsOutput{
			NowISO:                           nowISO,
			TotalQueued:                      stats.TotalQueued,
			EligibleQueued:                   stats.EligibleQueued,
			BlockedByTerminalOrPausedLoop:    stats.BlockedByTerminalOrPausedLoop,
			BlockedByLockKey:                 stats.BlockedByLockKey,
			BlockedByReviewerFixerDependency: stats.BlockedByReviewerFixerDependency,
			ScheduledForFuture:               stats.ScheduledForFuture,
			StaleQueued:                      stats.StaleQueued,
		}
		if getBoolFlag(cmd, "json") {
			return writeJSON(cmd.OutOrStdout(), output)
		}
		return writeHumanQueueStats(cmd.OutOrStdout(), output)
	})
}

func (r *commandRuntime) queueList(cmd *cobra.Command, args []string) error {
	_ = args
	return r.withQueueRepositories(cmd.Context(), func(repos *storage.Repositories) error {
		nowISO := eventlog.FormatJavaScriptISOString(time.Now().UTC())
		var (
			items []storage.QueueItemRecord
			err   error
		)
		if getBoolFlag(cmd, "eligible") {
			items, err = repos.Queue.ListScheduled(cmd.Context(), nowISO, 1000)
		} else {
			items, err = repos.Queue.ListQueued(cmd.Context(), 1000)
		}
		if err != nil {
			return err
		}
		output := queueListOutput{NowISO: nowISO, Items: queueItemOutputs(items)}
		if getBoolFlag(cmd, "json") {
			return writeJSON(cmd.OutOrStdout(), output)
		}
		return writeHumanQueueList(cmd.OutOrStdout(), output)
	})
}

func (r *commandRuntime) queueCleanup(cmd *cobra.Command, args []string) error {
	_ = args
	if !getBoolFlag(cmd, "stale") {
		return fmt.Errorf("queue cleanup currently requires --stale")
	}
	return r.withQueueRepositories(cmd.Context(), func(repos *storage.Repositories) error {
		nowISO := eventlog.FormatJavaScriptISOString(time.Now().UTC())
		cleaned, err := repos.Queue.CleanupStaleQueued(cmd.Context(), nowISO, "stale queue item attached to terminal loop")
		if err != nil {
			return err
		}
		output := queueCleanupOutput{Stale: true, Cleaned: cleaned}
		if getBoolFlag(cmd, "json") {
			return writeJSON(cmd.OutOrStdout(), output)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Cleaned %d stale queued item(s).\n", cleaned)
		return err
	})
}

func (r *commandRuntime) withQueueRepositories(ctx context.Context, fn func(*storage.Repositories) error) error {
	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}
	db, err := storage.OpenSQLiteDB(ctx, loaded.Config.Storage.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return fn(storage.NewRepositories(db))
}

func queueItemOutputs(items []storage.QueueItemRecord) []queueItemCommandOutput {
	outputs := make([]queueItemCommandOutput, 0, len(items))
	for _, item := range items {
		outputs = append(outputs, queueItemCommandOutput{
			ID:            item.ID,
			ProjectID:     item.ProjectID,
			LoopID:        item.LoopID,
			Type:          item.Type,
			TargetType:    item.TargetType,
			TargetID:      item.TargetID,
			Repo:          item.Repo,
			PRNumber:      item.PRNumber,
			Priority:      item.Priority,
			Status:        item.Status,
			AvailableAt:   item.AvailableAt,
			Attempts:      item.Attempts,
			MaxAttempts:   item.MaxAttempts,
			LockKey:       item.LockKey,
			LastError:     item.LastError,
			LastErrorKind: item.LastErrorKind,
			CreatedAt:     item.CreatedAt,
			UpdatedAt:     item.UpdatedAt,
		})
	}
	return outputs
}

func writeHumanQueueStats(w io.Writer, output queueStatsOutput) error {
	rows := [][2]any{
		{"totalQueued", output.TotalQueued},
		{"eligibleQueued", output.EligibleQueued},
		{"blockedByTerminalOrPausedLoop", output.BlockedByTerminalOrPausedLoop},
		{"blockedByLockKey", output.BlockedByLockKey},
		{"blockedByReviewerFixerDependency", output.BlockedByReviewerFixerDependency},
		{"scheduledForFuture", output.ScheduledForFuture},
		{"staleQueued", output.StaleQueued},
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(w, "%-36s %v\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

func writeHumanQueueList(w io.Writer, output queueListOutput) error {
	if len(output.Items) == 0 {
		_, err := fmt.Fprintln(w, "No eligible queued items.")
		return err
	}
	for _, item := range output.Items {
		target := strings.TrimSpace(item.TargetID)
		if item.Repo != nil && item.PRNumber != nil {
			target = fmt.Sprintf("%s#%d", *item.Repo, *item.PRNumber)
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", item.ID, item.Type, item.Status, item.AvailableAt, target); err != nil {
			return err
		}
	}
	return nil
}

package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// notifyHumanAttentionBestEffort builds a short-lived gateway from the current
// runtime config and emits human-attention notifications without affecting the
// caller's control flow. Used from recovery paths that park work outside the
// scheduler claim lifecycle.
func (r *Runtime) notifyHumanAttentionBestEffort(ctx context.Context, repos *storage.Repositories, loopID string) {
	if r == nil || repos == nil || strings.TrimSpace(loopID) == "" {
		return
	}
	cfg := r.Config()
	gateway := notify.NewGateway(notify.Options{
		Config:            cfg.Notifications,
		OsascriptPath:     strings.TrimSpace(derefString(cfg.Tools.OsascriptPath)),
		LogFilePath:       filepath.Join(cfg.Daemon.LogDir, "looperd.log"),
		DashboardBaseURL:  notify.ResolveDashboardBaseURL(cfg.Server),
		DashboardAuthMode: cfg.Server.AuthMode,
		Repositories:      repos,
		Now:               r.now,
	})
	notifyDurableHumanAttention(ctx, gateway, repos, loopID)
}

// notifyDurableHumanAttention observes durable loop/queue state after a claim
// finishes (or after recovery parks work) and emits at most one action_required
// notification for a newly entered human-attention condition.
//
// Authority remains the durable loop/queue state. Notification is best-effort:
// failures are audited by the gateway and must not change loop/queue/run behavior.
func notifyDurableHumanAttention(ctx context.Context, gateway *notify.Gateway, repos *storage.Repositories, loopID string) {
	if gateway == nil || repos == nil || repos.Loops == nil || strings.TrimSpace(loopID) == "" {
		return
	}
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}

	if notify.IsHumanAttentionLoopStatus(loop.Status) {
		entryKey := humanAttentionEntryKeyForAwaitingHuman(ctx, repos, *loop)
		if entryKey == "" {
			return
		}
		gateway.NotifyHumanAttention(ctx, notify.HumanAttentionInput{
			ProjectID:  loop.ProjectID,
			LoopID:     loop.ID,
			LoopSeq:    loop.Seq,
			RunID:      latestRunID(ctx, repos, loop.ID),
			LoopType:   loop.Type,
			Reason:     notify.HumanAttentionAwaitingHuman,
			EntryKey:   entryKey,
			Subtitle:   humanAttentionSubtitle(*loop),
			EntityType: "loop",
			EntityID:   loop.ID,
		})
		return
	}

	if repos.Queue == nil {
		return
	}
	queue, err := repos.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || queue == nil {
		return
	}
	if queue.Status != "manual_intervention" {
		return
	}
	lastErrorKind := ""
	if queue.LastErrorKind != nil {
		lastErrorKind = *queue.LastErrorKind
	}
	resumePolicy := ""
	if repos.Runs != nil {
		if run, runErr := repos.Runs.GetLatestByLoopID(ctx, loop.ID); runErr == nil && run != nil {
			resumePolicy = resumePolicyFromCheckpointJSON(run.CheckpointJSON)
		}
	}
	if !notify.IsManualInterventionCondition(lastErrorKind, resumePolicy) {
		return
	}
	entryKey := humanAttentionEntryKeyForManualIntervention(*queue)
	if entryKey == "" {
		return
	}
	gateway.NotifyHumanAttention(ctx, notify.HumanAttentionInput{
		ProjectID:  loop.ProjectID,
		LoopID:     loop.ID,
		LoopSeq:    loop.Seq,
		RunID:      latestRunID(ctx, repos, loop.ID),
		LoopType:   loop.Type,
		Reason:     notify.HumanAttentionManualIntervention,
		EntryKey:   entryKey,
		Subtitle:   humanAttentionSubtitle(*loop),
		EntityType: "queue_item",
		EntityID:   queue.ID,
	})
}

func humanAttentionEntryKeyForAwaitingHuman(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord) string {
	// Prefer run id so leave+re-enter (new run) is a new event. Fall back to
	// askedAt / loop.UpdatedAt so a park without a run still dedupes across restarts.
	if runID := latestRunID(ctx, repos, loop.ID); runID != "" {
		return "run:" + runID
	}
	if ask, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok {
		if askedAt := strings.TrimSpace(ask.AskedAt); askedAt != "" {
			return "asked:" + askedAt
		}
	}
	if updated := strings.TrimSpace(loop.UpdatedAt); updated != "" {
		return "loop:" + loop.ID + ":" + updated
	}
	return "loop:" + loop.ID
}

func humanAttentionEntryKeyForManualIntervention(queue storage.QueueItemRecord) string {
	// FinishedAt changes on each durable park of the same queue row after requeue.
	if queue.FinishedAt != nil && strings.TrimSpace(*queue.FinishedAt) != "" {
		return "queue:" + queue.ID + ":" + strings.TrimSpace(*queue.FinishedAt)
	}
	if updated := strings.TrimSpace(queue.UpdatedAt); updated != "" {
		return "queue:" + queue.ID + ":" + updated
	}
	return "queue:" + queue.ID
}

func humanAttentionSubtitle(loop storage.LoopRecord) string {
	parts := make([]string, 0, 3)
	if repo := strings.TrimSpace(derefString(loop.Repo)); repo != "" {
		parts = append(parts, repo)
	}
	if loop.PRNumber != nil && *loop.PRNumber > 0 {
		parts = append(parts, "#"+strconv.FormatInt(*loop.PRNumber, 10))
	}
	if loop.Type != "" {
		parts = append(parts, loop.Type)
	}
	return strings.Join(parts, " ")
}

func latestRunID(ctx context.Context, repos *storage.Repositories, loopID string) string {
	if repos == nil || repos.Runs == nil || strings.TrimSpace(loopID) == "" {
		return ""
	}
	run, err := repos.Runs.GetLatestByLoopID(ctx, loopID)
	if err != nil || run == nil {
		return ""
	}
	return strings.TrimSpace(run.ID)
}

func resumePolicyFromCheckpointJSON(checkpointJSON *string) string {
	if checkpointJSON == nil || strings.TrimSpace(*checkpointJSON) == "" {
		return ""
	}
	var parsed struct {
		ResumePolicy string `json:"resumePolicy"`
	}
	if err := json.Unmarshal([]byte(*checkpointJSON), &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.ResumePolicy)
}

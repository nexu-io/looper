package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// EnqueueInput is an explicit dispatch to an already configured checkout.
// Project identity and code repository must agree; task titles never select a repo.
type EnqueueInput struct {
	ProjectID string `json:"projectId"`
	Repo      string `json:"repo"`
	// Module is an optional stable collaboration scope. When present, all
	// requirements dispatched under the module share one Feishu group while
	// retaining separate task threads.
	ChatScope        string `json:"chatScope,omitempty"`
	ChatTitle        string `json:"chatTitle,omitempty"`
	ChatURL          string `json:"chatUrl,omitempty"`
	Module           string `json:"module,omitempty"`
	Title            string `json:"title"`
	Prompt           string `json:"prompt"`
	IssueNumber      int64  `json:"issueNumber,omitempty"`
	IssueURL         string `json:"issueUrl,omitempty"`
	StrictDispatchID string `json:"strictDispatchId,omitempty"`
}

func (r *Runner) Enqueue(ctx context.Context, input EnqueueInput) (storage.LoopRecord, error) {
	if input.IssueNumber < 0 {
		return storage.LoopRecord{}, fmt.Errorf("issueNumber must not be negative")
	}
	project, err := r.repos.Projects.GetByID(ctx, input.ProjectID)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	if project == nil {
		return storage.LoopRecord{}, fmt.Errorf("project not found: %s", input.ProjectID)
	}
	configuredRepo := stringFromAnyDefault(parseJSONObject(project.MetadataJSON)["repo"])
	if strings.TrimSpace(input.Repo) == "" || !strings.EqualFold(input.Repo, configuredRepo) {
		return storage.LoopRecord{}, fmt.Errorf("repository must explicitly match project %s (%s)", input.ProjectID, configuredRepo)
	}
	if strings.TrimSpace(input.Prompt) == "" && input.IssueNumber <= 0 {
		return storage.LoopRecord{}, fmt.Errorf("prompt or issueNumber is required")
	}
	work := workerInput{Title: input.Title, Prompt: input.Prompt, Repo: configuredRepo, Module: input.Module, ChatScope: input.ChatScope, ChatTitle: input.ChatTitle, ChatURL: input.ChatURL, IssueNumber: input.IssueNumber, IssueURL: input.IssueURL, StrictDispatchID: input.StrictDispatchID, BaseBranch: derefString(project.BaseBranch), ExecutionMode: "create-pr"}
	payload, err := json.Marshal(work)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	metadata, err := json.Marshal(map[string]any{"worker": work})
	if err != nil {
		return storage.LoopRecord{}, err
	}
	payloadJSON, metadataJSON := string(payload), string(metadata)
	target := domain.LoopTarget{TargetType: domain.LoopTargetTypeProject, ProjectID: project.ID}
	var key *string
	if input.IssueNumber > 0 {
		target = domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, Repo: configuredRepo, IssueNumber: input.IssueNumber}
		value := storage.IssueLockKey(project.ID, configuredRepo, input.IssueNumber)
		key = &value
	}
	now := r.nowISO()
	id := ""
	if input.StrictDispatchID != "" {
		id = "loop_plane_" + input.StrictDispatchID
	}
	service := loops.Service{DB: r.db, Repos: r.repos, Now: r.now}
	return service.Create(ctx, loops.CreateInput{ID: id, ProjectID: project.ID, Type: domain.LoopTypeWorker, Target: target, Status: domain.LoopStatusQueued, MetadataJSON: &metadataJSON,
		QueueItem: &storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), Type: "worker", Repo: &configuredRepo, LockKey: key, PayloadJSON: &payloadJSON, Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: now, MaxAttempts: r.retryMaxAttempts, CreatedAt: now, UpdatedAt: now},
	})
}

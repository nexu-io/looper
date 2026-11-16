package collab

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/executionhost"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

type planeSource struct {
	project config.ProjectRefConfig
	client  *forge.PlaneClient
	nodeID  string
}

func planeSources(cfg config.Config) ([]planeSource, error) {
	var result []planeSource
	for _, project := range cfg.Projects {
		for _, provider := range cfg.Providers {
			if project.Provider != provider.ID || provider.Kind != config.ProviderKindPlane || provider.StrictDispatch == nil || !provider.StrictDispatch.Enabled {
				continue
			}
			for _, existing := range result {
				if existing.project.Provider == project.Provider && strings.EqualFold(existing.project.Repo, project.Repo) {
					return nil, fmt.Errorf("Plane repository %s selects multiple local projects; configure one checkout per dispatch target", project.Repo)
				}
			}
			client, err := forge.NewPlaneClientFromConfig(provider, project.Repo)
			if err != nil {
				return nil, fmt.Errorf("configure Plane project %s: %w", project.ID, err)
			}
			result = append(result, planeSource{project: project, client: client, nodeID: provider.StrictDispatch.NodeID})
		}
	}
	return result, nil
}

func (s planeSource) poll(ctx context.Context, runner *worker.Runner, service *loops.Service, repos *storage.Repositories, owner *executionhost.ActiveExecutionRegistry) error {
	inbox, err := s.client.StrictDispatchInbox(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, dispatch := range inbox.Dispatches {
		if dispatch.RequestedMode != "collab" || dispatch.ActiveRole != "worker" || !strings.EqualFold(strings.TrimSpace(dispatch.CodeRepo), s.project.Repo) {
			continue
		}
		if err := s.processDispatch(ctx, runner, service, repos, owner, dispatch); err != nil {
			failures = append(failures, fmt.Errorf("dispatch %s: %w", dispatch.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (s planeSource) processDispatch(ctx context.Context, runner *worker.Runner, service *loops.Service, repos *storage.Repositories, owner *executionhost.ActiveExecutionRegistry, dispatch planestrict.Dispatch) error {
	loop, err := repos.Loops.GetByID(ctx, "loop_plane_"+dispatch.ID)
	if err != nil {
		return err
	}
	if loop == nil {
		if dispatch.Health != "ok" || (dispatch.State != "queued" && dispatch.State != "claimed") {
			return nil
		}
		issue, err := s.client.StrictDispatchIssue(ctx, dispatch)
		if err != nil {
			return err
		}
		if dispatch.State == "queued" {
			if err := s.client.ClaimStrictDispatch(ctx, dispatch); err != nil {
				return err
			}
		}
		input := worker.EnqueueInput{ProjectID: s.project.ID, Repo: dispatch.CodeRepo, Title: issue.Title, Prompt: issue.Body, IssueNumber: issue.Number, IssueURL: issue.HTMLURL, StrictDispatchID: dispatch.ID}
		if scope := dispatch.CollaborationScope; scope != nil {
			input.ChatScope, input.ChatTitle = scope.Key, scope.Name
			input.ChatURL = s.client.CollaborationURL(scope)
		}
		_, err = runner.Enqueue(ctx, input)
		return err
	}
	if loop.ProjectID != s.project.ID {
		return fmt.Errorf("Plane dispatch %s already belongs to local project %s", dispatch.ID, loop.ProjectID)
	}
	if dispatch.State == "stop_requested" || dispatch.State == "confirmed_stopped" {
		if err := stopPlaneTask(ctx, service, repos, owner, *loop); err != nil {
			return err
		}
		if dispatch.State == "stop_requested" {
			if err := s.client.TransitionStrictDispatch(ctx, dispatch.ID, "confirmed_stopped", nil); err != nil {
				return err
			}
		}
		return nil
	}
	if dispatch.Health != "ok" {
		return nil
	}
	return s.syncTask(ctx, repos, owner, *loop, dispatch)
}

func stopPlaneTask(ctx context.Context, service *loops.Service, repos *storage.Repositories, owner *executionhost.ActiveExecutionRegistry, loop storage.LoopRecord) error {
	// Never stop a process reconstructed from a persisted PID. Quarantined
	// crash evidence needs explicit recovery; it cannot prove a stopped group.
	active, err := repos.AgentExecutions.ListActive(ctx)
	if err != nil {
		return err
	}
	for _, execution := range active {
		if execution.LoopID != nil && *execution.LoopID == loop.ID && !owner.HasLiveHandle(loop.ID, value(execution.RunID), execution.ID) {
			return fmt.Errorf("task %s has unowned execution evidence; recovery confirmation is required before acknowledging a stop", loop.ID)
		}
	}
	if _, err := owner.BeginLoopStop(loop.ID, "Stopped by owner in Plane"); err != nil {
		return err
	}
	if loop.Status == "terminated" || loop.Status == "completed" || loop.Status == "failed" {
		return nil
	}
	// Keep the stop gate closed after a durable termination; late HITL replies
	// or an already-claimed runner must not start a new subprocess.
	reason := "Stopped by owner in Plane"
	_, err = service.Terminate(ctx, loop.ID, &reason)
	return err
}

func (s planeSource) syncTask(ctx context.Context, repos *storage.Repositories, owner *executionhost.ActiveExecutionRegistry, loop storage.LoopRecord, dispatch planestrict.Dispatch) error {
	state := ""
	var waitKind *string
	switch loop.Status {
	case "awaiting_human":
		state = "awaiting_human"
		kind := "role_decision"
		waitKind = &kind
	case "completed":
		state = "completed"
		if marker, ok := loops.ReadShepherd(loop.MetadataJSON); ok && marker.Outcome == "abandoned" {
			state = "failed"
		}
	case "failed":
		state = "failed"
	}
	if state == "" || state == dispatch.State {
		return nil
	}
	active, err := repos.AgentExecutions.ListActive(ctx)
	if err != nil {
		return err
	}
	for _, execution := range active {
		if execution.LoopID != nil && *execution.LoopID == loop.ID {
			return nil
		}
	}
	claim, err := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if claim != nil && owner.OwnsQueueClaim(claim.ID) {
		return nil
	}
	return s.client.TransitionStrictDispatch(ctx, dispatch.ID, state, waitKind)
}

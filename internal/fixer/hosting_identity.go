package fixer

import (
	"context"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/storage"
)

func (r *Runner) bindClaimedHostingIdentity(ctx context.Context, item storage.QueueItemRecord) (context.Context, error) {
	projectID := derefString(item.ProjectID)
	if projectID == "" && item.LoopID != nil && r.repos != nil && r.repos.Loops != nil {
		loop, err := r.repos.Loops.GetByID(ctx, *item.LoopID)
		if err != nil {
			return nil, err
		}
		if loop != nil {
			projectID = loop.ProjectID
		}
	}
	return hostingidentity.Bind(ctx, r.customInstructions, projectID, "fixer")
}

func hostingKindForContext(ctx context.Context) config.HostingIdentityKind {
	if session, selected := hostingidentity.FromContext(ctx); selected {
		return session.Kind()
	}
	return ""
}

func botPublicationPrompt() string {
	return "Hosting bot execution: make the requested local changes, run the required validation, and commit relevant changes locally. The Looper daemon owns any configured branch push and pull-request publication after validation using the selected bot. Do not push, create/edit PRs, change labels/reviewers, or publish review state from agent commands. Keep the normal structured completion and repair decisions; git_pr_lifecycle is optional and may describe local commits only."
}

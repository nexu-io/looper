package reviewer

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
	return hostingidentity.Bind(ctx, r.customInstructions, projectID, "reviewer")
}

func hostingKindForContext(ctx context.Context) config.HostingIdentityKind {
	if session, selected := hostingidentity.FromContext(ctx); selected {
		return session.Kind()
	}
	return ""
}

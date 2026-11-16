package runtime

import (
	"context"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/workerhost"
	"time"
)

const shepherdReconcileInterval = 60 * time.Second

var foldShepherdSignal = workerhost.FoldShepherdSignal

func (r *Runtime) workerShepherd() *workerhost.Shepherd {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return &workerhost.Shepherd{Config: r.config, Repos: r.services.Repositories, Now: r.now, Logger: r.logger, GitHub: r.githubGateway, Wake: r.TriggerSchedulerClaim}
}
func (r *Runtime) reconcileWorkerShepherd(ctx context.Context) { r.workerShepherd().Reconcile(ctx) }
func (r *Runtime) ReconcilePullRequest(ctx context.Context, projectID, repo string, number int64) error {
	return r.workerShepherd().ReconcilePullRequest(ctx, projectID, repo, number)
}
func (r *Runtime) shepherdNotifyGateway() (*notify.Gateway, bool) {
	return r.workerShepherd().NotifyGateway()
}

var shepherdCIPhase = workerhost.ShepherdCIPhase

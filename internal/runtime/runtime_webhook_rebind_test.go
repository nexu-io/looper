package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/webhookforward"
)

func TestSyncRuntimeProjectBindingDrainsAcceptedGitHubWorkAcrossForgejoRebind(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
	t.Cleanup(func() { _ = coordinator.Close() })
	repositories := storage.NewRepositories(coordinator.DB())
	metadata := `{"repo":"acme/looper"}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: workingDir, MetadataJSON: &metadata, CreatedAt: "2026-07-12T12:00:00.000Z", UpdatedAt: "2026-07-12T12:00:00.000Z"}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Discovery.AutoDiscovery = true
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test"}}
	githubRunner := &rebindBlockingReviewer{started: make(chan struct{}), release: make(chan struct{})}
	previous := webhookforward.New(webhookforward.Options{Repos: repositories, Config: cfg, Reviewer: githubRunner, MaxConcurrent: 1, QueueCapacity: 8})
	next := &trackingRuntimeWebhookForwarder{}
	rt := &Runtime{
		config:           cfg,
		webhookForwarder: previous,
		webhookForwarderForConfig: func(config.Config) WebhookForwarder {
			return next
		},
	}

	if _, err := previous.Forward(context.Background(), webhookforward.DeliveryRequest{DeliveryID: "running-github", EventType: "pull_request", Payload: []byte(`{"action":"review_requested","repository":{"full_name":"acme/looper"},"pull_request":{"number":1}}`)}); err != nil {
		t.Fatalf("Forward(running-github) error = %v", err)
	}
	select {
	case <-githubRunner.started:
	case <-time.After(time.Second):
		t.Fatal("running GitHub delivery did not start")
	}
	if _, err := previous.Forward(context.Background(), webhookforward.DeliveryRequest{DeliveryID: "queued-github", EventType: "pull_request", Payload: []byte(`{"action":"review_requested","repository":{"full_name":"acme/looper"},"pull_request":{"number":2}}`)}); err != nil {
		t.Fatalf("Forward(queued-github) error = %v", err)
	}

	rebound := make(chan struct{})
	go func() {
		rt.syncRuntimeProjectBinding(projects.ProjectBinding{ProjectID: "project_1", Name: "Looper", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: workingDir})
		close(rebound)
	}()
	select {
	case <-rebound:
	case <-time.After(time.Second):
		t.Fatal("GitHub to Forgejo rebind blocked on old webhook work")
	}
	close(githubRunner.release)
	deadline := time.Now().Add(time.Second)
	for githubRunner.callCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := githubRunner.callCount(); got != 2 {
		t.Fatalf("old GitHub runner call count = %d, want 2; accepted queued delivery was lost during Forgejo rebind", got)
	}
	if got := rt.WebhookForwarder(); got != next {
		t.Fatalf("WebhookForwarder() = %T, want Forgejo replacement", got)
	}
}

type rebindBlockingReviewer struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (r *rebindBlockingReviewer) DiscoverPullRequest(context.Context, reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	r.mu.Lock()
	r.calls++
	if r.calls == 1 {
		close(r.started)
	}
	r.mu.Unlock()
	<-r.release
	return reviewer.DiscoveryResult{}, nil
}

func (r *rebindBlockingReviewer) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

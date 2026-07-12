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

func TestRuntimeStopKeepsStorageOpenUntilWebhookWorkDrains(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
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
	runner := &storageCheckingReviewer{repositories: repositories, started: make(chan struct{}), release: make(chan struct{}), result: make(chan error, 1)}
	forwarder := webhookforward.New(webhookforward.Options{Repos: repositories, Config: cfg, Reviewer: runner, MaxConcurrent: 1, QueueCapacity: 8})
	rt := New(Options{Config: cfg})
	rt.services = Services{Coordinator: coordinator, Repositories: repositories}
	rt.webhookForwarder = forwarder

	if _, err := forwarder.Forward(context.Background(), webhookforward.DeliveryRequest{DeliveryID: "running-github", EventType: "pull_request", Payload: []byte(`{"action":"review_requested","repository":{"full_name":"acme/looper"},"pull_request":{"number":1}}`)}); err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("webhook delivery did not start")
	}

	stopped := make(chan struct{})
	go func() {
		rt.Stop("test")
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Runtime.Stop() returned before webhook work drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(runner.release)
	select {
	case err := <-runner.result:
		if err != nil {
			t.Fatalf("Projects.List() while draining error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook work did not access storage")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Runtime.Stop() did not return after webhook work drained")
	}
}

func TestRuntimeStopCancelsReboundWebhookWorkBeforeClosingStorage(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), filepath.Join(workingDir, "backups"))
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
	runner := &cancelCheckingReviewer{repositories: repositories, started: make(chan struct{}), result: make(chan error, 1)}
	previous := webhookforward.New(webhookforward.Options{Repos: repositories, Config: cfg, Reviewer: runner, MaxConcurrent: 1, QueueCapacity: 8})
	rt := New(Options{Config: cfg})
	rt.services = Services{Coordinator: coordinator, Repositories: repositories}
	rt.webhookForwarder = previous
	rt.webhookForwarderForConfig = func(config.Config) WebhookForwarder { return &trackingRuntimeWebhookForwarder{} }

	if _, err := previous.Forward(context.Background(), webhookforward.DeliveryRequest{DeliveryID: "running-github", EventType: "pull_request", Payload: []byte(`{"action":"review_requested","repository":{"full_name":"acme/looper"},"pull_request":{"number":1}}`)}); err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("webhook delivery did not start")
	}
	rt.syncRuntimeProjectBinding(projects.ProjectBinding{ProjectID: "project_1", Name: "Looper", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: workingDir})

	rt.Stop("test")
	select {
	case err := <-runner.result:
		if err != nil {
			t.Fatalf("Projects.List() after cancellation error = %v; storage closed before rebound work joined", err)
		}
	case <-time.After(time.Second):
		t.Fatal("rebound webhook work was not canceled during shutdown")
	}
}

type rebindBlockingReviewer struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

type storageCheckingReviewer struct {
	repositories *storage.Repositories
	started      chan struct{}
	release      chan struct{}
	result       chan error
}

type cancelCheckingReviewer struct {
	repositories *storage.Repositories
	started      chan struct{}
	result       chan error
}

func (r *cancelCheckingReviewer) DiscoverPullRequest(ctx context.Context, _ reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	close(r.started)
	<-ctx.Done()
	_, err := r.repositories.Projects.List(context.Background())
	r.result <- err
	return reviewer.DiscoveryResult{}, ctx.Err()
}

func (r *storageCheckingReviewer) DiscoverPullRequest(ctx context.Context, _ reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	close(r.started)
	<-r.release
	_, err := r.repositories.Projects.List(ctx)
	r.result <- err
	return reviewer.DiscoveryResult{}, err
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

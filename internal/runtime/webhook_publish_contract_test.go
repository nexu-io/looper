package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/projects"
)

// Contracts for deferred webhook reconcile after project catalog publication.
// Kept out of runtime_test.go / webhook_runtime_test.go to avoid propping up
// the lifecycle boundary inside those already-broad suites.

func TestRuntimeProjectAddDoesNotWaitForWebhookReconciliation(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Webhook.ListenPort = 0
	cfg.Webhook.PublicBaseURL = "https://looper.example.test"
	ghPath := "/usr/bin/gh"
	cfg.Tools.GHPath = &ghPath

	rt := New(Options{
		Config:           cfg,
		Logger:           &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	reconcileStarted := make(chan struct{})
	releaseReconcile := make(chan struct{})
	defer close(releaseReconcile)
	rt.webhook.bootstrapDone = true
	rt.webhook.tunnelClient = blockingWebhookTunnelGitHubClient{
		started: reconcileStarted,
		release: releaseReconcile,
	}

	projectService := rt.Services().Projects
	projectService.ListWorktrees = nil
	repo := "acme/live"
	addDone := make(chan error, 1)
	go func() {
		_, err := projectService.AddProject(context.Background(), projects.AddInput{
			ID: "live", Name: "Live", RepoPath: workingDir, Repo: &repo, SnapshotMode: projects.SnapshotModeOff,
		})
		addDone <- err
	}()

	select {
	case <-reconcileStarted:
	case <-time.After(time.Second):
		t.Fatal("webhook reconciliation did not start")
	}
	select {
	case err := <-addDone:
		if err != nil {
			t.Fatalf("AddProject() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("AddProject() waited for webhook reconciliation")
	}
}

// Contract: while tunnel reconcile from mutation A is blocked on GitHub, mutation B
// may publish the catalog (updateConfig) without racing the in-flight pass.
// Each Reconcile uses the config snapshot captured at pass start; the coalesced
// follow-up pass reconciles the second project after the first unblocks.
func TestRuntimeSecondProjectPublishDuringBlockedTunnelReconcile(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	secondDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = config.WebhookModeTunnel
	cfg.Webhook.ListenPort = 0
	cfg.Webhook.PublicBaseURL = "https://looper.example.test"
	ghPath := "/usr/bin/gh"
	cfg.Tools.GHPath = &ghPath

	rt := New(Options{
		Config:           cfg,
		Logger:           &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondCreateSeen := make(chan struct{})
	client := &recordingBlockingTunnelClient{
		firstStarted:     firstStarted,
		firstRelease:     releaseFirst,
		secondCreateSeen: secondCreateSeen,
	}
	rt.webhook.bootstrapDone = true
	rt.webhook.tunnelClient = client

	projectService := rt.Services().Projects
	projectService.ListWorktrees = nil
	repoA := "acme/first"
	if _, err := projectService.AddProject(context.Background(), projects.AddInput{
		ID: "first", Name: "First", RepoPath: workingDir, Repo: &repoA, SnapshotMode: projects.SnapshotModeOff,
	}); err != nil {
		t.Fatalf("AddProject(first) error = %v", err)
	}

	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first tunnel reconcile CreateHook did not start")
	}

	if got := client.createRepos(); len(got) != 1 || got[0] != repoA {
		t.Fatalf("creates while first blocked = %v, want only %q", got, repoA)
	}

	repoB := "acme/second"
	addSecondDone := make(chan error, 1)
	go func() {
		_, err := projectService.AddProject(context.Background(), projects.AddInput{
			ID: "second", Name: "Second", RepoPath: secondDir, Repo: &repoB, SnapshotMode: projects.SnapshotModeOff,
		})
		addSecondDone <- err
	}()

	select {
	case err := <-addSecondDone:
		if err != nil {
			t.Fatalf("AddProject(second) error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AddProject(second) waited for blocked first tunnel reconciliation")
	}

	if got := client.createRepos(); len(got) != 1 || got[0] != repoA {
		t.Fatalf("creates after second publish while first blocked = %v, want only %q", got, repoA)
	}

	close(releaseFirst)

	select {
	case <-secondCreateSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("coalesced reconcile did not CreateHook for second project")
	}

	got := client.createRepos()
	if len(got) < 2 || got[0] != repoA {
		t.Fatalf("create order = %v, want first %q then later %q", got, repoA, repoB)
	}
	foundB := false
	for _, repo := range got[1:] {
		if repo == repoB {
			foundB = true
			break
		}
	}
	if !foundB {
		t.Fatalf("create order = %v, want %q after first pass unblocked", got, repoB)
	}
}

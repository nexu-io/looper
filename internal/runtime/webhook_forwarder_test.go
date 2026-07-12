package runtime

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestUniqueConfiguredWebhookRepos(t *testing.T) {
	t.Parallel()

	projects := []storage.ProjectRecord{
		{ID: "a", MetadataJSON: stringPtr(`{"repo":"acme/alpha"}`)},
		{ID: "b", MetadataJSON: stringPtr(`{"repo":"acme/alpha"}`)},
		{ID: "c", MetadataJSON: stringPtr(`{"repo":"acme/beta"}`)},
		{ID: "d", Archived: true, MetadataJSON: stringPtr(`{"repo":"acme/ignored"}`)},
		{ID: "e", MetadataJSON: stringPtr(`{"repo":null}`)},
	}

	got := uniqueConfiguredWebhookRepos(projects)
	want := []string{"acme/alpha", "acme/beta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("uniqueConfiguredWebhookRepos() = %v, want %v", got, want)
	}
}

func TestWebhookForwardEndpointDegradedCases(t *testing.T) {
	t.Parallel()

	t.Run("missing gh", func(t *testing.T) {
		cfg, err := config.DefaultConfig(t.TempDir())
		if err != nil {
			t.Fatalf("DefaultConfig() error = %v", err)
		}
		cfg.Webhook.Enabled = true

		endpoint, reasons := webhookForwardEndpoint(cfg)
		if endpoint != nil {
			t.Fatalf("webhookForwardEndpoint() endpoint = %v, want nil", *endpoint)
		}
		if len(reasons) != 1 || reasons[0] != "configured/resolved gh executable is missing" {
			t.Fatalf("webhookForwardEndpoint() reasons = %v", reasons)
		}
	})

	t.Run("non loopback host", func(t *testing.T) {
		cfg, err := config.DefaultConfig(t.TempDir())
		if err != nil {
			t.Fatalf("DefaultConfig() error = %v", err)
		}
		cfg.Webhook.Enabled = true
		cfg.Server.Host = "0.0.0.0"
		ghPath := "/usr/bin/gh"
		cfg.Tools.GHPath = &ghPath

		endpoint, reasons := webhookForwardEndpoint(cfg)
		if endpoint != nil {
			t.Fatalf("webhookForwardEndpoint() endpoint = %v, want nil", *endpoint)
		}
		if len(reasons) != 1 || !strings.Contains(reasons[0], "not loopback") {
			t.Fatalf("webhookForwardEndpoint() reasons = %v, want non-loopback reason", reasons)
		}
	})

	t.Run("loopback endpoint", func(t *testing.T) {
		cfg, err := config.DefaultConfig(t.TempDir())
		if err != nil {
			t.Fatalf("DefaultConfig() error = %v", err)
		}
		cfg.Webhook.Enabled = true
		ghPath := "/usr/bin/gh"
		cfg.Tools.GHPath = &ghPath

		endpoint, reasons := webhookForwardEndpoint(cfg)
		if len(reasons) != 0 {
			t.Fatalf("webhookForwardEndpoint() reasons = %v, want none", reasons)
		}
		if endpoint == nil || *endpoint != "http://127.0.0.1:17310/webhook/forward" {
			t.Fatalf("webhookForwardEndpoint() endpoint = %v, want loopback webhook URL", endpoint)
		}
	})
}

func TestWebhookForwarderManagerStartsUniqueReposAndRetainsTail(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Enabled = true
	ghPath := "/resolved/gh"
	cfg.Tools.GHPath = &ghPath

	startedCh := make(chan *testStartedForwarder, 4)
	manager := newWebhookForwarderManager(webhookForwarderManagerOptions{
		Config:      cfg,
		Logger:      &testLogger{},
		Now:         func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
		BackoffBase: time.Millisecond,
		BackoffMax:  2 * time.Millisecond,
		StartProcess: func(ctx context.Context, command webhookForwarderCommand) (webhookForwarderStartResult, error) {
			started := newTestStartedForwarder(command)
			result := started.result()
			startedCh <- started
			return result, nil
		},
	})

	projects := []storage.ProjectRecord{
		{ID: "one", MetadataJSON: stringPtr(`{"repo":"acme/alpha"}`)},
		{ID: "two", MetadataJSON: stringPtr(`{"repo":"acme/alpha"}`)},
		{ID: "three", MetadataJSON: stringPtr(`{"repo":"acme/beta"}`)},
	}
	manager.Sync(context.Background(), projects)
	t.Cleanup(manager.Stop)

	started := []*testStartedForwarder{<-startedCh, <-startedCh}
	sort.Slice(started, func(i, j int) bool { return started[i].command.Repo < started[j].command.Repo })
	if got := started[0].command.Repo; got != "acme/alpha" {
		t.Fatalf("first forwarder repo = %q, want acme/alpha", got)
	}
	if got := started[1].command.Repo; got != "acme/beta" {
		t.Fatalf("second forwarder repo = %q, want acme/beta", got)
	}
	for _, item := range started {
		if item.command.Path != ghPath {
			t.Fatalf("forwarder gh path = %q, want %q", item.command.Path, ghPath)
		}
		if got := strings.Join(item.command.Events, ","); got != strings.Join(webhookForwarderEvents, ",") {
			t.Fatalf("forwarder events = %q, want %q", got, strings.Join(webhookForwarderEvents, ","))
		}
		if item.command.URL != "http://127.0.0.1:17310/webhook/forward" {
			t.Fatalf("forwarder URL = %q, want loopback webhook URL", item.command.URL)
		}
	}

	started[0].writeStdout("hello from stdout\n")
	started[0].writeStderr("warning from stderr\n")
	waitForWebhookCondition(t, time.Second, func() bool {
		status := manager.Status()
		for _, forwarder := range status.Forwarders {
			if forwarder.Repo == "acme/alpha" && len(forwarder.Tail) == 2 {
				return true
			}
		}
		return false
	})

	status := manager.Status()
	if !status.Enabled || !status.Healthy || status.Degraded {
		t.Fatalf("manager.Status() = %#v, want enabled healthy non-degraded", status)
	}
	if len(status.Forwarders) != 2 {
		t.Fatalf("forwarder count = %d, want 2", len(status.Forwarders))
	}
	alpha := status.Forwarders[0]
	if alpha.Repo != "acme/alpha" {
		alpha = status.Forwarders[1]
	}
	if !alpha.Running {
		t.Fatal("alpha forwarder Running = false, want true")
	}
	if got := strings.Join(alpha.Tail, "\n"); !strings.Contains(got, "stdout: hello from stdout") || !strings.Contains(got, "stderr: warning from stderr") {
		t.Fatalf("alpha tail = %q, want stdout/stderr lines retained", got)
	}
}

func TestWebhookForwarderEventsIncludePushAndCheckRun(t *testing.T) {
	t.Parallel()
	if !slices.Contains(webhookForwarderEvents, "push") {
		t.Fatalf("webhookForwarderEvents = %v, want push subscription", webhookForwarderEvents)
	}
	if !slices.Contains(webhookForwarderEvents, "check_run") {
		t.Fatalf("webhookForwarderEvents = %v, want check_run subscription", webhookForwarderEvents)
	}
}

func TestClassifyForwarderExitTerminalPatterns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		stderr  []string
		pattern string
	}{
		{name: "hook already exists", stderr: []string{"Error: error creating webhook: HTTP 422: Validation Failed", "Hook already exists on this repository"}, pattern: "Hook already exists on this repository"},
		{name: "auth", stderr: []string{"authentication required; run gh auth login"}, pattern: "authentication required"},
		{name: "forbidden", stderr: []string{"HTTP 403: Resource not accessible by integration"}, pattern: "HTTP 403"},
		{name: "not found", stderr: []string{"HTTP 404"}, pattern: "HTTP 404"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyForwarderExit(tc.stderr, errors.New("exit status 1"))
			if got.Class != forwarderExitTerminal || got.MatchedPattern != tc.pattern {
				t.Fatalf("classifyForwarderExit() = %#v, want terminal %q", got, tc.pattern)
			}
		})
	}
}

func TestCommandFingerprintCanonicalizesEvents(t *testing.T) {
	t.Parallel()
	one, eventsOne := commandFingerprint("/bin/gh", "nexu-io/looper", []string{"Push", "issue_comment"}, "http://127.0.0.1:17310/webhook/forward")
	two, eventsTwo := commandFingerprint("/bin/gh", "nexu-io/looper", []string{"issue_comment", "push"}, "http://127.0.0.1:17310/webhook/forward")
	if one != two || eventsOne != "issue_comment,push" || eventsTwo != eventsOne {
		t.Fatalf("fingerprints/events = %q/%q and %q/%q, want equal canonical", one, eventsOne, two, eventsTwo)
	}
	changed, _ := commandFingerprint("/bin/gh", "nexu-io/looper", []string{"issue_comment", "push"}, "http://localhost:17310/webhook/forward")
	if changed == one {
		t.Fatal("commandFingerprint endpoint change did not change fingerprint")
	}
}

func TestCommandIdentityPartsParsesSplitAndEqualsArgs(t *testing.T) {
	t.Parallel()
	gh, endpoint, events := commandIdentityParts([]string{"/bin/gh", "webhook", "forward", "--repo", "nexu-io/looper", "--events", "push,issue_comment", "--url", "http://127.0.0.1:17310/webhook/forward"})
	if gh != "/bin/gh" || endpoint != "http://127.0.0.1:17310/webhook/forward" || strings.Join(events, ",") != "push,issue_comment" {
		t.Fatalf("commandIdentityParts(split) = %q %q %v", gh, endpoint, events)
	}
	if !argvMatchesWebhookForward([]string{"/bin/gh", "webhook", "forward", "--repo=nexu-io/looper", "--events=issue_comment,push", "--url=http://127.0.0.1:17310/webhook/forward"}, "nexu-io/looper", []string{"push", "issue_comment"}, "http://127.0.0.1:17310/webhook/forward") {
		t.Fatal("argvMatchesWebhookForward rejected exact command")
	}
	if argvMatchesWebhookForward([]string{"/bin/gh", "webhook", "forward", "--repo", "nexu-io/looper-old", "--events", "issue_comment,push", "--url", "http://127.0.0.1:17310/webhook/forward"}, "nexu-io/looper", []string{"push", "issue_comment"}, "http://127.0.0.1:17310/webhook/forward") {
		t.Fatal("argvMatchesWebhookForward accepted prefix repo mismatch")
	}
}

func TestWebhookForwarderManagerRespawnsAndStopsRemovedRepo(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Enabled = true
	ghPath := "/resolved/gh"
	cfg.Tools.GHPath = &ghPath

	var mu sync.Mutex
	started := []*testStartedForwarder{}
	sleepCalls := 0
	manager := newWebhookForwarderManager(webhookForwarderManagerOptions{
		Config:      cfg,
		Logger:      &testLogger{},
		Now:         func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
		BackoffBase: time.Millisecond,
		BackoffMax:  2 * time.Millisecond,
		Sleep: func(ctx context.Context, delay time.Duration) error {
			mu.Lock()
			sleepCalls++
			mu.Unlock()
			return nil
		},
		StartProcess: func(ctx context.Context, command webhookForwarderCommand) (webhookForwarderStartResult, error) {
			startedForwarder := newTestStartedForwarder(command)
			result := startedForwarder.result()
			mu.Lock()
			started = append(started, startedForwarder)
			count := len(started)
			mu.Unlock()
			if count == 1 {
				startedForwarder.exit(errors.New("boom"))
			}
			return result, nil
		},
	})

	manager.Sync(context.Background(), []storage.ProjectRecord{{ID: "one", MetadataJSON: stringPtr(`{"repo":"acme/alpha"}`)}})
	waitForWebhookCondition(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) >= 2 && sleepCalls >= 1
	})

	status := manager.Status()
	if len(status.Forwarders) != 1 {
		t.Fatalf("forwarder count = %d, want 1", len(status.Forwarders))
	}
	if status.Forwarders[0].RespawnCount != 1 {
		t.Fatalf("respawn count = %d, want 1", status.Forwarders[0].RespawnCount)
	}

	manager.Sync(context.Background(), nil)
	waitForWebhookCondition(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) >= 2 && started[1].process.stopCalls() == 1
	})
	status = manager.Status()
	if len(status.Forwarders) != 0 {
		t.Fatalf("forwarder count after removal = %d, want 0", len(status.Forwarders))
	}
	manager.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 2 {
		t.Fatalf("start count = %d, want 2 without respawn after removal", len(started))
	}
}

func TestWebhookForwarderManagerResetsBackoffAfterSuccessfulStart(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Enabled = true
	ghPath := "/resolved/gh"
	cfg.Tools.GHPath = &ghPath

	var mu sync.Mutex
	started := []*testStartedForwarder{}
	startCalls := 0
	sleepCalls := []time.Duration{}
	manager := newWebhookForwarderManager(webhookForwarderManagerOptions{
		Config:      cfg,
		Logger:      &testLogger{},
		Now:         func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
		BackoffBase: time.Millisecond,
		BackoffMax:  4 * time.Millisecond,
		Sleep: func(ctx context.Context, delay time.Duration) error {
			mu.Lock()
			sleepCalls = append(sleepCalls, delay)
			mu.Unlock()
			return nil
		},
		StartProcess: func(ctx context.Context, command webhookForwarderCommand) (webhookForwarderStartResult, error) {
			mu.Lock()
			attempt := startCalls
			startCalls++
			mu.Unlock()
			if attempt < 3 {
				return webhookForwarderStartResult{}, errors.New("gh unavailable")
			}
			startedForwarder := newTestStartedForwarder(command)
			result := startedForwarder.result()
			mu.Lock()
			started = append(started, startedForwarder)
			mu.Unlock()
			if attempt == 3 || attempt == 4 {
				go startedForwarder.exit(errors.New("boom"))
			}
			return result, nil
		},
	})

	manager.Sync(context.Background(), []storage.ProjectRecord{{ID: "one", MetadataJSON: stringPtr(`{"repo":"acme/alpha"}`)}})
	waitForWebhookCondition(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(sleepCalls) < 5 {
			return false
		}
		return sleepCalls[0] == time.Millisecond && sleepCalls[1] == 2*time.Millisecond && sleepCalls[2] == 4*time.Millisecond && sleepCalls[3] == time.Millisecond && sleepCalls[4] == 2*time.Millisecond
	})
	manager.Stop()
}

func TestRuntimeStartDegradesWebhookWithoutFailingStartup(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Webhook.Enabled = true
	cfg.Server.Host = "0.0.0.0"
	ghPath := "/resolved/gh"
	cfg.Tools.GHPath = &ghPath

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil despite degraded webhook runtime", err)
	}
	defer rt.Stop("test cleanup")

	status := rt.WebhookStatus()
	if !status.Enabled || !status.Degraded {
		t.Fatalf("WebhookStatus() = %#v, want enabled degraded status", status)
	}
	if len(status.Forwarders) != 0 {
		t.Fatalf("WebhookStatus().Forwarders = %v, want none when host is non-loopback", status.Forwarders)
	}
	if len(status.DegradedReasons) == 0 || !strings.Contains(status.DegradedReasons[0], "not loopback") {
		t.Fatalf("WebhookStatus().DegradedReasons = %v, want non-loopback reason", status.DegradedReasons)
	}
}

func TestCloseWebhookForwarderPipesClosesAllNonNilClosers(t *testing.T) {
	t.Parallel()
	stdout := &testCloser{}
	stderr := &testCloser{}

	closeWebhookForwarderPipes(stdout, nil, stderr)

	if stdout.closeCalls != 1 {
		t.Fatalf("stdout close calls = %d, want 1", stdout.closeCalls)
	}
	if stderr.closeCalls != 1 {
		t.Fatalf("stderr close calls = %d, want 1", stderr.closeCalls)
	}
}

type testStartedForwarder struct {
	command webhookForwarderCommand
	process *testWebhookForwarderProcess
	stdoutW *io.PipeWriter
	stderrW *io.PipeWriter
}

type testCloser struct{ closeCalls int }

func (c *testCloser) Close() error {
	c.closeCalls++
	return nil
}

func newTestStartedForwarder(command webhookForwarderCommand) *testStartedForwarder {
	return &testStartedForwarder{
		command: command,
		process: newTestWebhookForwarderProcess(),
	}
}

func (t *testStartedForwarder) result() webhookForwarderStartResult {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	t.stdoutW = stdoutW
	t.stderrW = stderrW
	t.process.onStop = func() {
		_ = stdoutW.Close()
		_ = stderrW.Close()
	}
	return webhookForwarderStartResult{process: t.process, stdout: stdoutR, stderr: stderrR}
}

func (t *testStartedForwarder) writeStdout(line string) {
	_, _ = io.WriteString(t.stdoutW, line)
}

func (t *testStartedForwarder) writeStderr(line string) {
	_, _ = io.WriteString(t.stderrW, line)
}

func (t *testStartedForwarder) exit(err error) {
	t.process.exit(err)
	_ = t.stdoutW.Close()
	_ = t.stderrW.Close()
}

type testWebhookForwarderProcess struct {
	mu        sync.Mutex
	waitCh    chan error
	stopCount int
	onStop    func()
}

func newTestWebhookForwarderProcess() *testWebhookForwarderProcess {
	return &testWebhookForwarderProcess{waitCh: make(chan error, 1)}
}

func (p *testWebhookForwarderProcess) Wait() error {
	return <-p.waitCh
}

func (p *testWebhookForwarderProcess) Stop() error {
	p.mu.Lock()
	p.stopCount++
	onStop := p.onStop
	p.mu.Unlock()
	if onStop != nil {
		onStop()
	}
	p.exit(nil)
	return nil
}

func (p *testWebhookForwarderProcess) Kill() error {
	p.exit(nil)
	return nil
}

func (p *testWebhookForwarderProcess) exit(err error) {
	select {
	case p.waitCh <- err:
	default:
	}
}

func (p *testWebhookForwarderProcess) stopCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopCount
}

func waitForWebhookCondition(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

package modelcatalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/processcontainment"
)

func TestLoadStaticCatalog(t *testing.T) {
	catalog := loadStaticCatalog()
	claude := catalog[config.AgentVendorClaudeCode]
	if len(claude) < 3 {
		t.Fatalf("claude-code static models = %d, want at least sonnet/opus/haiku", len(claude))
	}
	ids := map[string]bool{}
	for _, m := range claude {
		ids[m.ID] = true
		if m.Label == "" {
			t.Fatalf("empty label for %q", m.ID)
		}
	}
	for _, want := range []string{"sonnet", "opus", "haiku"} {
		if !ids[want] {
			t.Fatalf("claude-code missing alias %q", want)
		}
	}
	for _, vendor := range []config.AgentVendor{
		config.AgentVendorCodex,
		config.AgentVendorOpenCode,
		config.AgentVendorCursorCLI,
		config.AgentVendorGrokBuild,
	} {
		if len(catalog[vendor]) == 0 {
			t.Fatalf("static catalog empty for %s", vendor)
		}
	}
}

func TestMergeModelsStaticFirstProbeLabelWinsDedupe(t *testing.T) {
	static := []Model{
		{ID: "gpt-5.4", Label: "Static GPT", Source: SourceStatic},
		{ID: "o3", Label: "o3", Source: SourceStatic},
	}
	probe := []Model{
		{ID: "gpt-5.4", Label: "Probe GPT-5.4", Source: SourceProbe},
		{ID: "gpt-4.1", Label: "GPT-4.1", Source: SourceProbe},
		{ID: "alpha-only", Label: "Alpha", Source: SourceProbe},
		{ID: "gpt-4.1", Label: "dup", Source: SourceProbe},
	}
	got := mergeModels(static, probe)
	want := []Model{
		{ID: "gpt-5.4", Label: "Probe GPT-5.4", Source: SourceMerged},
		{ID: "o3", Label: "o3", Source: SourceStatic},
		{ID: "alpha-only", Label: "Alpha", Source: SourceProbe},
		{ID: "gpt-4.1", Label: "GPT-4.1", Source: SourceProbe},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeModels() = %#v, want %#v", got, want)
	}
}

func TestMergeModelsBareProbeIDPreservesStaticLabel(t *testing.T) {
	static := []Model{
		{ID: "composer-1", Label: "Composer 1", Source: SourceStatic},
		{ID: "gpt-5", Label: "GPT-5", Source: SourceStatic},
	}
	// Bare probe ids: empty Label means "no label from probe".
	probe := []Model{
		{ID: "composer-1", Source: SourceProbe},
		{ID: "gpt-5", Label: "", Source: SourceProbe},
		{ID: "new-only", Source: SourceProbe},
	}
	got := mergeModels(static, probe)
	want := []Model{
		{ID: "composer-1", Label: "Composer 1", Source: SourceMerged},
		{ID: "gpt-5", Label: "GPT-5", Source: SourceMerged},
		{ID: "new-only", Label: "new-only", Source: SourceProbe},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeModels() = %#v, want %#v", got, want)
	}
}

func TestParseOpenCodeModelsFixture(t *testing.T) {
	raw := readTestdata(t, "opencode_models.txt")
	got := parseOpenCodeModels(raw)
	wantIDs := []string{
		"anthropic/claude-sonnet-4-5",
		"anthropic/claude-opus-4-5",
		"openai/gpt-5.4",
		"openai/gpt-4.1",
		"google/gemini-2.5-pro",
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("len = %d, want %d (%#v)", len(got), len(wantIDs), got)
	}
	for i, id := range wantIDs {
		if got[i].ID != id || got[i].Source != SourceProbe {
			t.Fatalf("got[%d] = %#v, want id=%q source=probe", i, got[i], id)
		}
		if got[i].Label != "" {
			t.Fatalf("got[%d].Label = %q, want empty bare-id label", i, got[i].Label)
		}
	}
}

func TestParseOpenCodeRejectsMultiwordDiagnostics(t *testing.T) {
	raw := []byte("Not logged in\nanthropic/claude-sonnet-4-5\nPlease run opencode auth\nopenai/gpt-4.1\n")
	got := parseOpenCodeModels(raw)
	if !reflect.DeepEqual(modelIDs(got), []string{"anthropic/claude-sonnet-4-5", "openai/gpt-4.1"}) {
		t.Fatalf("ids = %#v", modelIDs(got))
	}
}

func TestParseCodexModelsFixtureFiltersVisibility(t *testing.T) {
	raw := readTestdata(t, "codex_models.json")
	got, err := parseCodexModels(raw)
	if err != nil {
		t.Fatalf("parseCodexModels() error = %v", err)
	}
	ids := modelIDs(got)
	if reflect.DeepEqual(ids, []string{"gpt-5.4", "gpt-5.3-codex", "o3"}) == false {
		t.Fatalf("ids = %#v, want list-visible only (no experimental-hidden)", ids)
	}
	if got[0].Label != "GPT-5.4" {
		t.Fatalf("label = %q, want GPT-5.4", got[0].Label)
	}
}

func TestParseCursorAndGrokFixtures(t *testing.T) {
	cursor := parseCursorModels(readTestdata(t, "cursor_models.txt"))
	if got := modelIDs(cursor); !reflect.DeepEqual(got, []string{"auto", "sonnet-4", "gpt-5", "composer-1"}) {
		t.Fatalf("cursor ids = %#v", got)
	}
	if cursor[0].Label != "Auto" {
		t.Fatalf("cursor label = %q, want Auto", cursor[0].Label)
	}
	// Bare id line: no fabricated label.
	if cursor[3].ID != "composer-1" || cursor[3].Label != "" {
		t.Fatalf("composer-1 entry = %#v, want empty label", cursor[3])
	}
	grok := parseGrokModels(readTestdata(t, "grok_models.txt"))
	if got := modelIDs(grok); !reflect.DeepEqual(got, []string{"grok-4", "grok-3", "grok-code-fast-1"}) {
		t.Fatalf("grok ids = %#v", got)
	}
}

func TestParseLineModelsRejectsLoggedOutWarning(t *testing.T) {
	raw := []byte("Not logged in\nPlease authenticate first\nauto - Auto\n")
	got := parseLineModels(raw)
	if !reflect.DeepEqual(modelIDs(got), []string{"auto"}) {
		t.Fatalf("ids = %#v, want only table row", modelIDs(got))
	}
}

func TestListClaudeCodeUnsupportedProbeStaticOnly(t *testing.T) {
	var calls atomic.Int32
	svc := NewService(Options{
		Runner: runnerFunc(func(context.Context, []string, string, ...string) ([]byte, error) {
			calls.Add(1)
			return nil, errors.New("should not probe")
		}),
		LookPath: func(s string) (string, error) { return "/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorClaudeCode})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("probe calls = %d, want 0", calls.Load())
	}
	if got.Sources.Probe != ProbeUnsupported {
		t.Fatalf("probe = %q, want unsupported", got.Sources.Probe)
	}
	if !got.Sources.Static || len(got.Models) == 0 {
		t.Fatalf("expected static models, got %#v", got)
	}
	if got.ProbedAt != "" {
		t.Fatalf("probedAt = %q, want empty when unsupported", got.ProbedAt)
	}
	for _, m := range got.Models {
		if m.Source != SourceStatic {
			t.Fatalf("model source = %q, want static", m.Source)
		}
	}
}

func TestListProbeFailureReturnsStaticAndError(t *testing.T) {
	svc := NewService(Options{
		Runner: runnerFunc(func(context.Context, []string, string, ...string) ([]byte, error) {
			return nil, errors.New("exit status 1: not logged in")
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorCodex})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got.Sources.Probe != ProbeError {
		t.Fatalf("probe = %q, want error", got.Sources.Probe)
	}
	if !strings.Contains(got.Sources.ProbeError, "not logged in") {
		t.Fatalf("probeError = %q", got.Sources.ProbeError)
	}
	if !got.Sources.Static || len(got.Models) == 0 {
		t.Fatalf("expected static fallback, got %#v", got)
	}
	if got.ProbedAt != "2026-08-01T12:00:00Z" {
		t.Fatalf("probedAt = %q", got.ProbedAt)
	}
}

func TestListProbeErrorRedactsConfiguredEnvValues(t *testing.T) {
	// Mirrors the write-only agent.env contract: config API exposes envKeys only;
	// probe diagnostics must not re-surface credential values via probeError.
	const secret = "sk-super-secret-token-value-xyz"
	svc := NewService(Options{
		Runner: runnerFunc(func(context.Context, []string, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("authentication failed: invalid token %s for user", secret)
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.List(context.Background(), ListOptions{
		Vendor: config.AgentVendorCodex,
		Env:    map[string]string{"OPENAI_API_KEY": secret, "EMPTY": ""},
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got.Sources.Probe != ProbeError {
		t.Fatalf("probe = %q, want error", got.Sources.Probe)
	}
	if strings.Contains(got.Sources.ProbeError, secret) {
		t.Fatalf("probeError leaked agent.env value: %q", got.Sources.ProbeError)
	}
	if !strings.Contains(got.Sources.ProbeError, "[REDACTED]") {
		t.Fatalf("probeError = %q, want [REDACTED] placeholder", got.Sources.ProbeError)
	}
	if !strings.Contains(got.Sources.ProbeError, "authentication failed") {
		t.Fatalf("probeError = %q, want non-secret diagnostic preserved", got.Sources.ProbeError)
	}

	// Cached response must also stay redacted (listUncached redacts before cachePut).
	cached, err := svc.List(context.Background(), ListOptions{
		Vendor: config.AgentVendorCodex,
		Env:    map[string]string{"OPENAI_API_KEY": secret},
	})
	if err != nil {
		t.Fatalf("cached List() error = %v", err)
	}
	if strings.Contains(cached.Sources.ProbeError, secret) {
		t.Fatalf("cached probeError leaked agent.env value: %q", cached.Sources.ProbeError)
	}
}

func TestShortProbeErrorRedactsLongestSecretsFirst(t *testing.T) {
	err := errors.New("token=abc-longer-secret and short=abc")
	got := shortProbeError(err, map[string]string{
		"LONG":  "abc-longer-secret",
		"SHORT": "abc",
	})
	if strings.Contains(got, "abc-longer-secret") || strings.Contains(got, "token=abc") {
		t.Fatalf("shortProbeError = %q, secrets still present", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("shortProbeError = %q, want redaction", got)
	}
}

func TestDefaultRunnerStderrRedactedInListProbeError(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess stderr redaction test")
	}
	const secret = "probe-stderr-secret-must-not-leak-42"
	// Resolve a real bash so defaultRunner runs; override vendor command via params.
	// Probe always passes vendor-specific args; use a wrapper script that ignores
	// args and prints the secret on stderr then exits nonzero.
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "fake-cli")
	script := "#!/bin/sh\necho \"auth error: bad key " + secret + "\" >&2\nexit 1\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	svc := NewService(Options{
		// Production defaultRunner path (no fake Runner).
		LookPath: func(string) (string, error) { return wrapper, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.List(context.Background(), ListOptions{
		Vendor: config.AgentVendorOpenCode,
		Params: map[string]any{"command": wrapper},
		Env:    map[string]string{"OPENCODE_API_KEY": secret},
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got.Sources.Probe != ProbeError {
		t.Fatalf("probe = %q err=%q, want error", got.Sources.Probe, got.Sources.ProbeError)
	}
	if strings.Contains(got.Sources.ProbeError, secret) {
		t.Fatalf("probeError leaked stderr secret: %q", got.Sources.ProbeError)
	}
	if !strings.Contains(got.Sources.ProbeError, "[REDACTED]") {
		t.Fatalf("probeError = %q, want [REDACTED]", got.Sources.ProbeError)
	}
}

func TestListProbeOKMergesAndCaches(t *testing.T) {
	var now atomic.Value
	now.Store(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
	var calls atomic.Int32
	svc := NewService(Options{
		TTL: 60 * time.Second,
		Runner: runnerFunc(func(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
			calls.Add(1)
			if name != "/usr/bin/opencode" {
				t.Fatalf("binary = %q, want resolved path", name)
			}
			if !reflect.DeepEqual(args, []string{"models"}) {
				t.Fatalf("args = %#v", args)
			}
			return readTestdata(t, "opencode_models.txt"), nil
		}),
		LookPath: func(s string) (string, error) {
			if s == "opencode" {
				return "/usr/bin/opencode", nil
			}
			return "", errors.New("not found")
		},
		Now: func() time.Time { return now.Load().(time.Time) },
	})

	first, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorOpenCode})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if first.Sources.Probe != ProbeOK {
		t.Fatalf("probe = %q, want ok", first.Sources.Probe)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after first list", calls.Load())
	}
	// Static entries first; probe-only after.
	if first.Models[0].Source != SourceMerged && first.Models[0].Source != SourceStatic {
		t.Fatalf("first model source = %q", first.Models[0].Source)
	}
	// Bare probe id overlapping static must keep static label.
	for _, m := range first.Models {
		if m.ID == "anthropic/claude-sonnet-4-5" {
			if m.Label != "Anthropic Claude Sonnet 4.5" {
				t.Fatalf("overlapping bare probe overwrote static label: %#v", m)
			}
			if m.Source != SourceMerged {
				t.Fatalf("source = %q, want merged", m.Source)
			}
		}
	}
	foundProbeOnly := false
	for _, m := range first.Models {
		if m.ID == "openai/gpt-4.1" && m.Source == SourceProbe {
			foundProbeOnly = true
		}
	}
	if !foundProbeOnly {
		t.Fatalf("expected probe-only openai/gpt-4.1 in %#v", first.Models)
	}

	// Cache hit — no second probe.
	second, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorOpenCode})
	if err != nil {
		t.Fatalf("List() cache error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after cache hit, want 1", calls.Load())
	}
	if second.ProbedAt != first.ProbedAt {
		t.Fatalf("cached probedAt changed: %q vs %q", second.ProbedAt, first.ProbedAt)
	}

	// refresh bypasses cache.
	_, err = svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorOpenCode, Refresh: true})
	if err != nil {
		t.Fatalf("List(refresh) error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d after refresh, want 2", calls.Load())
	}

	// TTL expiry.
	now.Store(now.Load().(time.Time).Add(61 * time.Second))
	_, err = svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorOpenCode})
	if err != nil {
		t.Fatalf("List() after TTL error = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d after TTL, want 3", calls.Load())
	}
}

func TestListCoalescesInflightSameKey(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	svc := NewService(Options{
		Runner: runnerFunc(func(context.Context, []string, string, ...string) ([]byte, error) {
			n := calls.Add(1)
			if n == 1 {
				close(started)
			}
			<-release
			return []byte("probe-only-model\n"), nil
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	results := make(chan Result, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorOpenCode})
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	// Let waiters pile up on the in-flight call.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		t.Fatalf("List() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1 (coalesced)", calls.Load())
	}
	count := 0
	for got := range results {
		count++
		if got.Sources.Probe != ProbeOK {
			t.Fatalf("probe = %q, want ok", got.Sources.Probe)
		}
	}
	if count != n {
		t.Fatalf("results = %d, want %d", count, n)
	}
}

func TestListCanceledCallerDoesNotPoisonCache(t *testing.T) {
	// Request cancel (popup close) must not abort the shared probe or cache a
	// "probe canceled" failure that later Lists would serve for the full TTL.
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	svc := NewService(Options{
		Runner: runnerFunc(func(ctx context.Context, _ []string, _ string, _ ...string) ([]byte, error) {
			calls.Add(1)
			close(started)
			select {
			case <-release:
				return []byte("good-model\n"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	errCh := make(chan error, 1)
	go func() {
		got, err := svc.List(ctx, ListOptions{Vendor: config.AgentVendorOpenCode})
		errCh <- err
		done <- got
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	cancel()
	// Give the leader a chance to observe cancel if it still used the request ctx.
	time.Sleep(30 * time.Millisecond)
	close(release)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("List() after cancel error = %v (probe should outlive request cancel)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("List() did not return")
	}
	leader := <-done
	if leader.Sources.Probe != ProbeOK {
		t.Fatalf("leader probe = %q err=%q, want ok", leader.Sources.Probe, leader.Sources.ProbeError)
	}

	// Cache must hold the successful probe, not a cancel failure.
	second, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorOpenCode})
	if err != nil {
		t.Fatalf("second List() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1 (cached success)", calls.Load())
	}
	if second.Sources.Probe != ProbeOK {
		t.Fatalf("cached probe = %q err=%q, want ok", second.Sources.Probe, second.Sources.ProbeError)
	}
	found := false
	for _, m := range second.Models {
		if m.ID == "good-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected good-model in cached result %#v", second.Models)
	}
}

func TestListCanceledLeaderDoesNotPoisonLiveWaiters(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	svc := NewService(Options{
		Runner: runnerFunc(func(ctx context.Context, _ []string, _ string, _ ...string) ([]byte, error) {
			calls.Add(1)
			close(started)
			select {
			case <-release:
				return []byte("shared-model\n"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = svc.List(leaderCtx, ListOptions{Vendor: config.AgentVendorOpenCode})
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}

	// Waiter joins while probe is in flight; leader cancel must not poison it.
	waiterErr := make(chan error, 1)
	waiterResult := make(chan Result, 1)
	go func() {
		got, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorOpenCode})
		waiterErr <- err
		waiterResult <- got
	}()
	time.Sleep(30 * time.Millisecond)
	leaderCancel()
	close(release)

	select {
	case <-leaderDone:
	case <-time.After(3 * time.Second):
		t.Fatal("leader List() did not return")
	}
	select {
	case err := <-waiterErr:
		if err != nil {
			t.Fatalf("waiter List() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter List() did not return")
	}
	got := <-waiterResult
	if got.Sources.Probe != ProbeOK {
		t.Fatalf("waiter probe = %q err=%q, want ok", got.Sources.Probe, got.Sources.ProbeError)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1", calls.Load())
	}
}

func TestListUnknownVendor(t *testing.T) {
	svc := NewService(Options{})
	_, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendor("nope")})
	if !errors.Is(err, ErrUnknownVendor) {
		t.Fatalf("err = %v, want ErrUnknownVendor", err)
	}
}

func TestListUsesParamsCommand(t *testing.T) {
	var saw string
	svc := NewService(Options{
		Runner: runnerFunc(func(_ context.Context, _ []string, name string, _ ...string) ([]byte, error) {
			saw = name
			return []byte("custom-model\n"), nil
		}),
		LookPath: func(s string) (string, error) { return s, nil },
	})
	_, err := svc.List(context.Background(), ListOptions{
		Vendor: config.AgentVendorCodex,
		Params: map[string]any{"command": "/opt/custom-codex"},
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if saw != "/opt/custom-codex" {
		t.Fatalf("command = %q, want /opt/custom-codex", saw)
	}
}

func TestListRelativeCommandOverrideSkipsProbe(t *testing.T) {
	// Spawn resolves ./tools/... against the run worktree (cmd.Dir). Catalog
	// probes have no worktree — must not LookPath/run against looperd CWD.
	var runnerCalls atomic.Int32
	var lookPathCalls atomic.Int32
	svc := NewService(Options{
		Runner: runnerFunc(func(context.Context, []string, string, ...string) ([]byte, error) {
			runnerCalls.Add(1)
			return []byte("should-not-run\n"), nil
		}),
		LookPath: func(s string) (string, error) {
			lookPathCalls.Add(1)
			// If this path were used, LookPath would wrongly pin to daemon CWD.
			return "/wrong-daemon-cwd/" + s, nil
		},
		Now: func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	for _, command := range []string{"./tools/codex-wrapper", "tools/codex-wrapper", "../bin/codex"} {
		got, err := svc.List(context.Background(), ListOptions{
			Vendor:  config.AgentVendorCodex,
			Params:  map[string]any{"command": command},
			Refresh: true,
		})
		if err != nil {
			t.Fatalf("List(%q) error = %v", command, err)
		}
		if got.Sources.Probe != ProbeError {
			t.Fatalf("List(%q) probe = %q, want error", command, got.Sources.Probe)
		}
		if got.Sources.ProbeError != relativeCommandProbeError {
			t.Fatalf("List(%q) probeError = %q, want %q", command, got.Sources.ProbeError, relativeCommandProbeError)
		}
		if !got.Sources.Static || len(got.Models) == 0 {
			t.Fatalf("List(%q) expected static models, got %#v", command, got)
		}
		if got.ProbedAt != "2026-08-01T12:00:00Z" {
			t.Fatalf("List(%q) probedAt = %q", command, got.ProbedAt)
		}
		for _, m := range got.Models {
			if m.Source != SourceStatic {
				t.Fatalf("List(%q) model source = %q, want static only", command, m.Source)
			}
		}
	}
	if runnerCalls.Load() != 0 {
		t.Fatalf("runner calls = %d, want 0", runnerCalls.Load())
	}
	if lookPathCalls.Load() != 0 {
		t.Fatalf("LookPath calls = %d, want 0 (must not resolve relative against daemon CWD)", lookPathCalls.Load())
	}
}

func TestListBareAndAbsoluteCommandsStillProbe(t *testing.T) {
	var saw []string
	svc := NewService(Options{
		Runner: runnerFunc(func(_ context.Context, _ []string, name string, _ ...string) ([]byte, error) {
			saw = append(saw, name)
			return []byte(`{"models":[{"id":"gpt-5.4","name":"GPT-5.4","visibility":"list"}]}`), nil
		}),
		LookPath: func(s string) (string, error) {
			if s == "codex" {
				return "/usr/bin/codex", nil
			}
			return s, nil
		},
	})

	if _, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorCodex}); err != nil {
		t.Fatalf("bare List() error = %v", err)
	}
	if _, err := svc.List(context.Background(), ListOptions{
		Vendor: config.AgentVendorCodex,
		Params: map[string]any{"command": "/opt/custom-codex"},
	}); err != nil {
		t.Fatalf("absolute List() error = %v", err)
	}
	if !reflect.DeepEqual(saw, []string{"/usr/bin/codex", "/opt/custom-codex"}) {
		t.Fatalf("probed binaries = %#v, want bare resolved + absolute", saw)
	}
}

func TestIsRelativePathCommand(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"codex", false},
		{"my.codex-wrapper", false},
		{"/opt/custom-codex", false},
		{"./tools/codex-wrapper", true},
		{"tools/codex-wrapper", true},
		{"../bin/codex", true},
	}
	for _, tc := range cases {
		if got := isRelativePathCommand(tc.in); got != tc.want {
			t.Fatalf("isRelativePathCommand(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestDefaultRunnerRejectsRelativeCommand(t *testing.T) {
	_, err := defaultRunner{}.Run(context.Background(), nil, "./tools/codex-wrapper", "debug", "models")
	if err == nil {
		t.Fatal("expected error for relative command")
	}
	if !strings.Contains(err.Error(), relativeCommandProbeError) {
		t.Fatalf("error = %q, want %q", err, relativeCommandProbeError)
	}
}

func TestListProbeOutputOverflowIsError(t *testing.T) {
	// Fake runner simulates defaultRunner overflow failure.
	svc := NewService(Options{
		Runner: runnerFunc(func(context.Context, []string, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("probe output exceeded %d bytes", maxProbeOutputBytes)
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.List(context.Background(), ListOptions{Vendor: config.AgentVendorOpenCode})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got.Sources.Probe != ProbeError {
		t.Fatalf("probe = %q, want error", got.Sources.Probe)
	}
	if !strings.Contains(got.Sources.ProbeError, "exceeded") {
		t.Fatalf("probeError = %q", got.Sources.ProbeError)
	}
	if len(got.Models) == 0 {
		t.Fatal("expected static models on overflow")
	}
}

func TestParseCodexWithoutVisibilityIncludesAll(t *testing.T) {
	raw := []byte(`{"models":[{"id":"a","name":"A"},{"id":"b","name":"B"}]}`)
	got, err := parseCodexModels(raw)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(modelIDs(got), []string{"a", "b"}) {
		t.Fatalf("ids = %#v", modelIDs(got))
	}
}

func TestDefaultRunnerTimeoutKillsProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess group-kill test")
	}
	// Leader spawns a same-group descendant that ignores SIGTERM and holds the
	// process group alive; timeout must still return via group Kill+drain.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := defaultRunner{}.Run(ctx, nil, "bash", "-c", `
trap '' TERM
sleep 60 &
sleep 60
wait
`)
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Fatalf("Run hung for %v; expected process-group kill to bound return", elapsed)
	}
	if err == nil {
		t.Fatal("expected timeout/cancel error")
	}
}

func TestDefaultRunnerBoundsHugeOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess output-bound test")
	}
	// Emit more than maxProbeOutputBytes on stdout; runner must fail (not OOM)
	// and return promptly while still draining the writer.
	script := fmt.Sprintf(`python3 -c 'import sys; sys.stdout.write("x"*%d)'`, maxProbeOutputBytes+64*1024)
	if _, lookErr := exec.LookPath("python3"); lookErr != nil {
		// Fallback without python.
		script = fmt.Sprintf(`dd if=/dev/zero bs=1024 count=%d 2>/dev/null | tr '\0' 'x'`, (maxProbeOutputBytes/1024)+64)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := defaultRunner{}.Run(ctx, nil, "bash", "-c", script)
	if err == nil {
		t.Fatal("expected overflow error")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err = %v, want exceeded", err)
	}
}

func TestBoundedBufferDiscardsAfterCap(t *testing.T) {
	b := newBoundedBuffer(8)
	n, err := b.Write([]byte("hello world!!!"))
	if err != nil {
		t.Fatalf("Write error = %v", err)
	}
	if n != 14 {
		t.Fatalf("Write n = %d, want 14 (full accept)", n)
	}
	if !b.Truncated() {
		t.Fatal("expected truncated")
	}
	if got := b.String(); got != "hello wo" {
		t.Fatalf("retained = %q, want %q", got, "hello wo")
	}
	// Further writes still succeed (drain) without growing.
	n, err = b.Write([]byte("more"))
	if err != nil || n != 4 {
		t.Fatalf("second Write n=%d err=%v", n, err)
	}
	if len(b.Bytes()) != 8 {
		t.Fatalf("len after discard = %d", len(b.Bytes()))
	}
}

func TestListShutdownCancelsSharedProbeNotRequestCancel(t *testing.T) {
	// Daemon Shutdown must cancel shared probes; request cancel must not.
	var calls atomic.Int32
	started := make(chan struct{})
	sawCancel := make(chan struct{})
	svc := NewService(Options{
		Runner: runnerFunc(func(ctx context.Context, _ []string, _ string, _ ...string) ([]byte, error) {
			calls.Add(1)
			close(started)
			<-ctx.Done()
			close(sawCancel)
			return nil, ctx.Err()
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})

	reqCtx, reqCancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	errCh := make(chan error, 1)
	go func() {
		got, err := svc.List(reqCtx, ListOptions{Vendor: config.AgentVendorOpenCode})
		errCh <- err
		done <- got
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	// Request cancel (popup) must not abort the shared probe.
	reqCancel()
	select {
	case <-sawCancel:
		t.Fatal("probe observed cancel after request cancel; want daemon-owned lifetime")
	case <-time.After(80 * time.Millisecond):
	}

	// Daemon shutdown cancels probeCtx → runner sees cancel.
	svc.Shutdown()
	select {
	case <-sawCancel:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not observe Shutdown cancel")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("List() error = %v, want soft probe error result", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("List() did not return after Shutdown")
	}
	got := <-done
	if got.Sources.Probe != ProbeError {
		t.Fatalf("probe = %q, want error after shutdown cancel", got.Sources.Probe)
	}
	if got.Sources.ProbeError != "probe canceled" && !strings.Contains(got.Sources.ProbeError, "canceled") {
		t.Fatalf("probeError = %q, want cancel", got.Sources.ProbeError)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestListPassesAgentEnvToRunner(t *testing.T) {
	var sawEnv []string
	svc := NewService(Options{
		Runner: runnerFunc(func(_ context.Context, env []string, _ string, _ ...string) ([]byte, error) {
			sawEnv = append([]string(nil), env...)
			return []byte("from-env-model\n"), nil
		}),
		LookPath: func(s string) (string, error) { return "/usr/bin/" + s, nil },
		Now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.List(context.Background(), ListOptions{
		Vendor: config.AgentVendorOpenCode,
		Env:    map[string]string{"OPENCODE_API_KEY": "secret-from-agent-env", "CUSTOM_PROBE": "1"},
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got.Sources.Probe != ProbeOK {
		t.Fatalf("probe = %q err=%q", got.Sources.Probe, got.Sources.ProbeError)
	}
	envMap := envSliceToMap(sawEnv)
	if envMap["OPENCODE_API_KEY"] != "secret-from-agent-env" {
		t.Fatalf("OPENCODE_API_KEY = %q, want agent.env value", envMap["OPENCODE_API_KEY"])
	}
	if envMap["CUSTOM_PROBE"] != "1" {
		t.Fatalf("CUSTOM_PROBE = %q", envMap["CUSTOM_PROBE"])
	}
	// Ambient secrets must not be inherited wholesale: require allowlist shape
	// (PATH may be present; arbitrary LOOPER_TEST_SECRET must not unless configured).
	if _, ok := envMap["LOOPER_TEST_SECRET_SHOULD_NOT_LEAK"]; ok {
		t.Fatal("ambient secret leaked into probe env")
	}
}

func TestDefaultRunnerUsesSanitizedEnvNotAmbient(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess env sanitization test")
	}
	t.Setenv("LOOPER_PROBE_AMBIENT_SECRET", "daemon-credential-must-not-leak")
	t.Setenv("OPENCODE_API_KEY", "ambient-key-must-not-leak")

	// Child prints whether ambient secret is visible and whether configured key is.
	script := `python3 -c 'import os; print("ambient="+("yes" if os.environ.get("LOOPER_PROBE_AMBIENT_SECRET") else "no")); print("cfg="+os.environ.get("OPENCODE_API_KEY",""))'`
	if _, err := exec.LookPath("python3"); err != nil {
		script = `sh -c 'if [ -n "$LOOPER_PROBE_AMBIENT_SECRET" ]; then echo ambient=yes; else echo ambient=no; fi; echo cfg=$OPENCODE_API_KEY'`
	}
	env := buildProbeEnv(map[string]string{"OPENCODE_API_KEY": "from-agent-env"})
	out, err := defaultRunner{}.Run(context.Background(), env, "bash", "-c", script)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "ambient=no") {
		t.Fatalf("ambient secret leaked into probe; output=%q", text)
	}
	if !strings.Contains(text, "cfg=from-agent-env") {
		t.Fatalf("configured agent.env not present; output=%q", text)
	}
	// Double-check ambient OPENCODE_API_KEY was replaced by agent.env, not ambient.
	if strings.Contains(text, "ambient-key-must-not-leak") {
		t.Fatalf("ambient OPENCODE_API_KEY leaked; output=%q", text)
	}
}

func TestDefaultRunnerTracksHandleWithLiveTracker(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess tracker test")
	}
	tracker := &recordingTracker{}
	runner := defaultRunner{tracker: tracker}
	_, err := runner.Run(context.Background(), buildProbeEnv(nil), "bash", "-c", "echo ok")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if tracker.begins.Load() != 1 {
		t.Fatalf("BeginTrack calls = %d, want 1", tracker.begins.Load())
	}
	if tracker.tracks.Load() != 1 {
		t.Fatalf("Track calls = %d, want 1", tracker.tracks.Load())
	}
	if tracker.releases.Load() != 1 {
		t.Fatalf("release calls = %d, want 1", tracker.releases.Load())
	}
}

func envSliceToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	return out
}

type recordingTracker struct {
	begins   atomic.Int32
	tracks   atomic.Int32
	releases atomic.Int32
}

func (t *recordingTracker) BeginTrack() (end func(), err error) {
	t.begins.Add(1)
	return func() {}, nil
}

func (t *recordingTracker) Track(handle *processcontainment.Handle) (release func()) {
	t.tracks.Add(1)
	return func() { t.releases.Add(1) }
}

func (t *recordingTracker) ReportDrainFailure(error) {}

type runnerFunc func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	return f(ctx, env, name, args...)
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return raw
}

func modelIDs(models []Model) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = m.ID
	}
	return out
}

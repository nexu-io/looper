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
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
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
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
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

func TestListProbeOKMergesAndCaches(t *testing.T) {
	var now atomic.Value
	now.Store(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
	var calls atomic.Int32
	svc := NewService(Options{
		TTL: 60 * time.Second,
		Runner: runnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
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
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
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
		Runner: runnerFunc(func(_ context.Context, name string, _ ...string) ([]byte, error) {
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

func TestListProbeOutputOverflowIsError(t *testing.T) {
	// Fake runner simulates defaultRunner overflow failure.
	svc := NewService(Options{
		Runner: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
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
	_, err := defaultRunner{}.Run(ctx, "bash", "-c", `
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
	_, err := defaultRunner{}.Run(ctx, "bash", "-c", script)
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

type runnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
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

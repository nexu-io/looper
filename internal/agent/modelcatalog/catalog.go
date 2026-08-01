// Package modelcatalog serves advisory agent model suggestions (static + CLI probe).
// Catalog membership is never a config or runtime gate (ADR-0016).
package modelcatalog

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/processcontainment"
)

const (
	defaultProbeTimeout = 5 * time.Second
	defaultCacheTTL     = 60 * time.Second

	SourceStatic = "static"
	SourceProbe  = "probe"
	SourceMerged = "merged"

	ProbeOK          = "ok"
	ProbeSkipped     = "skipped"
	ProbeError       = "error"
	ProbeUnsupported = "unsupported"
)

// Model is one suggested model id for a vendor CLI.
type Model struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Source string `json:"source"`
}

// Sources describes how the model list was assembled.
type Sources struct {
	Static     bool   `json:"static"`
	Probe      string `json:"probe"`
	ProbeError string `json:"probeError,omitempty"`
}

// Result is the GET /api/v1/agent/models data payload.
type Result struct {
	Vendor   string  `json:"vendor"`
	Models   []Model `json:"models"`
	Sources  Sources `json:"sources"`
	ProbedAt string  `json:"probedAt,omitempty"`
}

// Options configures a Service. Zero values use defaults.
type Options struct {
	TTL     time.Duration
	Timeout time.Duration
	Now     func() time.Time
	Runner  Runner
	// BaseContext bounds shared probe lifetime. Request cancel (popup close)
	// must not cancel shared probes; BaseContext cancel (daemon BeginShutdown)
	// must. Defaults to a service-owned cancelable background context.
	BaseContext context.Context
	// Tracker registers probe containment handles with the Supervisor for
	// shutdown drain / retain-storage (ADR-0015 / #577). Optional.
	Tracker  processcontainment.LiveTracker
	LookPath func(string) (string, error)
}

// Service merges static catalog entries with on-demand CLI probes.
type Service struct {
	ttl      time.Duration
	timeout  time.Duration
	now      func() time.Time
	runner   Runner
	lookPath func(string) (string, error)
	static   map[config.AgentVendor][]Model

	// probeCtx is the daemon-owned parent for shared probes: independent of
	// per-request cancel, canceled by Shutdown (BeginShutdown path).
	probeCtx    context.Context
	probeCancel context.CancelFunc

	mu       sync.Mutex
	cache    map[string]cacheEntry
	inflight map[string]*inflightCall
}

type cacheEntry struct {
	result    Result
	expiresAt time.Time
}

// inflightCall coalesces concurrent cold Lists for the same cache key so only
// one probe runs; waiters share the result.
type inflightCall struct {
	done   chan struct{}
	result Result
	err    error
}

// NewService constructs a catalog service with embedded static data.
func NewService(opts Options) *Service {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	parent := opts.BaseContext
	if parent == nil {
		parent = context.Background()
	}
	probeCtx, probeCancel := context.WithCancel(parent)

	runner := opts.Runner
	if runner == nil {
		runner = defaultRunner{tracker: opts.Tracker}
	}
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = execLookPath
	}
	return &Service{
		ttl:         ttl,
		timeout:     timeout,
		now:         now,
		runner:      runner,
		lookPath:    lookPath,
		static:      loadStaticCatalog(),
		probeCtx:    probeCtx,
		probeCancel: probeCancel,
		cache:       make(map[string]cacheEntry),
		inflight:    make(map[string]*inflightCall),
	}
}

// Shutdown cancels the daemon-owned shared probe parent so in-flight vendor
// CLIs observe cancel and enter process-group Kill. Idempotent. Call from
// BeginShutdown before non-agent drain waits so probes drain promptly.
func (s *Service) Shutdown() {
	if s == nil || s.probeCancel == nil {
		return
	}
	s.probeCancel()
}

// ListOptions controls a single catalog request.
type ListOptions struct {
	Vendor  config.AgentVendor
	Params  map[string]any    // spawn-filtered agent.params (command override)
	Env     map[string]string // configured agent.env (auth/config for vendor CLIs)
	Refresh bool
}

// List returns merged model suggestions for vendor. Probe failures never error
// when vendor is valid — callers always get static baseline + probe status.
// Concurrent cold Lists for the same vendor+binary key share one in-flight probe.
func (s *Service) List(ctx context.Context, opts ListOptions) (Result, error) {
	if !isKnownVendor(opts.Vendor) {
		return Result{}, ErrUnknownVendor
	}

	command := agent.ResolveCommand(opts.Vendor, opts.Params)
	// ADR-0016: never discover via ambiguous bare `agent` (Cursor default
	// ResolveCommand name; also excluded from takeover autodetection). Skip
	// LookPath/probe unless the command path establishes Cursor identity.
	if opts.Vendor == config.AgentVendorCursorCLI && isAmbiguousBareAgentCommand(command) {
		return s.listAmbiguousCursorAgent(command), nil
	}
	// Relative path overrides (e.g. ./tools/codex-wrapper) resolve against each
	// run's worktree at spawn time (executor sets cmd.Dir). Catalog probes have
	// no worktree context; do not LookPath/run them from looperd's CWD.
	resolved := command
	if !isRelativePathCommand(command) {
		resolved = resolveBinaryPath(command, s.lookPath)
	}
	cacheKey := string(opts.Vendor) + "\x00" + resolved

	if !opts.Refresh {
		if hit, ok := s.cacheGet(cacheKey); ok {
			return hit, nil
		}
	}

	// Coalesce concurrent work for this key (singleflight-style).
	s.mu.Lock()
	if !opts.Refresh {
		if entry, ok := s.cache[cacheKey]; ok && !s.now().After(entry.expiresAt) {
			s.mu.Unlock()
			return cloneResult(entry.result), nil
		}
	}
	if call, ok := s.inflight[cacheKey]; ok {
		s.mu.Unlock()
		return s.waitInflight(ctx, call)
	}
	call := &inflightCall{done: make(chan struct{})}
	s.inflight[cacheKey] = call
	s.mu.Unlock()

	// Shared probe work must outlive any single request cancel (popup close /
	// AbortController) but must still observe daemon shutdown via probeCtx.
	// Using WithoutCancel(request) would also ignore BeginShutdown/HTTP stop.
	probeCtx := s.probeCtx
	if probeCtx == nil {
		probeCtx = context.Background()
	}
	result := s.listUncached(probeCtx, opts.Vendor, resolved, opts.Env)

	// Defensive: never populate the shared cache with a cancel-sourced probe
	// error (e.g. daemon shutdown cancel during probe) so later Lists retry.
	if !isCancelPoisonedProbe(result) {
		s.cachePut(cacheKey, result)
	}

	s.mu.Lock()
	call.result = result
	// call.err stays nil: valid vendors never return a hard error from listUncached.
	delete(s.inflight, cacheKey)
	close(call.done)
	s.mu.Unlock()

	return cloneResult(result), nil
}

// isCancelPoisonedProbe reports whether the result is a probe failure caused by
// context cancellation. Those must not be cached or they starve later callers.
func isCancelPoisonedProbe(result Result) bool {
	if result.Sources.Probe != ProbeError {
		return false
	}
	msg := result.Sources.ProbeError
	return msg == "probe canceled" || msg == context.Canceled.Error()
}

func (s *Service) waitInflight(ctx context.Context, call *inflightCall) (Result, error) {
	select {
	case <-call.done:
		if call.err != nil {
			return Result{}, call.err
		}
		return cloneResult(call.result), nil
	case <-ctx.Done():
		// Prefer a completed shared result if it races with cancellation.
		select {
		case <-call.done:
			if call.err != nil {
				return Result{}, call.err
			}
			return cloneResult(call.result), nil
		default:
			return Result{}, ctx.Err()
		}
	}
}

func (s *Service) listUncached(ctx context.Context, vendor config.AgentVendor, resolvedBinary string, agentEnv map[string]string) Result {
	staticModels := append([]Model(nil), s.static[vendor]...)
	for i := range staticModels {
		staticModels[i].Source = SourceStatic
	}

	result := Result{
		Vendor: string(vendor),
		Models: staticModels,
		Sources: Sources{
			Static: true,
			Probe:  ProbeSkipped,
		},
	}

	if !supportsProbe(vendor) {
		result.Sources.Probe = ProbeUnsupported
		// No probe attempt — leave probedAt empty.
		return result
	}

	probedAt := s.now().UTC()
	result.ProbedAt = probedAt.Format(time.RFC3339)

	// Relative command overrides are not spawn-equivalent here: agent launch
	// sets cmd.Dir to the run worktree; this endpoint has no such directory.
	// Reject rather than resolving/running from looperd's current directory.
	if isRelativePathCommand(resolvedBinary) {
		result.Sources.Probe = ProbeError
		result.Sources.ProbeError = relativeCommandProbeError
		return result
	}

	env := buildProbeEnv(agentEnv)
	probeModels, err := s.probe(ctx, vendor, resolvedBinary, env)
	if err != nil {
		result.Sources.Probe = ProbeError
		// Redact agent.env values before caching / returning to API clients.
		result.Sources.ProbeError = shortProbeError(err, agentEnv)
		return result
	}

	result.Sources.Probe = ProbeOK
	result.Models = mergeModels(staticModels, probeModels)
	return result
}

func (s *Service) cacheGet(key string) (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok || s.now().After(entry.expiresAt) {
		return Result{}, false
	}
	return cloneResult(entry.result), true
}

func (s *Service) cachePut(key string, result Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = cacheEntry{
		result:    cloneResult(result),
		expiresAt: s.now().Add(s.ttl),
	}
}

func cloneResult(in Result) Result {
	out := in
	if in.Models != nil {
		out.Models = append([]Model(nil), in.Models...)
	}
	return out
}

func isKnownVendor(vendor config.AgentVendor) bool {
	switch vendor {
	case config.AgentVendorClaudeCode, config.AgentVendorCodex, config.AgentVendorOpenCode,
		config.AgentVendorCursorCLI, config.AgentVendorGrokBuild:
		return true
	default:
		return false
	}
}

func supportsProbe(vendor config.AgentVendor) bool {
	switch vendor {
	case config.AgentVendorCodex, config.AgentVendorOpenCode, config.AgentVendorCursorCLI, config.AgentVendorGrokBuild:
		return true
	default:
		return false
	}
}

func resolveBinaryPath(command string, lookPath func(string) (string, error)) string {
	if command == "" {
		return command
	}
	// Absolute or explicit path overrides stay as-is.
	if lookPath == nil {
		return command
	}
	if path, err := lookPath(command); err == nil && path != "" {
		return path
	}
	return command
}

// relativeCommandProbeError is returned when agent.params.command is a
// worktree-relative path. Probes cannot supply spawn's WorkingDirectory.
const relativeCommandProbeError = "relative command override cannot be probed outside a run worktree"

// ambiguousBareAgentProbeError is returned when cursor-cli would probe via the
// default/unqualified bare name "agent" (ADR-0016).
const ambiguousBareAgentProbeError = "cursor-cli command \"agent\" is ambiguous; set agent.params.command to an explicit cursor-agent path to enable probing"

// listAmbiguousCursorAgent returns static suggestions only without LookPath or
// running PATH's first "agent" binary.
func (s *Service) listAmbiguousCursorAgent(command string) Result {
	staticModels := append([]Model(nil), s.static[config.AgentVendorCursorCLI]...)
	for i := range staticModels {
		staticModels[i].Source = SourceStatic
	}
	result := Result{
		Vendor: string(config.AgentVendorCursorCLI),
		Models: staticModels,
		Sources: Sources{
			Static:     true,
			Probe:      ProbeError,
			ProbeError: ambiguousBareAgentProbeError,
		},
		ProbedAt: s.now().UTC().Format(time.RFC3339),
	}
	// Cache the skip so repeated combobox opens do not re-evaluate PATH.
	cacheKey := string(config.AgentVendorCursorCLI) + "\x00" + command
	s.cachePut(cacheKey, result)
	return result
}

// isAmbiguousBareAgentCommand reports whether command is the unqualified bare
// name "agent" (or agent.exe). Paths that include "cursor" (e.g. cursor-agent
// or .../Cursor.app/.../agent) establish Cursor identity and may be probed.
func isAmbiguousBareAgentCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	base := filepath.Base(command)
	base = strings.TrimSuffix(base, ".exe")
	if !strings.EqualFold(base, "agent") {
		return false
	}
	// Path or basename establishes Cursor identity (cursor-agent, Cursor.app, …).
	if strings.Contains(strings.ToLower(command), "cursor") {
		return false
	}
	return true
}

// isRelativePathCommand reports whether command is a path that agent spawn
// resolves relative to the run worktree (cmd.Dir). Bare PATH names and
// absolute paths do not need a worktree to resolve and may be probed as-is.
func isRelativePathCommand(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || filepath.IsAbs(command) {
		return false
	}
	// PATH lookup names have no path separators.
	if !strings.ContainsRune(command, '/') && (filepath.Separator == '/' || !strings.ContainsRune(command, filepath.Separator)) {
		return false
	}
	return true
}

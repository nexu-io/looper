// Package modelcatalog serves advisory agent model suggestions (static + CLI probe).
// Catalog membership is never a config or runtime gate (ADR-0016).
package modelcatalog

import (
	"context"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
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
	TTL      time.Duration
	Timeout  time.Duration
	Now      func() time.Time
	Runner   Runner
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
	runner := opts.Runner
	if runner == nil {
		runner = defaultRunner{}
	}
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = execLookPath
	}
	return &Service{
		ttl:      ttl,
		timeout:  timeout,
		now:      now,
		runner:   runner,
		lookPath: lookPath,
		static:   loadStaticCatalog(),
		cache:    make(map[string]cacheEntry),
		inflight: make(map[string]*inflightCall),
	}
}

// ListOptions controls a single catalog request.
type ListOptions struct {
	Vendor  config.AgentVendor
	Params  map[string]any // spawn-filtered agent.params (command override)
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
	resolved := resolveBinaryPath(command, s.lookPath)
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
	// AbortController). Tying listUncached to the leader's ctx would mark the
	// probe as canceled, cache that failure for the full TTL, and poison every
	// waiter coalesced behind this call even when their contexts are still live.
	probeCtx := context.WithoutCancel(ctx)
	result := s.listUncached(probeCtx, opts.Vendor, resolved)

	// Defensive: never populate the shared cache with a cancel-sourced probe
	// error (e.g. if a future caller re-introduces request cancel into probeCtx).
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

func (s *Service) listUncached(ctx context.Context, vendor config.AgentVendor, resolvedBinary string) Result {
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

	probeModels, err := s.probe(ctx, vendor, resolvedBinary)
	if err != nil {
		result.Sources.Probe = ProbeError
		result.Sources.ProbeError = shortProbeError(err)
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

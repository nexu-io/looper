// Package hostingidentity owns code hosting credentials inside the daemon.
// A Session captures operator configuration once; refreshing credentials never
// selects another identity or consults legacy CLI authentication.
package hostingidentity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

// Options supplies daemon dependencies. Omitted fields use standard library
// implementations. An injected HTTP client's redirects are still disabled.
type Options struct {
	HTTPClient *http.Client
	Now        func() time.Time
	LookupEnv  func(string) (string, bool)
	ReadFile   func(string) ([]byte, error)
}

// Manager caches credentials by the captured identity definition and target.
// It never writes tokens, keys, or account state to disk.
type Manager struct {
	client    *http.Client
	now       func() time.Time
	lookupEnv func(string) (string, bool)
	readFile  func(string) ([]byte, error)
	mu        sync.Mutex
	entries   map[string]*credentialEntry
}

type credentialEntry struct {
	mu         sync.Mutex
	credential Credential
	refreshAt  time.Time
	refreshing *credentialRefresh
	principal  Credential
}

type credentialRefresh struct {
	done        chan struct{}
	sourceToken string
	credential  Credential
	err         error
}

// Credential is daemon-only. Token is excluded from JSON and formatted output;
// callers must also redact subprocess output before publishing or logging it.
type Credential struct {
	Token     string    `json:"-"`
	Login     string    `json:"login"`
	NumericID int64     `json:"numericId"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

func (credential Credential) String() string {
	return fmt.Sprintf("hosting credential(login=%q, id=%d)", credential.Login, credential.NumericID)
}

func (credential Credential) GoString() string { return credential.String() }

// Error describes a failed identity operation without including response
// bodies, credential material, private-key contents, or transport errors.
type Error struct {
	Identity   string
	Operation  string
	StatusCode int
	Reason     string
	cause      error
}

func (err *Error) Error() string {
	if err.StatusCode != 0 {
		return fmt.Sprintf("hosting identity %q: %s: %s (HTTP %d)", err.Identity, err.Operation, err.Reason, err.StatusCode)
	}
	return fmt.Sprintf("hosting identity %q: %s: %s", err.Identity, err.Operation, err.Reason)
}

func (err *Error) Unwrap() error { return err.cause }

// ErrNoSession means the operation has legacy authentication and no explicit
// hosting identity. Callers must check the binding before using Credentials.
var ErrNoSession = errors.New("no hosting identity is bound to this context")

func NewManager(options Options) *Manager {
	client := &http.Client{Timeout: 30 * time.Second}
	if options.HTTPClient != nil {
		copy := *options.HTTPClient
		client = &copy
		if client.Timeout == 0 {
			client.Timeout = 30 * time.Second
		}
	}
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.LookupEnv == nil {
		options.LookupEnv = os.LookupEnv
	}
	if options.ReadFile == nil {
		options.ReadFile = os.ReadFile
	}
	return &Manager{client: client, now: options.Now, lookupEnv: options.LookupEnv, readFile: options.ReadFile, entries: make(map[string]*credentialEntry)}
}

// Session constructs a detached binding without contacting the hosting server
// or reading credentials. Configuration errors remain distinct from runtime
// authentication failures.
func (manager *Manager) Session(resolved config.ResolvedHostingIdentity) (*Session, error) {
	session := &Session{manager: manager, resolved: resolved}
	if err := session.validate(); err != nil {
		return nil, err
	}
	keyData, _ := json.Marshal(struct {
		Name       string
		Definition config.HostingIdentityConfig
		Target     config.RepositoryIdentity
	}{resolved.Name, resolved.Definition, resolved.Target})
	session.cacheKey = fmt.Sprintf("%x", sha256.Sum256(keyData))
	manager.mu.Lock()
	entry := manager.entries[session.cacheKey]
	if entry == nil {
		entry = &credentialEntry{}
		manager.entries[session.cacheKey] = entry
	}
	manager.mu.Unlock()
	session.entry = entry
	return session, nil
}

// Probe verifies one configured binding. Authentication failures affect this
// identity only; callers may continue probing independent identities.
func (manager *Manager) Probe(ctx context.Context, resolved config.ResolvedHostingIdentity) error {
	session, err := manager.Session(resolved)
	if err != nil {
		return err
	}
	return session.Check(ctx)
}

type managerContextKey struct{}
type sessionContextKey struct{}

type runBinding struct {
	projectID string
	role      string
	session   *Session
}

var defaultManager = NewManager(Options{})

func WithManager(ctx context.Context, manager *Manager) context.Context {
	return context.WithValue(ctx, managerContextKey{}, manager)
}

// FromContext reports only explicit identities, leaving legacy callers intact.
func FromContext(ctx context.Context) (*Session, bool) {
	if ctx == nil {
		return nil, false
	}
	binding, ok := ctx.Value(sessionContextKey{}).(runBinding)
	return binding.session, ok && binding.session != nil
}

// Binding reports captured execution metadata, including legacy selections.
func Binding(ctx context.Context) (projectID, role string, bound bool) {
	if ctx == nil {
		return "", "", false
	}
	value, ok := ctx.Value(sessionContextKey{}).(runBinding)
	return value.projectID, value.role, ok
}

func managerFromContext(ctx context.Context) *Manager {
	if manager, ok := ctx.Value(managerContextKey{}).(*Manager); ok && manager != nil {
		return manager
	}
	return defaultManager
}

// Bind preserves an already-bound run's configuration across reloads. Switching
// project or role requires an explicit Rebind at a new execution boundary.
func Bind(ctx context.Context, cfg config.Config, projectID, role string) (context.Context, error) {
	projectID, role = strings.TrimSpace(projectID), strings.TrimSpace(role)
	if binding, ok := ctx.Value(sessionContextKey{}).(runBinding); ok {
		if binding.projectID == projectID && binding.role == role {
			return ctx, nil
		}
		if binding.session != nil {
			return nil, binding.session.failure("bind", "a different project or role requires a new execution binding")
		}
		return nil, errors.New("hosting identity: a different project or role requires a new execution binding")
	}
	return Rebind(ctx, cfg, projectID, role)
}

// Rebind starts a new execution binding, including clearing an inherited
// explicit identity when the new role uses legacy authentication.
func Rebind(ctx context.Context, cfg config.Config, projectID, role string) (context.Context, error) {
	projectID, role = strings.TrimSpace(projectID), strings.TrimSpace(role)
	resolved, selected, err := config.ResolveHostingIdentity(cfg, projectID, role)
	if err != nil {
		return nil, err
	}
	if !selected {
		return context.WithValue(ctx, sessionContextKey{}, runBinding{projectID: projectID, role: role}), nil
	}
	return BindResolved(ctx, resolved)
}

// BindResolved attaches the daemon's captured selection in a trusted child.
// It accepts no credential values and never re-resolves the live config.
func BindResolved(ctx context.Context, resolved config.ResolvedHostingIdentity) (context.Context, error) {
	session, err := managerFromContext(ctx).Session(resolved)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, sessionContextKey{}, runBinding{projectID: resolved.ProjectID, role: resolved.Role, session: session}), nil
}

// Credentials returns daemon-owned credentials for the captured binding.
func Credentials(ctx context.Context) (Credential, error) {
	session, ok := FromContext(ctx)
	if !ok {
		return Credential{}, ErrNoSession
	}
	return session.Credentials(ctx)
}

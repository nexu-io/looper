package hostingidentity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

type testClock struct{ seconds atomic.Int64 }

func newTestClock() *testClock {
	clock := &testClock{}
	clock.seconds.Store(time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC).Unix())
	return clock
}

func (clock *testClock) now() time.Time { return time.Unix(clock.seconds.Load(), 0).UTC() }
func (clock *testClock) advance(duration time.Duration) {
	clock.seconds.Add(int64(duration / time.Second))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

var testRSAKey struct {
	sync.Once
	key *rsa.PrivateKey
	err error
}

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testRSAKey.Do(func() { testRSAKey.key, testRSAKey.err = rsa.GenerateKey(rand.Reader, 2048) })
	if testRSAKey.err != nil {
		t.Fatal(testRSAKey.err)
	}
	return testRSAKey.key
}

func writeJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("encode fixture: %v", err)
	}
}

type githubFixture struct {
	t           *testing.T
	clock       *testClock
	key         *rsa.PrivateKey
	server      *httptest.Server
	mints       atomic.Int64
	revoked     atomic.Bool
	wrongRepo   atomic.Bool
	wrongApp    atomic.Bool
	expired     atomic.Bool
	botID       atomic.Int64
	mu          sync.Mutex
	jwts        []string
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

func newGitHubFixture(t *testing.T, blocked bool) *githubFixture {
	t.Helper()
	fixture := &githubFixture{t: t, clock: newTestClock(), key: rsaKey(t)}
	fixture.botID.Store(9917)
	if blocked {
		fixture.started, fixture.release = make(chan struct{}), make(chan struct{})
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *githubFixture) handle(response http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/app":
		fixture.verifyJWT(request.Header.Get("Authorization"))
		id := int64(137)
		if fixture.wrongApp.Load() {
			id++
		}
		writeJSON(fixture.t, response, map[string]any{"id": id, "slug": "looper-worker"})
	case "/app/installations/246/access_tokens":
		fixture.verifyJWT(request.Header.Get("Authorization"))
		if request.Method != http.MethodPost {
			fixture.t.Errorf("token method = %s", request.Method)
		}
		var payload struct {
			Repositories []string `json:"repositories"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || len(payload.Repositories) != 1 || payload.Repositories[0] != "repo" {
			fixture.t.Errorf("token must be repository scoped: %+v, %v", payload, err)
		}
		mint := fixture.mints.Add(1)
		if fixture.release != nil {
			fixture.startedOnce.Do(func() { close(fixture.started) })
			select {
			case <-fixture.release:
			case <-request.Context().Done():
				return
			}
		}
		if fixture.revoked.Load() {
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(response, "upstream-secret-body-PRIVATE-KEY installation-token-1")
			return
		}
		expires := fixture.clock.now().Add(time.Hour)
		if fixture.expired.Load() {
			expires = fixture.clock.now().Add(-time.Second)
		}
		writeJSON(fixture.t, response, map[string]any{"token": fmt.Sprintf("installation-token-%d", mint), "expires_at": expires})
	case "/repos/org/repo":
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer installation-token-") {
			fixture.t.Error("repository request did not use installation token")
		}
		name := "org/repo"
		if fixture.wrongRepo.Load() {
			name = "outside/repo"
		}
		writeJSON(fixture.t, response, map[string]any{"full_name": name})
	case "/users/looper-worker[bot]":
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer installation-token-") {
			fixture.t.Error("bot request did not use installation token")
		}
		writeJSON(fixture.t, response, map[string]any{"id": fixture.botID.Load(), "login": "looper-worker[bot]"})
	default:
		fixture.t.Errorf("unexpected GitHub operation: %s", request.URL.Path)
		response.WriteHeader(http.StatusNotFound)
	}
}

func (fixture *githubFixture) verifyJWT(authorization string) {
	fixture.t.Helper()
	if !strings.HasPrefix(authorization, "Bearer ") {
		fixture.t.Error("JWT must use Bearer authorization")
		return
	}
	jwt := strings.TrimPrefix(authorization, "Bearer ")
	fixture.mu.Lock()
	fixture.jwts = append(fixture.jwts, jwt)
	fixture.mu.Unlock()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		fixture.t.Error("malformed JWT")
		return
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(header) != `{"alg":"RS256","typ":"JWT"}` {
		fixture.t.Errorf("JWT header = %s, %v", header, err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		fixture.t.Error(err)
		return
	}
	var claims struct {
		Issuer    string `json:"iss"`
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		fixture.t.Error(err)
		return
	}
	now := fixture.clock.now().Unix()
	if claims.Issuer != "137" || claims.IssuedAt != now-60 || claims.ExpiresAt <= now || claims.ExpiresAt > now+600 {
		fixture.t.Errorf("invalid JWT claims: %+v", claims)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		fixture.t.Error(err)
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&fixture.key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		fixture.t.Errorf("JWT signature: %v", err)
	}
}

func githubResolved() config.ResolvedHostingIdentity {
	return config.ResolvedHostingIdentity{
		Name: "worker-bot", ProjectID: "project", Role: "worker",
		Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityGitHubApp, BaseURL: "https://github.com", AppID: 137, InstallationID: 246, PrivateKeyFile: "/daemon-only/key.pem"},
		Target:     config.RepositoryIdentity{ProviderID: "github", Kind: config.ProviderKindGitHub, BaseURL: "https://github.com", Repo: "org/repo"},
	}
}

func (fixture *githubFixture) options() Options {
	serverURL, _ := url.Parse(fixture.server.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	fixture.t.Cleanup(transport.CloseIdleConnections)
	return Options{
		Now: fixture.clock.now,
		LookupEnv: func(name string) (string, bool) {
			fixture.t.Errorf("GitHub App must not consult environment: %s", name)
			return "", false
		},
		ReadFile: func(path string) ([]byte, error) {
			if path != "/daemon-only/key.pem" {
				fixture.t.Errorf("unexpected private key path %s", path)
			}
			return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(fixture.key)}), nil
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" {
				fixture.t.Errorf("unexpected API origin: %s", request.URL)
			}
			copy := request.Clone(request.Context())
			copy.URL.Scheme, copy.URL.Host = serverURL.Scheme, serverURL.Host
			return transport.RoundTrip(copy)
		})},
	}
}

func sessionFor(t *testing.T, manager *Manager, resolved config.ResolvedHostingIdentity) *Session {
	t.Helper()
	session, err := manager.Session(resolved)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func concurrentCredentials(t *testing.T, session *Session) []Credential {
	t.Helper()
	const count = 20
	credentials := make([]Credential, count)
	var wait sync.WaitGroup
	for i := range credentials {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			var err error
			credentials[i], err = session.Credentials(context.Background())
			if err != nil {
				t.Errorf("credential request: %v", err)
			}
		}(i)
	}
	wait.Wait()
	return credentials
}

func TestGitHubSessionRealJWTConcurrentRefreshAndAttribution(t *testing.T) {
	fixture := newGitHubFixture(t, false)
	session := sessionFor(t, NewManager(fixture.options()), githubResolved())
	for _, credential := range concurrentCredentials(t, session) {
		if credential.Token != "installation-token-1" || credential.Login != "looper-worker[bot]" || credential.NumericID != 9917 || credential.Name != credential.Login || credential.Email != "9917+looper-worker[bot]@users.noreply.github.com" {
			t.Errorf("wrong bot attribution or token: %s", credential)
		}
	}
	if got := fixture.mints.Load(); got != 1 {
		t.Fatalf("concurrent first use minted %d tokens", got)
	}
	fixture.clock.advance(59 * time.Minute)
	for _, credential := range concurrentCredentials(t, session) {
		if credential.Token != "installation-token-2" {
			t.Error("expired token was reused")
		}
	}
	if got := fixture.mints.Load(); got != 2 {
		t.Fatalf("concurrent refresh minted %d tokens total", got)
	}
	session.Invalidate("installation-token-1")
	credential, err := session.Credentials(context.Background())
	if err != nil || credential.Token != "installation-token-2" || fixture.mints.Load() != 2 {
		t.Fatalf("delayed rejection invalidated fresh token: %v", err)
	}
	fixture.mu.Lock()
	jwts := append([]string(nil), fixture.jwts...)
	fixture.mu.Unlock()
	secrets := append(jwts, "installation-token-1", credential.Token, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+credential.Token)))
	for _, secret := range secrets {
		if got := session.Redact("failure: " + secret); strings.Contains(got, secret) {
			t.Error("credential material was not redacted")
		}
	}
	encoded, err := json.Marshal(credential)
	if err != nil || strings.Contains(string(encoded), credential.Token) || strings.Contains(fmt.Sprintf("%+v %#v", credential, credential), credential.Token) {
		t.Fatal("credential is exposed by JSON or formatted output")
	}
}

func TestGitHubRevokedExpiredAndWrongScopeFailWithoutFallback(t *testing.T) {
	for _, scenario := range []string{"revoked", "expired", "different repository", "different app", "changed bot"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newGitHubFixture(t, false)
			session := sessionFor(t, NewManager(fixture.options()), githubResolved())
			if _, err := session.Credentials(context.Background()); err != nil {
				t.Fatal(err)
			}
			fixture.clock.advance(time.Hour)
			switch scenario {
			case "revoked":
				fixture.revoked.Store(true)
			case "expired":
				fixture.expired.Store(true)
			case "different repository":
				fixture.wrongRepo.Store(true)
			case "different app":
				fixture.wrongApp.Store(true)
			case "changed bot":
				fixture.botID.Add(1)
			}
			credential, err := session.Credentials(context.Background())
			if err == nil || credential.Token != "" {
				t.Fatal("credential failure returned a usable token")
			}
			if !strings.Contains(err.Error(), `hosting identity "worker-bot"`) {
				t.Errorf("unlabeled error: %v", err)
			}
			if strings.Contains(err.Error(), "upstream-secret-body") || strings.Contains(err.Error(), "installation-token-") {
				t.Errorf("unsafe error: %v", err)
			}
		})
	}
}

func TestConcurrentCredentialWaiterCanCancel(t *testing.T) {
	fixture := newGitHubFixture(t, true)
	session := sessionFor(t, NewManager(fixture.options()), githubResolved())
	leader := make(chan error, 1)
	go func() { _, err := session.Credentials(context.Background()); leader <- err }()
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("token refresh did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { _, err := session.Credentials(ctx); waiter <- err }()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiter cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Error("credential waiter did not observe cancellation")
	}
	close(fixture.release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	if fixture.mints.Load() != 1 {
		t.Error("cancelled waiter changed refresh ownership")
	}
}

func TestCredentialBindingDoesNotReadKeyUntilAuthentication(t *testing.T) {
	var reads int
	manager := NewManager(Options{ReadFile: func(string) ([]byte, error) { reads++; return nil, errors.New("private-key-secret transport-token") }})
	session := sessionFor(t, manager, githubResolved())
	if reads != 0 {
		t.Fatal("configuration binding read a private key")
	}
	credential, err := session.Credentials(context.Background())
	if err == nil || reads != 1 || credential.Token != "" {
		t.Fatalf("missing private key: %v", err)
	}
	if strings.Contains(err.Error(), "private-key-secret") || strings.Contains(err.Error(), "transport-token") {
		t.Fatal("private key error leaked its cause")
	}
	broken := githubResolved()
	broken.Target.BaseURL = "https://other.example"
	if _, err := manager.Session(broken); err == nil || reads != 1 {
		t.Fatal("static target mismatch did not fail before key access")
	}
}

func TestGitHubSupportsPKCS8AndCommitOverrides(t *testing.T) {
	fixture := newGitHubFixture(t, false)
	options := fixture.options()
	options.ReadFile = func(string) ([]byte, error) {
		data, err := x509.MarshalPKCS8PrivateKey(fixture.key)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: data}), nil
	}
	resolved := githubResolved()
	resolved.Definition.Commit = config.HostingCommitIdentity{Name: "Worker Automation", Email: "automation@example.com"}
	session := sessionFor(t, NewManager(options), resolved)
	identity, err := session.CommitIdentity(context.Background())
	if err != nil || identity != resolved.Definition.Commit {
		t.Fatalf("commit overrides = %+v, %v", identity, err)
	}
}

func TestBindCapturesConfigurationAndRequiresExplicitNewRun(t *testing.T) {
	resolved := githubResolved()
	cfg := config.Config{Identities: map[string]config.HostingIdentityConfig{"worker-bot": resolved.Definition}, Projects: []config.ProjectRefConfig{{ID: "project", Repo: "org/repo", Identity: "worker-bot"}}}
	ctx := WithManager(context.Background(), NewManager(Options{}))
	bound, err := Bind(ctx, cfg, "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	session, ok := FromContext(bound)
	if !ok {
		t.Fatal("selected identity was not bound")
	}
	original := session.Snapshot()
	updated := cfg.Identities["worker-bot"]
	updated.InstallationID++
	cfg.Identities["worker-bot"] = updated
	boundAgain, err := Bind(bound, cfg, "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	stillBound, _ := FromContext(boundAgain)
	if stillBound != session || session.Snapshot() != original {
		t.Fatal("configuration reload changed an active run")
	}
	if _, err := Bind(bound, cfg, "project", "reviewer"); err == nil {
		t.Fatal("role changed without an execution boundary")
	}
	newRun, err := Rebind(bound, cfg, "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	newSession, _ := FromContext(newRun)
	if newSession.CacheKey() == session.CacheKey() || newSession.Snapshot().Definition.InstallationID != updated.InstallationID {
		t.Fatal("new run did not capture changed configuration")
	}
	cfg.Projects[0].Identity = ""
	legacy, err := Rebind(bound, cfg, "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := FromContext(legacy); ok {
		t.Fatal("new legacy run inherited prior bot")
	}
	if _, err := Credentials(legacy); !errors.Is(err, ErrNoSession) {
		t.Fatalf("legacy credentials = %v", err)
	}
	cfg.Projects[0].Identity = "worker-bot"
	legacyAgain, err := Bind(legacy, cfg, "project", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := FromContext(legacyAgain); ok {
		t.Fatal("reload changed active legacy run into bot mode")
	}
	cfg.Projects[0].Identity = "missing"
	if _, err := Rebind(bound, cfg, "project", "worker"); err == nil {
		t.Fatal("invalid selected identity fell back to legacy")
	}
}

func TestCacheNamespaceIncludesDefinitionAndRepository(t *testing.T) {
	manager := NewManager(Options{})
	resolved := githubResolved()
	first := sessionFor(t, manager, resolved)
	resolved.Role = "reviewer"
	sameIdentity := sessionFor(t, manager, resolved)
	if first.CacheKey() != sameIdentity.CacheKey() {
		t.Fatal("same identity and target failed to share a credential cache")
	}
	for _, change := range []func(*config.ResolvedHostingIdentity){
		func(value *config.ResolvedHostingIdentity) { value.Name = "other-bot" },
		func(value *config.ResolvedHostingIdentity) { value.Definition.PrivateKeyFile = "/new-key.pem" },
		func(value *config.ResolvedHostingIdentity) { value.Definition.InstallationID++ },
		func(value *config.ResolvedHostingIdentity) { value.Definition.Commit.Email = "other@example.com" },
		func(value *config.ResolvedHostingIdentity) { value.Target.Repo = "org/other" },
	} {
		updated := resolved
		change(&updated)
		if sessionFor(t, manager, updated).CacheKey() == first.CacheKey() {
			t.Fatal("changed definition or target reused credential cache")
		}
	}
}

type joiningContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (ctx *joiningContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.joined) })
	return ctx.Context.Done()
}

func TestConcurrentFailedRefreshSharesOneAttempt(t *testing.T) {
	fixture := newGitHubFixture(t, true)
	fixture.revoked.Store(true)
	manager := NewManager(fixture.options())
	session := sessionFor(t, manager, githubResolved())
	leader := make(chan error, 1)
	go func() { _, err := session.Credentials(context.Background()); leader <- err }()
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("token request did not start")
	}
	const waiters = 12
	results := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		ctx := &joiningContext{Context: context.Background(), joined: make(chan struct{})}
		waiter := sessionFor(t, manager, githubResolved())
		go func() { _, err := waiter.Credentials(ctx); results <- err }()
		select {
		case <-ctx.joined:
		case <-time.After(time.Second):
			t.Fatal("waiter did not join refresh")
		}
	}
	close(fixture.release)
	if err := <-leader; err == nil {
		t.Fatal("revoked token succeeded")
	}
	for i := 0; i < waiters; i++ {
		if err := <-results; err == nil {
			t.Fatal("joined caller received credentials for a revoked installation")
		}
	}
	if fixture.mints.Load() != 1 {
		t.Fatal("concurrent callers amplified a failed authentication attempt")
	}
}

// controlledDeadlineContext lets the test end a deadline while both sessions
// are known to be waiting, without depending on scheduler or timer timing.
type controlledDeadlineContext struct{ context.Context }

func (ctx controlledDeadlineContext) Err() error {
	if ctx.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestLiveSessionRetriesAfterRefreshLeaderEnds(t *testing.T) {
	for _, outcome := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(outcome.Error(), func(t *testing.T) {
			fixture := newGitHubFixture(t, true)
			manager := NewManager(fixture.options())
			leaderSession := sessionFor(t, manager, githubResolved())
			waiterSession := sessionFor(t, manager, githubResolved())
			leaderCtx, stopLeader := context.WithCancel(context.Background())
			defer stopLeader()
			var refreshCtx context.Context = leaderCtx
			if outcome == context.DeadlineExceeded {
				refreshCtx = controlledDeadlineContext{Context: leaderCtx}
			}
			leaderResult := make(chan error, 1)
			go func() { _, err := leaderSession.Credentials(refreshCtx); leaderResult <- err }()
			select {
			case <-fixture.started:
			case <-time.After(3 * time.Second):
				t.Fatal("leader refresh did not start")
			}
			waiterCtx := &joiningContext{Context: context.Background(), joined: make(chan struct{})}
			type result struct {
				credential Credential
				err        error
			}
			waiterResult := make(chan result, 1)
			go func() {
				credential, err := waiterSession.Credentials(waiterCtx)
				waiterResult <- result{credential, err}
			}()
			select {
			case <-waiterCtx.joined:
			case <-time.After(time.Second):
				t.Fatal("live session did not join leader refresh")
			}
			stopLeader()
			select {
			case err := <-leaderResult:
				if !errors.Is(err, outcome) {
					t.Errorf("leader error = %v, want %v", err, outcome)
				}
			case <-time.After(time.Second):
				t.Fatal("leader did not observe its context ending")
			}
			close(fixture.release)
			select {
			case actual := <-waiterResult:
				if actual.err != nil || actual.credential.Token != "installation-token-2" {
					t.Fatalf("live waiter inherited leader cancellation: %s, %v", actual.credential, actual.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("live waiter did not retry canceled refresh")
			}
			if fixture.mints.Load() != 2 {
				t.Fatalf("wanted one canceled attempt and one successful retry, got %d", fixture.mints.Load())
			}
		})
	}
}

func TestCacheHitsRecordSecretsForEachRunWithoutRetainingManagerHistory(t *testing.T) {
	fixture := newGitHubFixture(t, false)
	manager := NewManager(fixture.options())
	first := sessionFor(t, manager, githubResolved())
	second := sessionFor(t, manager, githubResolved())
	for _, session := range []*Session{first, second} {
		if _, err := session.Credentials(context.Background()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(session.Redact("installation-token-1"), "installation-token-1") {
			t.Fatal("cache hit did not record its token for redaction")
		}
	}
	fixture.clock.advance(time.Hour)
	if _, err := first.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	newRun := sessionFor(t, manager, githubResolved())
	if _, err := newRun.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(newRun.Redact("installation-token-2"), "installation-token-2") {
		t.Fatal("new run failed to redact current token")
	}
	if newRun.Redact("installation-token-1") != "installation-token-1" {
		t.Fatal("new run retained the daemon's historical tokens")
	}
	if strings.Contains(first.Redact("installation-token-1 installation-token-2"), "installation-token-") {
		t.Fatal("active run lost its earlier token after refresh")
	}
}

func TestGitHubEnterpriseRequiresKnownCommitEmail(t *testing.T) {
	fixture := newGitHubFixture(t, false)
	options := fixture.options()
	serverURL, _ := url.Parse(fixture.server.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	options.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "enterprise.example" || !strings.HasPrefix(request.URL.Path, "/api/v3/") {
			t.Errorf("incorrect GHES API origin or prefix: %s", request.URL)
		}
		copy := request.Clone(request.Context())
		copy.URL.Scheme, copy.URL.Host = serverURL.Scheme, serverURL.Host
		copy.URL.Path, copy.URL.RawPath = strings.TrimPrefix(copy.URL.Path, "/api/v3"), ""
		return transport.RoundTrip(copy)
	})}
	resolved := githubResolved()
	resolved.Definition.BaseURL, resolved.Target.BaseURL = "https://enterprise.example", "https://enterprise.example"
	manager := NewManager(options)
	session := sessionFor(t, manager, resolved)
	if _, err := session.Credentials(context.Background()); err == nil || !strings.Contains(err.Error(), "commit.email") {
		t.Fatalf("GHES invented an unverified noreply address: %v", err)
	}
	resolved.Definition.Commit.Email = "bot@enterprise.example"
	configured := sessionFor(t, manager, resolved)
	credential, err := configured.Credentials(context.Background())
	if err != nil || credential.Email != resolved.Definition.Commit.Email {
		t.Fatalf("GHES configured attribution failed: %v", err)
	}
}

func TestGitHubAPIOriginMapping(t *testing.T) {
	for _, item := range []struct{ web, api string }{
		{"https://github.com", "https://api.github.com"},
		{"https://github.com:443/", "https://api.github.com"},
		{"https://acme.ghe.com", "https://api.acme.ghe.com"},
		{"https://acme.ghe.com:443", "https://api.acme.ghe.com"},
		{"https://enterprise.example", "https://enterprise.example/api/v3"},
		{"https://enterprise.example:8443", "https://enterprise.example:8443/api/v3"},
	} {
		t.Run(item.web, func(t *testing.T) {
			resolved := githubResolved()
			resolved.Definition.BaseURL, resolved.Target.BaseURL = item.web, item.web
			session := sessionFor(t, NewManager(Options{}), resolved)
			if session.APIURL() != item.api {
				t.Errorf("API origin = %s, want %s", session.APIURL(), item.api)
			}
		})
	}
}

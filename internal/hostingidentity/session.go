package hostingidentity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nexu-io/looper/internal/config"
)

// Session is an immutable selection of a named identity, project, and role.
// Its manager may refresh a token, but may never change the verified account.
type Session struct {
	manager  *Manager
	resolved config.ResolvedHostingIdentity
	cacheKey string
	apiURL   string
	entry    *credentialEntry
	secrets  redactor
}

func (session *Session) Name() string                             { return session.resolved.Name }
func (session *Session) Kind() config.HostingIdentityKind         { return session.resolved.Definition.Kind }
func (session *Session) ProjectID() string                        { return session.resolved.ProjectID }
func (session *Session) Role() string                             { return session.resolved.Role }
func (session *Session) Target() config.RepositoryIdentity        { return session.resolved.Target }
func (session *Session) APIURL() string                           { return session.apiURL }
func (session *Session) CacheKey() string                         { return session.cacheKey }
func (session *Session) Snapshot() config.ResolvedHostingIdentity { return session.resolved }

func (session *Session) failure(operation, reason string) error {
	return &Error{Identity: session.Name(), Operation: operation, Reason: reason}
}

func (session *Session) validate() error {
	definition, target := session.resolved.Definition, session.resolved.Target
	base, err := url.Parse(definition.BaseURL)
	if err != nil || !validBaseURL(base) {
		return session.failure("configure", "base URL must be HTTP(S) without credentials, query, or fragment")
	}
	repoParts := strings.Split(target.Repo, "/")
	if len(repoParts) != 2 || !validPathSegment(repoParts[0]) || !validPathSegment(repoParts[1]) {
		return session.failure("configure", "target repository must be owner/name")
	}
	if normalizeURL(definition.BaseURL) != normalizeURL(target.BaseURL) {
		return session.failure("configure", "identity and repository target must use the same base URL")
	}
	switch definition.Kind {
	case config.HostingIdentityGitHubApp:
		if target.Kind != config.ProviderKindGitHub || strings.Trim(base.Path, "/") != "" {
			return session.failure("configure", "GitHub App identity requires a GitHub repository and origin-only base URL")
		}
		if definition.AppID <= 0 || definition.InstallationID <= 0 || strings.TrimSpace(definition.PrivateKeyFile) == "" {
			return session.failure("configure", "GitHub App identity requires app ID, installation ID, and private-key file")
		}
		session.apiURL = strings.TrimRight(base.String(), "/") + "/api/v3"
		if base.Scheme == "https" && (base.Port() == "" || base.Port() == "443") {
			host := strings.ToLower(base.Hostname())
			if host == "github.com" {
				session.apiURL = "https://api.github.com"
			} else if strings.HasSuffix(host, ".ghe.com") {
				session.apiURL = "https://api." + host
			}
		}
	case config.HostingIdentityForgejoToken:
		if target.Kind != config.ProviderKindForgejo || strings.TrimSpace(definition.TokenEnv) == "" {
			return session.failure("configure", "Forgejo token identity requires a Forgejo repository and named token environment variable")
		}
		session.apiURL = strings.TrimRight(base.String(), "/") + "/api/v1"
	default:
		return session.failure("configure", "unsupported identity kind")
	}
	if (definition.Commit.Name != "" && !validCommitName(definition.Commit.Name)) || (definition.Commit.Email != "" && !validEmail(definition.Commit.Email)) {
		return session.failure("configure", "commit attribution is invalid")
	}
	return nil
}

func validBaseURL(base *url.URL) bool {
	return base != nil && (base.Scheme == "http" || base.Scheme == "https") && base.Hostname() != "" && base.User == nil && base.RawQuery == "" && !base.ForceQuery && base.Fragment == "" && base.RawFragment == "" && base.Opaque == ""
}

func normalizeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || !validBaseURL(parsed) {
		return ""
	}
	return strings.ToLower(parsed.Scheme+"://"+parsed.Host) + strings.TrimRight(parsed.EscapedPath(), "/")
}

func validPathSegment(segment string) bool {
	return segment != "" && segment != "." && segment != ".." && !strings.ContainsAny(segment, "/\\?#%") && !strings.ContainsFunc(segment, unicode.IsSpace) && !strings.ContainsFunc(segment, unicode.IsControl)
}

func validCommitName(name string) bool {
	return strings.TrimSpace(name) != "" && !strings.ContainsAny(name, "<>") && !strings.ContainsFunc(name, unicode.IsControl)
}

func validEmail(email string) bool {
	if strings.ContainsAny(email, "<>") || strings.ContainsFunc(email, unicode.IsControl) || strings.ContainsFunc(email, unicode.IsSpace) {
		return false
	}
	// GitHub's official noreply addresses contain an unquoted [bot] local
	// part, which net/mail rejects. Validate Git attribution, not RFC mail.
	at := strings.LastIndexByte(email, '@')
	return at > 0 && at < len(email)-1
}

// Check forces an authentication probe using the captured definition.
func (session *Session) Check(ctx context.Context) error {
	session.Invalidate("")
	_, err := session.Credentials(ctx)
	return err
}

// Invalidate forgets an unusable cached token. Passing the rejected token
// prevents a delayed response from invalidating a newer refresh; an empty
// token explicitly invalidates whichever credential is currently cached.
func (session *Session) Invalidate(token string) {
	session.entry.mu.Lock()
	defer session.entry.mu.Unlock()
	if token == "" || session.entry.credential.Token == token {
		session.entry.credential = Credential{}
		session.entry.refreshAt = time.Time{}
	}
}

// Credentials refreshes only the selected identity. A failed refresh never
// returns an older token and never invokes gh, tea, or ambient credentials.
func (session *Session) Credentials(ctx context.Context) (Credential, error) {
	var sourceToken string
	if session.Kind() == config.HostingIdentityForgejoToken {
		var err error
		sourceToken, err = session.forgejoToken()
		if err != nil {
			return Credential{}, err
		}
	}
	entry := session.entry
	for {
		if err := ctx.Err(); err != nil {
			return Credential{}, session.contextFailure("authenticate", err)
		}
		entry.mu.Lock()
		cached := entry.credential
		if cached.Token != "" && session.manager.now().Before(entry.refreshAt) && (sourceToken == "" || sourceToken == cached.Token) {
			entry.mu.Unlock()
			session.secrets.remember(cached.Token, cached.Login)
			return cached, nil
		}
		if pending := entry.refreshing; pending != nil {
			entry.mu.Unlock()
			select {
			case <-pending.done:
				if err := ctx.Err(); err != nil {
					return Credential{}, session.contextFailure("authenticate", err)
				}
				// Refresh ownership does not transfer the leader's lifetime to
				// other runs. A live waiter can lead a new attempt after the
				// prior owner stops; actual authentication failures stay shared.
				if errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded) {
					continue
				}
				if pending.sourceToken == sourceToken {
					session.secrets.remember(pending.credential.Token, pending.credential.Login)
					return pending.credential, pending.err
				}
				continue
			case <-ctx.Done():
				return Credential{}, session.contextFailure("authenticate", ctx.Err())
			}
		}
		pending := &credentialRefresh{done: make(chan struct{}), sourceToken: sourceToken}
		entry.refreshing = pending
		entry.mu.Unlock()

		credential, refreshAt, err := session.fetch(ctx, sourceToken)
		entry.mu.Lock()
		if err == nil && entry.principal.Login != "" {
			if !strings.EqualFold(entry.principal.Login, credential.Login) || entry.principal.NumericID != credential.NumericID {
				err = session.failure("authenticate", "credential account changed; configure a new named identity to switch accounts")
			} else {
				credential.Login, credential.Name, credential.Email = entry.principal.Login, entry.principal.Name, entry.principal.Email
			}
		}
		if err == nil {
			if entry.principal.Login == "" {
				entry.principal = credential
				entry.principal.Token = ""
				entry.principal.ExpiresAt = time.Time{}
			}
			entry.credential, entry.refreshAt = credential, refreshAt
		} else {
			entry.credential, entry.refreshAt = Credential{}, time.Time{}
		}
		if err == nil {
			pending.credential = credential
		}
		pending.err = err
		close(pending.done)
		entry.refreshing = nil
		entry.mu.Unlock()
		if err != nil {
			return Credential{}, err
		}
		session.secrets.remember(credential.Token, credential.Login)
		return credential, nil
	}
}

func (session *Session) contextFailure(operation string, err error) error {
	return &Error{Identity: session.Name(), Operation: operation, Reason: err.Error(), cause: err}
}

func (session *Session) fetch(ctx context.Context, sourceToken string) (Credential, time.Time, error) {
	if session.Kind() == config.HostingIdentityGitHubApp {
		return session.fetchGitHub(ctx)
	}
	return session.fetchForgejo(ctx, sourceToken)
}

// CommitIdentity returns the verified account's default attribution with the
// operator's captured overrides applied.
func (session *Session) CommitIdentity(ctx context.Context) (config.HostingCommitIdentity, error) {
	credential, err := session.Credentials(ctx)
	if err != nil {
		return config.HostingCommitIdentity{}, err
	}
	return config.HostingCommitIdentity{Name: credential.Name, Email: credential.Email}, nil
}

func (session *Session) attribution(credential Credential) (Credential, error) {
	if override := session.resolved.Definition.Commit; override.Name != "" || override.Email != "" {
		if override.Name != "" {
			credential.Name = override.Name
		}
		if override.Email != "" {
			credential.Email = override.Email
		}
	}
	if !validCommitName(credential.Name) || !validEmail(credential.Email) {
		return Credential{}, session.failure("resolve bot attribution", "account must provide a valid name and email, or configure commit.name and commit.email overrides")
	}
	return credential, nil
}

type redactor struct {
	mu      sync.RWMutex
	secrets map[string]struct{}
}

func (redactor *redactor) remember(secret string, logins ...string) {
	if secret == "" {
		return
	}
	values := []string{secret, url.QueryEscape(secret), url.PathEscape(secret)}
	plain := []string{secret, "x-access-token:" + secret, "oauth2:" + secret}
	for _, login := range logins {
		if login != "" {
			plain = append(plain, login+":"+secret)
		}
	}
	for _, value := range plain {
		values = append(values, base64.StdEncoding.EncodeToString([]byte(value)), base64.RawStdEncoding.EncodeToString([]byte(value)), base64.URLEncoding.EncodeToString([]byte(value)), base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	redactor.mu.Lock()
	defer redactor.mu.Unlock()
	if redactor.secrets == nil {
		redactor.secrets = make(map[string]struct{})
	}
	for _, value := range values {
		redactor.secrets[value] = struct{}{}
	}
}

// Redact covers credentials produced during this binding, including prior
// refreshes and common URL/base64 encodings in daemon subprocess errors.
func (session *Session) Redact(text string) string {
	session.secrets.mu.RLock()
	secrets := make([]string, 0, len(session.secrets.secrets))
	for secret := range session.secrets.secrets {
		secrets = append(secrets, secret)
	}
	session.secrets.mu.RUnlock()
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return text
}

func (session *Session) String() string {
	return fmt.Sprintf("hosting identity %q (%s, %s)", session.Name(), session.ProjectID(), session.Role())
}

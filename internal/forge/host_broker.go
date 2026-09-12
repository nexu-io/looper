package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/processcontainment"
)

const HostSockEnv = TrustedReviewSockEnv
const HostCLIEnv = "LOOPER_HOST_CLI"
const maxHostResponseBytes = 1 << 20
const maxHostPages = 50

// HostRequest is a bounded capability request, not a provider API passthrough.
// The legacy argv fields are accepted only by the existing review authority.
type HostRequest struct {
	Op       string   `json:"op,omitempty"`
	Path     string   `json:"path,omitempty"`
	Method   string   `json:"method,omitempty"`
	Paginate bool     `json:"paginate,omitempty"`
	Diff     bool     `json:"diff,omitempty"`
	PRNumber int64    `json:"prNumber,omitempty"`
	ThreadID string   `json:"threadId,omitempty"`
	Ref      string   `json:"ref,omitempty"`
	Argv     []string `json:"argv,omitempty"`
	Stdin    []byte   `json:"stdin,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
}

func (r HostRequest) hasHostArguments() bool {
	return r.Path != "" || r.Method != "" || r.Paginate || r.Diff || r.PRNumber != 0 || r.ThreadID != "" || r.Ref != ""
}

// TrustedReviewAuthority is present only for native reviewer publication.
// A read capability alone cannot manufacture this policy or submit a review.
type TrustedReviewAuthority struct {
	PRRef  string
	Policy TrustedReviewProxyPolicy
}

type HostBrokerOptions struct {
	Config     config.Config
	RealLooper string
	CWD        string
	Review     *TrustedReviewAuthority
	Tracker    processcontainment.LiveTracker
	HTTPClient *http.Client // Optional transport injection for contract tests.
}

type hostBroker struct {
	options  HostBrokerOptions
	session  *hostingidentity.Session
	client   *http.Client
	snapshot []byte
}

// StartHostBroker binds every operation to the already-selected run identity.
// The socket shares the legacy review transport's limits and drain lifecycle.
func StartHostBroker(ctx context.Context, options HostBrokerOptions) (string, func(), error) {
	session, ok := hostingidentity.FromContext(ctx)
	if !ok {
		return "", nil, hostingidentity.ErrNoSession
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	cwd, err := normalizeTrustedReviewCwd(options.CWD)
	if err != nil {
		return "", nil, err
	}
	options.CWD = cwd
	if !filepath.IsAbs(options.RealLooper) {
		return "", nil, errors.New("hosting broker requires an absolute trusted looper path")
	}
	if _, err := os.Stat(options.RealLooper); err != nil {
		return "", nil, fmt.Errorf("trusted looper executable is unavailable: %w", err)
	}
	if options.Review != nil {
		if session.Role() != "reviewer" {
			return "", nil, errors.New("review publication requires the reviewer role")
		}
		bound := *options.Review
		bound.PRRef, err = normalizeTrustedReviewPRRef(bound.PRRef)
		if err != nil {
			return "", nil, err
		}
		repo, _, _ := strings.Cut(bound.PRRef, "#")
		if err := session.CheckRepository(repo); err != nil {
			return "", nil, err
		}
		bound.Policy, err = normalizeTrustedReviewProxyPolicy(bound.Policy)
		if err != nil {
			return "", nil, err
		}
		options.Review = &bound
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if options.HTTPClient != nil {
		copy := *options.HTTPClient
		client = &copy
	}
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	snapshot, err := marshalTrustedHostingSnapshot(options.Config, session.Snapshot())
	if err != nil {
		return "", nil, err
	}
	b := &hostBroker{options: options, session: session, client: client, snapshot: snapshot}
	return startHostingSocket(ctx, b.handle)
}

func (b *hostBroker) handle(ctx context.Context, conn net.Conn) {
	serveHostingRequest(ctx, conn, func(ctx context.Context, req HostRequest) {
		if req.Op == "" || req.Op == "review.submit" {
			if b.options.Review == nil || req.hasHostArguments() {
				writeHostingError(conn, "this execution has no review publication capability")
				return
			}
			env, err := b.reviewChildEnvironment(ctx)
			if err != nil {
				writeHostingError(conn, err.Error())
				return
			}
			authority := b.options.Review
			runTrustedReviewProxyRequest(ctx, conn, req, b.options.RealLooper, env, authority.PRRef, b.options.CWD, b.snapshot, authority.Policy, b.options.Tracker)
			return
		}
		if len(req.Argv) > 0 || len(req.Stdin) > 0 || req.Cwd != "" {
			writeHostingError(conn, "hosting reads do not accept argv, stdin, or a working directory")
			return
		}
		output, err := b.read(ctx, req)
		if err != nil {
			writeHostingError(conn, b.session.Redact(err.Error()))
			return
		}
		if len(output) > maxHostResponseBytes {
			writeHostingError(conn, "hosting response exceeds size limit")
			return
		}
		_ = json.NewEncoder(conn).Encode(trustedReviewProxyResponse{Stdout: b.session.Redact(string(output))})
	})
}

func (b *hostBroker) read(ctx context.Context, req HostRequest) ([]byte, error) {
	switch req.Op {
	case "identity":
		if req.hasHostArguments() {
			return nil, errors.New("identity takes no arguments")
		}
		credential, err := b.session.Credentials(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Login     string              `json:"login"`
			NumericID int64               `json:"numericId"`
			Name      string              `json:"name"`
			Email     string              `json:"email"`
			Provider  config.ProviderKind `json:"provider"`
			Repo      string              `json:"repo"`
			BaseURL   string              `json:"baseUrl"`
		}{credential.Login, credential.NumericID, credential.Name, credential.Email, b.session.Target().Kind, b.session.Target().Repo, b.session.Target().BaseURL})
	case "api.read":
		if req.PRNumber != 0 || req.ThreadID != "" || req.Ref != "" {
			return nil, errors.New("API reads accept only a repository-relative path, GET, diff and pagination")
		}
		return b.readAPI(ctx, req)
	case "threads.list", "thread.read":
		if req.Path != "" || req.Method != "" || req.Paginate || req.Diff || req.Ref != "" || req.PRNumber <= 0 || (req.Op == "threads.list" && req.ThreadID != "") || (req.Op == "thread.read" && !hostNodeIDPattern.MatchString(req.ThreadID)) {
			return nil, errors.New("thread reads require a positive PR number and, for one thread, its node ID")
		}
		return b.readThreads(ctx, req.PRNumber, req.ThreadID)
	case "git.fetch":
		if req.Path != "" || req.Method != "" || req.Paginate || req.Diff || req.PRNumber != 0 || req.ThreadID != "" || !validHostFetchRef(req.Ref) {
			return nil, errors.New("fetch requires one bounded repository ref")
		}
		if b.options.Config.Tools.GitPath == nil || !filepath.IsAbs(*b.options.Config.Tools.GitPath) {
			return nil, errors.New("hosting fetch requires a configured absolute git path")
		}
		result, err := hostingidentity.RunGit(ctx, shell.Options{Command: *b.options.Config.Tools.GitPath, Args: []string{"fetch", "--no-tags", "--no-recurse-submodules", "origin", req.Ref}, CWD: b.options.CWD, Tracker: b.options.Tracker, MaxCapturedBytes: maxHostResponseBytes}, shell.Run)
		if err != nil {
			return nil, err
		}
		if result.StdoutTruncated || result.StderrTruncated {
			return nil, errors.New("hosting fetch output exceeds size limit")
		}
		return []byte(result.Stdout + result.Stderr), nil
	default:
		return nil, fmt.Errorf("unsupported hosting operation %q", req.Op)
	}
}

var hostNodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,200}$`)
var hostRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)

func validHostFetchRef(ref string) bool {
	return hostRefPattern.MatchString(ref) && !strings.Contains(ref, "..") && !strings.Contains(ref, "//") && !strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".lock")
}

// ProxyHost sends only a typed capability request. It never loads daemon config
// or provider credentials, and uses the caller's context to close on cancel.
func ProxyHost(ctx context.Context, request HostRequest) (string, error) {
	socket := strings.TrimSpace(os.Getenv(HostSockEnv))
	if socket == "" {
		return "", errors.New("hosting capability is unavailable; run this command inside a Looper agent execution")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return "", fmt.Errorf("connect hosting broker: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return "", err
	}
	var response trustedReviewProxyResponse
	raw, err := io.ReadAll(io.LimitReader(conn, maxTrustedReviewProxyResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read hosting response: %w", err)
	}
	if len(raw) > maxTrustedReviewProxyResponseBytes {
		return "", errors.New("hosting response exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return "", fmt.Errorf("decode hosting response: %w", err)
	}
	if response.Error != "" {
		return "", errors.New(response.Error)
	}
	if response.ExitCode != 0 {
		return "", fmt.Errorf("hosting operation failed (exit %d)", response.ExitCode)
	}
	return response.Stdout, nil
}

func trustedHostingChildEnv(env map[string]string) []string {
	result := make([]string, 0, len(env)+2)
	for key, value := range env {
		result = append(result, key+"="+value)
	}
	return append(result, trustedReviewProxySkipEnv+"=1", TrustedReviewConfigFDEnv+"="+strconv.Itoa(TrustedReviewConfigChildFD))
}

func (b *hostBroker) reviewChildEnvironment(ctx context.Context) (map[string]string, error) {
	// Only process necessities enter a trusted child. In particular, no model
	// credentials or unselected bot/provider tokens are inherited from daemon.
	env := make(map[string]string)
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TMP", "TEMP", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	env = hostingidentity.SanitizeAgentEnv(b.options.Config, env)
	if b.session.Kind() == config.HostingIdentityForgejoToken {
		credential, err := b.session.Credentials(ctx)
		if err != nil {
			return nil, err
		}
		env[b.session.Snapshot().Definition.TokenEnv] = credential.Token
	}
	return env, nil
}

func marshalTrustedHostingSnapshot(cfg config.Config, resolved config.ResolvedHostingIdentity) ([]byte, error) {
	raw, err := marshalTrustedReviewConfigSnapshot(cfg)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(struct {
		Config   json.RawMessage                `json:"config"`
		Identity config.ResolvedHostingIdentity `json:"hostingIdentity"`
	}{raw, resolved})
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxTrustedReviewConfigSnapshotSize {
		return nil, errors.New("trusted hosting snapshot exceeds size limit")
	}
	return encoded, nil
}

// request performs one fixed-origin authenticated call and bounds/redacts its
// response. Authentication rejection invalidates this token for the next call;
// it never triggers another account or a personal-CLI fallback.
func (b *hostBroker) request(ctx context.Context, method, path string, payload []byte, accept string) ([]byte, http.Header, error) {
	address := b.session.APIURL() + path
	if err := b.session.CheckAPIURL(address); err != nil {
		return nil, nil, err
	}
	if path == "/graphql" && b.session.Kind() == config.HostingIdentityGitHubApp && strings.HasSuffix(b.session.APIURL(), "/api/v3") {
		// GHES GraphQL has the same origin but uses /api/graphql, unlike REST.
		// Only this fixed server-authored operation can select that path.
		address = strings.TrimSuffix(b.session.APIURL(), "/v3") + "/graphql"
	}
	credential, err := b.session.Credentials(ctx)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, errors.New("cannot construct hosting request")
	}
	auth := "Bearer "
	if b.session.Kind() == config.HostingIdentityForgejoToken {
		auth = "token "
	}
	req.Header.Set("Authorization", auth+credential.Token)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "looper-host-broker")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("hosting request failed")
	}
	defer resp.Body.Close()
	// Job-log downloads may redirect to a signed storage URL. Download it
	// daemon-side without API authorization, and never expose that signed URL.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && method == http.MethodGet && strings.Contains(path, "/actions/jobs/") && strings.Contains(path, "/logs") {
		return b.downloadJobLog(ctx, resp.Header.Get("Location"))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			b.session.Invalidate(credential.Token)
		}
		return nil, nil, &hostingidentity.Error{Identity: b.session.Name(), Operation: "read hosting context", StatusCode: resp.StatusCode, Reason: "hosting server rejected the request"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHostResponseBytes+1))
	if err != nil {
		return nil, nil, errors.New("cannot read hosting response")
	}
	if len(data) > maxHostResponseBytes {
		return nil, nil, errors.New("hosting response exceeds size limit")
	}
	return data, resp.Header, nil
}

func (b *hostBroker) downloadJobLog(ctx context.Context, location string) ([]byte, http.Header, error) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, nil, errors.New("hosting job log redirect is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, errors.New("hosting job log redirect is invalid")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, nil, errors.New("hosting job log download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, errors.New("hosting job log download was rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHostResponseBytes+1))
	if err != nil || len(data) > maxHostResponseBytes {
		return nil, nil, errors.New("hosting job log exceeds response limit or could not be read")
	}
	return data, resp.Header, nil
}

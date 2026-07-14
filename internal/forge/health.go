package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

const (
	defaultForgejoProbeTimeout = 3 * time.Second
	maxForgejoOpenAPIBytes     = 8 << 20
)

type ProbeState string

const (
	ProbeStateSupported   ProbeState = "supported"
	ProbeStateUnsupported ProbeState = "unsupported"
	ProbeStateUnknown     ProbeState = "unknown"
)

type ReachabilityState string

const (
	ReachabilityReachable   ReachabilityState = "reachable"
	ReachabilityUnreachable ReachabilityState = "unreachable"
)

type AuthenticationState string

const (
	AuthenticationValid                AuthenticationState = "valid"
	AuthenticationInvalid              AuthenticationState = "invalid"
	AuthenticationForbidden            AuthenticationState = "forbidden"
	AuthenticationMissingToken         AuthenticationState = "missing_token"
	AuthenticationTeaMissing           AuthenticationState = "tea_missing"
	AuthenticationTeaLoginMissing      AuthenticationState = "tea_login_missing"
	AuthenticationTeaLoginHostMismatch AuthenticationState = "tea_login_host_mismatch"
	AuthenticationTeaAuthFailed        AuthenticationState = "tea_auth_failed"
	AuthenticationUnknown              AuthenticationState = "unknown"
)

type AccessState string

const (
	AccessReadable     AccessState = "readable"
	AccessWritable     AccessState = "writable"
	AccessReadOnly     AccessState = "read_only"
	AccessInsufficient AccessState = "insufficient"
	AccessUnknown      AccessState = "unknown"
)

type CapabilityReport struct {
	Configured      ProbeState `json:"configured"`
	ConfiguredScope string     `json:"configuredScope,omitempty"`
	Observed        ProbeState `json:"observed"`
	Effective       ProbeState `json:"effective"`
	Degraded        bool       `json:"degraded"`
	Reason          string     `json:"reason,omitempty"`
}

type ForgejoProjectHealth struct {
	ProjectID    string                      `json:"projectId"`
	Repository   string                      `json:"repository"`
	Access       AccessState                 `json:"access"`
	Readable     *bool                       `json:"readable,omitempty"`
	Writable     *bool                       `json:"writable,omitempty"`
	StatusCode   int                         `json:"statusCode,omitempty"`
	Capabilities map[string]CapabilityReport `json:"capabilities"`
}

type ForgejoProviderHealth struct {
	ProviderID     string                      `json:"providerId"`
	Kind           ProviderKind                `json:"kind"`
	Reachability   ReachabilityState           `json:"reachability"`
	Authentication AuthenticationState         `json:"authentication"`
	Identity       *Identity                   `json:"identity,omitempty"`
	Version        string                      `json:"version,omitempty"`
	VersionState   ProbeState                  `json:"versionState"`
	StatusCode     int                         `json:"statusCode,omitempty"`
	Capabilities   map[string]CapabilityReport `json:"capabilities"`
	Projects       []ForgejoProjectHealth      `json:"projects"`
}

type ForgejoProbeProject struct {
	ID   string
	Repo string
}

// ProbeForgejoProvider performs bounded, read-only requests. The returned
// structure deliberately contains no endpoint, token environment name, token,
// tea token material, or raw error text so status output remains safe to share.
func ProbeForgejoProvider(ctx context.Context, provider config.ProviderConfig, projects []ForgejoProbeProject, options ...ForgejoOption) ForgejoProviderHealth {
	if config.EffectiveProviderAuth(provider) == config.ProviderAuthTea {
		return probeForgejoProviderTea(ctx, provider, projects, options...)
	}
	return probeForgejoProviderTokenEnv(ctx, provider, projects, options...)
}

func probeForgejoProviderTokenEnv(ctx context.Context, provider config.ProviderConfig, projects []ForgejoProbeProject, options ...ForgejoOption) ForgejoProviderHealth {
	health := ForgejoProviderHealth{
		ProviderID:     provider.ID,
		Kind:           ProviderKindForgejo,
		Reachability:   ReachabilityUnreachable,
		Authentication: AuthenticationUnknown,
		VersionState:   ProbeStateUnknown,
		Capabilities:   forgejoCapabilityReports(nil),
		Projects:       make([]ForgejoProjectHealth, 0, len(projects)),
	}
	for _, project := range projects {
		health.Projects = append(health.Projects, unknownForgejoProjectHealth(project, health.Capabilities))
	}

	baseURL, err := parseForgejoBaseURL(provider.BaseURL)
	if err != nil {
		return health
	}
	probeCtx, cancel := context.WithTimeout(ctx, defaultForgejoProbeTimeout)
	defer cancel()

	client := &http.Client{Timeout: defaultForgejoProbeTimeout}
	probeClient := &ForgejoClient{httpClient: client}
	for _, option := range options {
		if option != nil {
			option(probeClient)
		}
	}
	if probeClient.httpClient != nil {
		client = probeClient.httpClient
	}
	if client.Timeout == 0 || client.Timeout > defaultForgejoProbeTimeout {
		client.Timeout = defaultForgejoProbeTimeout
	}

	token := ""
	if provider.TokenEnv != nil {
		token = LookupProviderToken(*provider.TokenEnv)
	}

	versionResponse, versionErr := forgejoProbeGET(probeCtx, client, forgejoProbeURL(baseURL, "api/v1/version"), "", maxForgejoResponseBodyBytes)
	if versionErr == nil || versionResponse.statusCode > 0 {
		health.Reachability = ReachabilityReachable
	}
	if versionErr == nil {
		var version struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(versionResponse.body, &version) == nil && strings.TrimSpace(version.Version) != "" {
			health.Version = strings.TrimSpace(version.Version)
			health.VersionState = ProbeStateSupported
		}
	} else if versionResponse.statusCode == http.StatusNotFound || versionResponse.statusCode == http.StatusMethodNotAllowed {
		health.VersionState = ProbeStateUnsupported
	}

	openAPIResponse, openAPIErr := forgejoProbeGET(probeCtx, client, forgejoProbeURL(baseURL, "swagger.v1.json"), token, maxForgejoOpenAPIBytes)
	if openAPIErr == nil || openAPIResponse.statusCode > 0 {
		health.Reachability = ReachabilityReachable
	}
	if openAPIErr == nil {
		if paths := decodeForgejoOpenAPIPaths(openAPIResponse.body); paths != nil {
			health.Capabilities = forgejoCapabilityReports(paths)
		}
	}

	if token == "" {
		health.Authentication = AuthenticationMissingToken
		health.Projects = rebuildForgejoProjectHealth(projects, health.Capabilities)
		return health
	}

	userResponse, userErr := forgejoProbeGET(probeCtx, client, forgejoProbeURL(baseURL, "api/v1/user"), token, maxForgejoResponseBodyBytes)
	if userErr == nil || userResponse.statusCode > 0 {
		health.Reachability = ReachabilityReachable
	}
	if userErr != nil {
		health.StatusCode = userResponse.statusCode
		switch userResponse.statusCode {
		case http.StatusUnauthorized:
			health.Authentication = AuthenticationInvalid
		case http.StatusForbidden:
			health.Authentication = AuthenticationForbidden
		default:
			health.Authentication = AuthenticationUnknown
		}
		health.Projects = rebuildForgejoProjectHealth(projects, health.Capabilities)
		return health
	}
	var user forgejoUser
	if err := json.Unmarshal(userResponse.body, &user); err != nil || strings.TrimSpace(user.Login) == "" {
		health.Authentication = AuthenticationUnknown
		health.Projects = rebuildForgejoProjectHealth(projects, health.Capabilities)
		return health
	}
	health.Authentication = AuthenticationValid
	health.Identity = &Identity{Login: user.Login, ID: user.ID}

	health.Projects = make([]ForgejoProjectHealth, 0, len(projects))
	for _, project := range projects {
		health.Projects = append(health.Projects, probeForgejoProject(probeCtx, client, baseURL, token, project, health.Capabilities))
	}
	return health
}

func probeForgejoProviderTea(ctx context.Context, provider config.ProviderConfig, projects []ForgejoProbeProject, options ...ForgejoOption) ForgejoProviderHealth {
	health := ForgejoProviderHealth{
		ProviderID:     provider.ID,
		Kind:           ProviderKindForgejo,
		Reachability:   ReachabilityUnreachable,
		Authentication: AuthenticationUnknown,
		VersionState:   ProbeStateUnknown,
		Capabilities:   forgejoCapabilityReports(nil),
		Projects:       make([]ForgejoProjectHealth, 0, len(projects)),
	}
	for _, project := range projects {
		health.Projects = append(health.Projects, unknownForgejoProjectHealth(project, health.Capabilities))
	}

	baseURL, err := parseForgejoBaseURL(provider.BaseURL)
	if err != nil {
		return health
	}

	probeClient := &ForgejoClient{}
	for _, option := range options {
		if option != nil {
			option(probeClient)
		}
	}
	runner := probeClient.teaRunner
	if runner == nil {
		runner = defaultTeaRunner{}
	}

	probeCtx, cancel := context.WithTimeout(ctx, defaultForgejoProbeTimeout)
	defer cancel()

	// Unauthenticated version probe for reachability (no token).
	httpClient := &http.Client{Timeout: defaultForgejoProbeTimeout}
	if probeClient.httpClient != nil {
		httpClient = probeClient.httpClient
		if httpClient.Timeout == 0 || httpClient.Timeout > defaultForgejoProbeTimeout {
			httpClient.Timeout = defaultForgejoProbeTimeout
		}
	}
	versionResponse, versionErr := forgejoProbeGET(probeCtx, httpClient, forgejoProbeURL(baseURL, "api/v1/version"), "", maxForgejoResponseBodyBytes)
	if versionErr == nil || versionResponse.statusCode > 0 {
		health.Reachability = ReachabilityReachable
	}
	if versionErr == nil {
		var version struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(versionResponse.body, &version) == nil && strings.TrimSpace(version.Version) != "" {
			health.Version = strings.TrimSpace(version.Version)
			health.VersionState = ProbeStateSupported
		}
	} else if versionResponse.statusCode == http.StatusNotFound || versionResponse.statusCode == http.StatusMethodNotAllowed {
		health.VersionState = ProbeStateUnsupported
	}

	teaPath, _, validateErr := ValidateTeaLoginForProvider(probeCtx, provider, runner, probeClient.lookPath)
	if validateErr != nil {
		health.Authentication = authenticationStateFromTeaError(validateErr)
		health.Projects = rebuildForgejoProjectHealth(projects, health.Capabilities)
		return health
	}

	openAPITransport := newTeaTransport(teaPath, strings.TrimSpace(*provider.TeaLogin), baseURL, defaultForgejoProbeTimeout, runner)
	// swagger.v1.json lives at the server root, not under /api/v1; pass absolute URL.
	if openAPIResponse, openAPIErr := openAPITransport.doRaw(probeCtx, http.MethodGet, forgejoProbeURL(baseURL, "swagger.v1.json"), nil, nil); openAPIErr == nil {
		health.Reachability = ReachabilityReachable
		if paths := decodeForgejoOpenAPIPaths(openAPIResponse.body); paths != nil {
			health.Capabilities = forgejoCapabilityReports(paths)
		}
	}

	userTransport := newTeaTransport(teaPath, strings.TrimSpace(*provider.TeaLogin), baseURL, defaultForgejoProbeTimeout, runner)
	userResponse, userErr := userTransport.doRaw(probeCtx, http.MethodGet, "user", nil, nil)
	if userErr != nil {
		health.Authentication = authenticationStateFromTeaError(userErr)
		if httpErr, ok := userErr.(*ForgejoHTTPError); ok {
			health.StatusCode = httpErr.StatusCode
			switch httpErr.StatusCode {
			case http.StatusUnauthorized:
				health.Authentication = AuthenticationInvalid
			case http.StatusForbidden:
				health.Authentication = AuthenticationForbidden
			}
		}
		health.Projects = rebuildForgejoProjectHealth(projects, health.Capabilities)
		return health
	}
	health.Reachability = ReachabilityReachable
	var user forgejoUser
	if err := json.Unmarshal(userResponse.body, &user); err != nil || strings.TrimSpace(user.Login) == "" {
		health.Authentication = AuthenticationUnknown
		health.Projects = rebuildForgejoProjectHealth(projects, health.Capabilities)
		return health
	}
	health.Authentication = AuthenticationValid
	health.Identity = &Identity{Login: user.Login, ID: user.ID}

	health.Projects = make([]ForgejoProjectHealth, 0, len(projects))
	for _, project := range projects {
		health.Projects = append(health.Projects, probeForgejoProjectTea(probeCtx, userTransport, project, health.Capabilities))
	}
	return health
}

func authenticationStateFromTeaError(err error) AuthenticationState {
	var teaErr *TeaAuthError
	if errors.As(err, &teaErr) {
		switch teaErr.Code {
		case TeaErrorMissing:
			return AuthenticationTeaMissing
		case TeaErrorLoginMissing:
			return AuthenticationTeaLoginMissing
		case TeaErrorLoginHostMismatch:
			return AuthenticationTeaLoginHostMismatch
		case TeaErrorAuthFailed:
			return AuthenticationTeaAuthFailed
		}
	}
	var httpErr *ForgejoHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized:
			return AuthenticationInvalid
		case http.StatusForbidden:
			return AuthenticationForbidden
		}
	}
	return AuthenticationUnknown
}

func probeForgejoProjectTea(ctx context.Context, transport *teaTransport, project ForgejoProbeProject, capabilities map[string]CapabilityReport) ForgejoProjectHealth {
	health := unknownForgejoProjectHealth(project, capabilities)
	repoPath := "repos/" + strings.Trim(strings.TrimSpace(project.Repo), "/")
	response, err := transport.doRaw(ctx, http.MethodGet, repoPath, nil, nil)
	if err != nil {
		if httpErr, ok := err.(*ForgejoHTTPError); ok {
			health.StatusCode = httpErr.StatusCode
			if httpErr.StatusCode == http.StatusForbidden || httpErr.StatusCode == http.StatusNotFound {
				health.Access = AccessInsufficient
				readable := false
				health.Readable = &readable
			}
		}
		return health
	}
	var repository struct {
		Permissions struct {
			Pull  bool `json:"pull"`
			Push  bool `json:"push"`
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if json.Unmarshal(response.body, &repository) != nil {
		return health
	}
	readable := true
	writable := repository.Permissions.Push || repository.Permissions.Admin
	health.Readable = &readable
	health.Writable = &writable
	if writable {
		health.Access = AccessWritable
	} else {
		health.Access = AccessReadOnly
		health.Capabilities = restrictForgejoWriteCapabilities(capabilities, ProbeStateUnsupported, "repository is not writable")
	}
	return health
}

func ProbeForgejoReviewCommentResolution(ctx context.Context, provider config.ProviderConfig, repo string, options ...ForgejoOption) (ProbeState, error) {
	health := ProbeForgejoProvider(ctx, provider, []ForgejoProbeProject{{Repo: repo}}, options...)
	if health.Authentication == AuthenticationMissingToken ||
		health.Authentication == AuthenticationInvalid ||
		health.Authentication == AuthenticationForbidden ||
		health.Authentication == AuthenticationTeaMissing ||
		health.Authentication == AuthenticationTeaLoginMissing ||
		health.Authentication == AuthenticationTeaLoginHostMismatch ||
		health.Authentication == AuthenticationTeaAuthFailed {
		return ProbeStateUnknown, fmt.Errorf("forgejo capability probe authentication = %s", health.Authentication)
	}
	report := health.Capabilities["reviewCommentResolve"]
	if len(health.Projects) > 0 {
		report = health.Projects[0].Capabilities["reviewCommentResolve"]
	}
	return report.Effective, nil
}

type forgejoProbeResponse struct {
	statusCode int
	body       []byte
}

func forgejoProbeGET(ctx context.Context, client *http.Client, endpoint, token string, maxBytes int64) (forgejoProbeResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return forgejoProbeResponse{}, err
	}
	request.Header.Set("Accept", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "token "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return forgejoProbeResponse{}, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	result := forgejoProbeResponse{statusCode: response.StatusCode, body: body}
	if readErr != nil {
		return result, readErr
	}
	if int64(len(body)) > maxBytes {
		return result, errors.New("forgejo probe response too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, errors.New(http.StatusText(response.StatusCode))
	}
	return result, nil
}

func forgejoProbeURL(baseURL *url.URL, path string) string {
	result := *baseURL
	result.Path = strings.TrimRight(baseURL.Path, "/") + "/" + strings.TrimLeft(path, "/")
	result.RawPath = ""
	result.RawQuery = ""
	result.Fragment = ""
	return result.String()
}

func decodeForgejoOpenAPIPaths(body []byte) map[string]map[string]json.RawMessage {
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &document) != nil || document.Paths == nil {
		return nil
	}
	return document.Paths
}

func forgejoCapabilityReports(paths map[string]map[string]json.RawMessage) map[string]CapabilityReport {
	static, _ := StaticCapabilities(ProviderKindForgejo)
	configured := map[string]bool{
		"reviewRequests":       static.ReviewRequests,
		"nativeReviews":        static.NativeReviews,
		"reviewCommentResolve": static.ReviewCommentResolution != ReviewCommentResolutionDisabled && static.ReviewCommentResolution != "",
		"merge":                static.AutoMerge,
		"webhooks":             static.Webhooks,
		"dependencies":         static.Dependencies,
	}
	observed := map[string]ProbeState{
		"reviewRequests": forgejoOpenAPISupport(paths, http.MethodPost, "/repos/{owner}/{repo}/pulls/{index}/requested_reviewers"),
		// Native review runs require list (GET) for discovery/marker verification,
		// create (POST) for publication, and per-review comments (GET) because
		// ListPullRequestReviews eagerly loads that endpoint for marker checks.
		"nativeReviews": forgejoOpenAPISupportAnd(
			forgejoOpenAPISupportAll(paths, "/repos/{owner}/{repo}/pulls/{index}/reviews", http.MethodGet, http.MethodPost),
			forgejoOpenAPISupport(paths, http.MethodGet, "/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments"),
		),
		"reviewCommentResolve": forgejoOpenAPISupport(paths, http.MethodPost, "/repos/{owner}/{repo}/pulls/comments/{id}/resolve"),
		"merge":                forgejoOpenAPISupport(paths, http.MethodPost, "/repos/{owner}/{repo}/pulls/{index}/merge"),
		"webhooks":             forgejoOpenAPISupport(paths, http.MethodPost, "/repos/{owner}/{repo}/hooks"),
		"dependencies":         forgejoOpenAPISupport(paths, http.MethodGet, "/repos/{owner}/{repo}/issues/{index}/dependencies"),
	}
	reports := make(map[string]CapabilityReport, len(configured))
	for name, enabled := range configured {
		configuredState := ProbeStateUnsupported
		if enabled {
			configuredState = ProbeStateSupported
		}
		report := CapabilityReport{Configured: configuredState, Observed: observed[name], Effective: ProbeStateUnsupported}
		if name == "reviewCommentResolve" {
			report.ConfiguredScope = string(static.ReviewCommentResolution)
		}
		if enabled {
			report.Effective = observed[name]
			if observed[name] != ProbeStateSupported {
				report.Degraded = true
				report.Reason = "configured capability is not confirmed by the provider contract"
			}
		}
		reports[name] = report
	}
	return reports
}

func forgejoOpenAPISupport(paths map[string]map[string]json.RawMessage, method, path string) ProbeState {
	if paths == nil {
		return ProbeStateUnknown
	}
	methods, ok := paths[path]
	if !ok {
		return ProbeStateUnsupported
	}
	if _, ok := methods[strings.ToLower(method)]; !ok {
		return ProbeStateUnsupported
	}
	return ProbeStateSupported
}

// forgejoOpenAPISupportAll requires every listed method on path to be present.
// Unknown (missing OpenAPI document) wins over unsupported so callers can
// distinguish "could not probe" from "probed and missing".
func forgejoOpenAPISupportAll(paths map[string]map[string]json.RawMessage, path string, methods ...string) ProbeState {
	if paths == nil {
		return ProbeStateUnknown
	}
	if len(methods) == 0 {
		return ProbeStateUnsupported
	}
	state := ProbeStateSupported
	for _, method := range methods {
		next := forgejoOpenAPISupport(paths, method, path)
		if next == ProbeStateUnknown {
			return ProbeStateUnknown
		}
		if next != ProbeStateSupported {
			state = ProbeStateUnsupported
		}
	}
	return state
}

// forgejoOpenAPISupportAnd combines independent OpenAPI path probes. Unknown
// wins; otherwise every input must be Supported for the result to be Supported.
func forgejoOpenAPISupportAnd(states ...ProbeState) ProbeState {
	if len(states) == 0 {
		return ProbeStateUnsupported
	}
	state := ProbeStateSupported
	for _, next := range states {
		if next == ProbeStateUnknown {
			return ProbeStateUnknown
		}
		if next != ProbeStateSupported {
			state = ProbeStateUnsupported
		}
	}
	return state
}

func probeForgejoProject(ctx context.Context, client *http.Client, baseURL *url.URL, token string, project ForgejoProbeProject, capabilities map[string]CapabilityReport) ForgejoProjectHealth {
	health := unknownForgejoProjectHealth(project, capabilities)
	repoPath := "api/v1/repos/" + strings.Trim(strings.TrimSpace(project.Repo), "/")
	response, err := forgejoProbeGET(ctx, client, forgejoProbeURL(baseURL, repoPath), token, maxForgejoResponseBodyBytes)
	if err != nil {
		health.StatusCode = response.statusCode
		if response.statusCode == http.StatusForbidden || response.statusCode == http.StatusNotFound {
			health.Access = AccessInsufficient
			readable := false
			health.Readable = &readable
		}
		return health
	}
	var repository struct {
		Permissions struct {
			Pull  bool `json:"pull"`
			Push  bool `json:"push"`
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if json.Unmarshal(response.body, &repository) != nil {
		return health
	}
	readable := true
	writable := repository.Permissions.Push || repository.Permissions.Admin
	health.Readable = &readable
	health.Writable = &writable
	if writable {
		health.Access = AccessWritable
	} else {
		health.Access = AccessReadOnly
		health.Capabilities = restrictForgejoWriteCapabilities(capabilities, ProbeStateUnsupported, "repository is not writable")
	}
	return health
}

func unknownForgejoProjectHealth(project ForgejoProbeProject, capabilities map[string]CapabilityReport) ForgejoProjectHealth {
	return ForgejoProjectHealth{ProjectID: project.ID, Repository: project.Repo, Access: AccessUnknown, Capabilities: restrictForgejoWriteCapabilities(capabilities, ProbeStateUnknown, "repository write access is unknown")}
}

func rebuildForgejoProjectHealth(projects []ForgejoProbeProject, capabilities map[string]CapabilityReport) []ForgejoProjectHealth {
	health := make([]ForgejoProjectHealth, 0, len(projects))
	for _, project := range projects {
		health = append(health, unknownForgejoProjectHealth(project, capabilities))
	}
	return health
}

func restrictForgejoWriteCapabilities(capabilities map[string]CapabilityReport, state ProbeState, reason string) map[string]CapabilityReport {
	result := make(map[string]CapabilityReport, len(capabilities))
	for name, report := range capabilities {
		if report.Configured == ProbeStateSupported && (name == "reviewRequests" || name == "nativeReviews" || name == "reviewCommentResolve" || name == "merge" || name == "webhooks") && report.Effective == ProbeStateSupported {
			report.Effective = state
			report.Degraded = true
			report.Reason = reason
		}
		result[name] = report
	}
	return result
}

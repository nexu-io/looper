package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestProbeForgejoProviderReportsHealthAccessAndCapabilities(t *testing.T) {
	t.Setenv("FORGEJO_HEALTH_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); r.URL.Path != "/api/v1/version" && got != "token secret" {
			t.Fatalf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"version":"15.0.4+gitea-1.24.6"}`))
		case "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{` +
				`"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}},` +
				`"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}},` +
				`"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}},` +
				`"/repos/{owner}/{repo}/pulls/comments/{id}/resolve":{"post":{}},` +
				`"/repos/{owner}/{repo}/pulls/{index}/merge":{"post":{}},` +
				`"/repos/{owner}/{repo}/hooks":{"post":{}}}}`))
		case "/api/v1/user":
			_, _ = w.Write([]byte(`{"id":7,"login":"forge-bot"}`))
		case "/api/v1/repos/acme/looper":
			_, _ = w.Write([]byte(`{"permissions":{"pull":true,"push":true,"admin":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	health := ProbeForgejoProvider(context.Background(), forgejoProviderConfig(server.URL, "FORGEJO_HEALTH_TOKEN"), []ForgejoProbeProject{{ID: "looper", Repo: "acme/looper"}}, WithHTTPClient(server.Client()))
	if health.Reachability != ReachabilityReachable || health.Authentication != AuthenticationValid {
		t.Fatalf("provider health = %#v", health)
	}
	if health.Identity == nil || health.Identity.Login != "forge-bot" || health.Version != "15.0.4+gitea-1.24.6" {
		t.Fatalf("identity/version = %#v / %q", health.Identity, health.Version)
	}
	resolve := health.Capabilities["reviewCommentResolve"]
	if resolve.Configured != ProbeStateSupported || resolve.ConfiguredScope != string(ReviewCommentResolutionManualOnly) || resolve.Observed != ProbeStateSupported || resolve.Effective != ProbeStateSupported || resolve.Degraded {
		t.Fatalf("reviewCommentResolve = %#v", resolve)
	}
	nativeReview := health.Capabilities["nativeReviews"]
	if nativeReview.Configured != ProbeStateSupported || nativeReview.Observed != ProbeStateSupported || nativeReview.Effective != ProbeStateSupported {
		t.Fatalf("nativeReviews = %#v", nativeReview)
	}
	if len(health.Projects) != 1 || health.Projects[0].Access != AccessWritable {
		t.Fatalf("projects = %#v", health.Projects)
	}
}

func TestProbeForgejoProviderRequiresGetAndPostForNativeReviews(t *testing.T) {
	t.Setenv("FORGEJO_HEALTH_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); r.URL.Path != "/api/v1/version" && got != "token secret" {
			t.Fatalf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"version":"15.0.4+gitea-1.24.6"}`))
		case "/swagger.v1.json":
			// POST-only reviews must not mark nativeReviews observed/effective.
			_, _ = w.Write([]byte(`{"paths":{` +
				`"/repos/{owner}/{repo}/pulls/{index}/reviews":{"post":{}},` +
				`"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}}}}`))
		case "/api/v1/user":
			_, _ = w.Write([]byte(`{"id":7,"login":"forge-bot"}`))
		case "/api/v1/repos/acme/looper":
			_, _ = w.Write([]byte(`{"permissions":{"pull":true,"push":true,"admin":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	health := ProbeForgejoProvider(context.Background(), forgejoProviderConfig(server.URL, "FORGEJO_HEALTH_TOKEN"), []ForgejoProbeProject{{ID: "looper", Repo: "acme/looper"}}, WithHTTPClient(server.Client()))
	nativeReview := health.Capabilities["nativeReviews"]
	if nativeReview.Configured != ProbeStateSupported {
		t.Fatalf("nativeReviews.Configured = %s, want supported", nativeReview.Configured)
	}
	if nativeReview.Observed == ProbeStateSupported || nativeReview.Effective == ProbeStateSupported {
		t.Fatalf("nativeReviews = %#v, want observed/effective unsupported when GET is missing", nativeReview)
	}
	if !nativeReview.Degraded {
		t.Fatalf("nativeReviews.Degraded = false, want true when only POST is advertised")
	}
}

func TestProbeForgejoProviderRequiresReviewCommentsForNativeReviews(t *testing.T) {
	t.Setenv("FORGEJO_HEALTH_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); r.URL.Path != "/api/v1/version" && got != "token secret" {
			t.Fatalf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"version":"15.0.4+gitea-1.24.6"}`))
		case "/swagger.v1.json":
			// List/create reviews without per-review comments must not mark
			// nativeReviews observed: ListPullRequestReviews fetches comments.
			_, _ = w.Write([]byte(`{"paths":{` +
				`"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}}}}`))
		case "/api/v1/user":
			_, _ = w.Write([]byte(`{"id":7,"login":"forge-bot"}`))
		case "/api/v1/repos/acme/looper":
			_, _ = w.Write([]byte(`{"permissions":{"pull":true,"push":true,"admin":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	health := ProbeForgejoProvider(context.Background(), forgejoProviderConfig(server.URL, "FORGEJO_HEALTH_TOKEN"), []ForgejoProbeProject{{ID: "looper", Repo: "acme/looper"}}, WithHTTPClient(server.Client()))
	nativeReview := health.Capabilities["nativeReviews"]
	if nativeReview.Configured != ProbeStateSupported {
		t.Fatalf("nativeReviews.Configured = %s, want supported", nativeReview.Configured)
	}
	if nativeReview.Observed == ProbeStateSupported || nativeReview.Effective == ProbeStateSupported {
		t.Fatalf("nativeReviews = %#v, want observed/effective unsupported when review comments GET is missing", nativeReview)
	}
	if !nativeReview.Degraded {
		t.Fatalf("nativeReviews.Degraded = false, want true when review comments endpoint is missing")
	}
}

func TestProbeForgejoProviderDifferentiatesFailureStates(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		userStatus int
		repoStatus int
		wantAuth   AuthenticationState
		wantAccess AccessState
	}{
		{name: "missing token", wantAuth: AuthenticationMissingToken, wantAccess: AccessUnknown},
		{name: "invalid token", token: "bad", userStatus: http.StatusUnauthorized, wantAuth: AuthenticationInvalid, wantAccess: AccessUnknown},
		{name: "token permission forbidden", token: "limited", userStatus: http.StatusForbidden, wantAuth: AuthenticationForbidden, wantAccess: AccessUnknown},
		{name: "insufficient repository access", token: "valid", repoStatus: http.StatusForbidden, wantAuth: AuthenticationValid, wantAccess: AccessInsufficient},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FORGEJO_HEALTH_TOKEN", test.token)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/version":
					_, _ = w.Write([]byte(`{"version":"15.0.4"}`))
				case "/swagger.v1.json":
					_, _ = w.Write([]byte(`{"paths":{}}`))
				case "/api/v1/user":
					if test.userStatus != 0 {
						http.Error(w, "denied", test.userStatus)
						return
					}
					_, _ = w.Write([]byte(`{"id":7,"login":"forge-bot"}`))
				case "/api/v1/repos/acme/looper":
					if test.repoStatus != 0 {
						http.Error(w, "denied", test.repoStatus)
						return
					}
					_, _ = w.Write([]byte(`{"permissions":{"pull":true,"push":false}}`))
				}
			}))
			defer server.Close()

			health := ProbeForgejoProvider(context.Background(), forgejoProviderConfig(server.URL, "FORGEJO_HEALTH_TOKEN"), []ForgejoProbeProject{{ID: "looper", Repo: "acme/looper"}}, WithHTTPClient(server.Client()))
			if health.Authentication != test.wantAuth || health.Projects[0].Access != test.wantAccess {
				t.Fatalf("authentication/access = %s/%s, want %s/%s", health.Authentication, health.Projects[0].Access, test.wantAuth, test.wantAccess)
			}
		})
	}
}

func TestProbeForgejoProviderReportsUnreachableAndUnsupportedContract(t *testing.T) {
	t.Setenv("FORGEJO_HEALTH_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	baseURL := server.URL
	server.Close()

	health := ProbeForgejoProvider(context.Background(), forgejoProviderConfig(baseURL, "FORGEJO_HEALTH_TOKEN"), nil, WithTimeout(50*time.Millisecond))
	if health.Reachability != ReachabilityUnreachable || health.Authentication != AuthenticationUnknown || health.VersionState != ProbeStateUnknown {
		t.Fatalf("health = %#v", health)
	}

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	health = ProbeForgejoProvider(context.Background(), forgejoProviderConfig(server.URL, "FORGEJO_HEALTH_TOKEN"), nil, WithHTTPClient(server.Client()))
	if health.Reachability != ReachabilityReachable || health.VersionState != ProbeStateUnsupported || health.Capabilities["reviewCommentResolve"].Observed != ProbeStateUnknown {
		t.Fatalf("unsupported health = %#v", health)
	}
}

func forgejoProviderConfig(baseURL, tokenEnv string) config.ProviderConfig {
	return config.ProviderConfig{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: baseURL, TokenEnv: &tokenEnv}
}

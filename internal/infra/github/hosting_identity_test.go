package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestHostingIdentityPartitionsGatewayAndSharedDiscoveryCaches(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v3/app", strings.HasSuffix(r.URL.Path, "/access_tokens"):
			parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
			if len(parts) != 3 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Issuer string `json:"iss"`
			}
			_ = json.Unmarshal(body, &claims)
			id, _ := strconv.Atoi(claims.Issuer)
			if r.URL.Path == "/api/v3/app" {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "slug": "app-" + claims.Issuer})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "installation-" + claims.Issuer, "expires_at": time.Now().Add(time.Hour)})
			}
		case r.URL.Path == "/api/v3/repos/acme/looper":
			_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "acme/looper"})
		case strings.HasPrefix(r.URL.Path, "/api/v3/users/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "login": strings.TrimPrefix(r.URL.Path, "/api/v3/users/"), "email": "bot@example.com"})
		default:
			t.Errorf("unexpected identity API request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	manager := hostingidentity.NewManager(hostingidentity.Options{ReadFile: func(string) ([]byte, error) { return keyPEM, nil }})
	ctx := hostingidentity.WithManager(context.Background(), manager)
	counts := make(map[string]int)
	gateway := New(Options{DiscoveryCacheTTL: time.Hour, GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		actor := options.Env["GH_ENTERPRISE_TOKEN"]
		if actor == "" {
			actor = "legacy"
		}
		args := strings.Join(options.Args, " ")
		if strings.HasPrefix(args, "api user ") {
			if actor != "legacy" {
				t.Fatal("installation token used unsupported /user endpoint")
			}
			counts["legacy-user"]++
			return shell.Result{Stdout: "personal-user"}, nil
		}
		if strings.HasPrefix(args, "issue list ") {
			counts[actor]++
			return shell.Result{Stdout: fmt.Sprintf(`[{"number":27,"title":%q,"state":"OPEN"}]`, strings.ReplaceAll(actor, "installation-", "visible-to-bot-"))}, nil
		}
		return shell.Result{}, fmt.Errorf("unexpected gh command %q", args)
	}})
	tick := NewDiscoveryTickState()
	snapshot := NewDiscoverySnapshot(gateway, tick, DiscoverySnapshotOptions{})
	ctx = ContextWithDiscoverySnapshot(ctx, snapshot)
	bind := func(id int64, name string) context.Context {
		t.Helper()
		bound, err := hostingidentity.BindResolved(ctx, config.ResolvedHostingIdentity{Name: name, Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityGitHubApp, BaseURL: server.URL, AppID: id, InstallationID: id, PrivateKeyFile: "/snapshot.pem"}, Target: config.RepositoryIdentity{Kind: config.ProviderKindGitHub, BaseURL: server.URL, Repo: "acme/looper"}, ProjectID: "project", Role: "worker"})
		if err != nil {
			t.Fatal(err)
		}
		return bound
	}
	a, b, reloaded := bind(10, "bot-a"), bind(20, "bot-b"), bind(30, "bot-a")
	for _, test := range []struct {
		ctx          context.Context
		actor, login string
	}{{a, "installation-10", "app-10[bot]"}, {b, "installation-20", "app-20[bot]"}, {ctx, "legacy", "personal-user"}, {reloaded, "installation-30", "app-30[bot]"}, {a, "installation-10", "app-10[bot]"}} {
		// Repeat through both the shared snapshot and a new snapshot sharing
		// the gateway cache and tick's CWD login cache.
		for _, current := range []context.Context{test.ctx, test.ctx, ContextWithDiscoverySnapshot(test.ctx, NewDiscoverySnapshot(gateway, tick, DiscoverySnapshotOptions{}))} {
			issues, err := gateway.ListOpenIssues(current, ListOpenIssuesInput{Repo: "acme/looper", CWD: "/same-checkout", Limit: 10})
			if err != nil || len(issues) != 1 || issues[0].Title != strings.ReplaceAll(test.actor, "installation-", "visible-to-bot-") {
				t.Fatalf("%s saw another identity's issues: %#v, %v", test.actor, issues, err)
			}
			login, err := gateway.GetCurrentUserLogin(current, "/same-checkout")
			if err != nil || login != test.login {
				t.Fatalf("%s actor = %q, %v", test.actor, login, err)
			}
		}
	}
	for _, actor := range []string{"installation-10", "installation-20", "installation-30", "legacy", "legacy-user"} {
		if counts[actor] != 1 {
			t.Errorf("%s calls = %d, want one per identity", actor, counts[actor])
		}
	}
}

package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestForgejoWebhookClientUsesProjectIdentityAndProviderAuthForOrphans(t *testing.T) {
	t.Setenv("WEBHOOK_ADMIN_TOKEN", "admin-token")
	t.Setenv("WEBHOOK_PROVIDER_TOKEN", "provider-token")
	wantToken := "admin-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+wantToken {
			t.Error("webhook used a different hosting identity")
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/forge/api/v1/user":
			_, _ = w.Write([]byte(`{"id":1,"login":"admin","email":"bot@example.com"}`))
		case "/forge/api/v1/repos/acme/app":
			_, _ = w.Write([]byte(`{"full_name":"acme/app","permissions":{"admin":true,"push":true,"pull":true}}`))
		case "/forge/api/v1/repos/acme/app/hooks/11":
			_, _ = w.Write([]byte(`{"id":11,"type":"forgejo","active":true}`))
		default:
			t.Errorf("unexpected webhook identity request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := config.Config{
		Providers:  []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: server.URL + "/forge", TokenEnv: stringPtr("WEBHOOK_PROVIDER_TOKEN")}},
		Projects:   []config.ProjectRefConfig{{ID: "app", Provider: "fj", Repo: "acme/app", Identity: "admin"}},
		Identities: map[string]config.HostingIdentityConfig{"admin": {Kind: config.HostingIdentityForgejoToken, BaseURL: server.URL + "/forge", TokenEnv: "WEBHOOK_ADMIN_TOKEN"}},
	}
	// Repository administration does not inherit the discovery role's identity.
	cfg.Roles.Reviewer.Identity = "missing-role-identity"
	key := config.WebhookRepositoryKey(cfg, cfg.Projects[0])
	for _, orphan := range []bool{false, true} {
		if orphan {
			cfg.Projects = nil
			wantToken = "provider-token"
		}
		client, err := NewForgejoWebhookClient(context.Background(), cfg, key)
		if err != nil {
			t.Fatal(err)
		}
		hook, found, err := client.GetWebhook(context.Background(), 11)
		if err != nil || !found || hook.ID != 11 {
			t.Fatalf("orphan=%t hook=%#v found=%t error=%v", orphan, hook, found, err)
		}
	}
}

func TestForgejoWebhookClientRejectsRedirectAndRedactsSecret(t *testing.T) {
	t.Setenv("WEBHOOK_TRANSPORT_TOKEN", "provider-token")
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(true)
		_, _ = w.Write([]byte(`{"id":99}`))
	}))
	defer destination.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const secret = "new-signing-secret"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"rejected ` + secret + `"}`))
			}))
			defer server.Close()
			cfg := config.Config{Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("WEBHOOK_TRANSPORT_TOKEN")}}}
			client, err := NewForgejoWebhookClient(context.Background(), cfg, server.URL+"/acme/app")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CreateWebhook(context.Background(), "https://hooks.example/app", secret)
			var apiErr *ForgejoHTTPError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status || strings.Contains(err.Error(), secret) || leaked.Load() {
				t.Fatalf("unsafe mutation: err=%v redirected=%t", err, leaked.Load())
			}
		})
	}
}

func TestForgejoWebhookTeaRequiresHTTPStatusForMutations(t *testing.T) {
	for _, status := range []string{"HTTP/1.1 403 Forbidden\n\n", ""} {
		t.Run(status, func(t *testing.T) {
			runner := &recordingTeaRunner{
				loginsJSON: `[{"name":"chosen","url":"https://code.example","default":false}]`,
				apiHandlers: map[string]teaAPIResponse{
					"DELETE /repos/acme/app/hooks/11": {Stdout: `{"message":"forbidden"}`, Stderr: status},
				},
			}
			cfg := config.Config{Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example", Auth: config.ProviderAuthTea, TeaLogin: stringPtr("chosen")}}}
			client, err := NewForgejoWebhookClient(context.Background(), cfg, "https://code.example/acme/app", WithTeaRunner(runner), WithLookPath(fakeTeaLookPath))
			if err != nil {
				t.Fatal(err)
			}
			if err := client.DeleteWebhook(context.Background(), 11); err == nil {
				t.Fatal("tea exit zero hid a rejected webhook deletion")
			}
			for _, call := range runner.callsSnapshot() {
				if call.Args[0] == "api" && !strings.Contains(strings.Join(call.Args, " "), "--login chosen -i -X DELETE") {
					t.Fatalf("wrong tea auth/status selection: %v", call.Args)
				}
			}
		})
	}
}

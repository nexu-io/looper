package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
)

func TestForgejoWebhookCommandsRotateAndDeleteByRecordedID(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_rotation_%t", conflict), func(t *testing.T) {
			t.Setenv("CLI_FORGEJO_WEBHOOK_TOKEN", "cli-provider-token")
			ctx := context.Background()
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "looper.sqlite")
			coordinator := openMigratedCLIWebhookCoordinator(t, dbPath)
			defer coordinator.Close()
			repos := storage.NewRepositories(coordinator.DB())
			var mu sync.Mutex
			hooks := map[int64]forge.RepositoryHook{}
			secrets := map[int64]string{11: "original-secret"}
			methods := []string{}
			var record storage.WebhookTunnelHookRecord
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "token cli-provider-token" {
					t.Error("wrong provider auth")
					http.Error(w, "unauthorized", 401)
					return
				}
				methods = append(methods, r.Method+" "+r.URL.Path)
				var id int64
				_, _ = fmt.Sscanf(r.URL.Path, "/forge/api/v1/repos/acme/app/hooks/%d", &id)
				switch r.Method {
				case http.MethodGet:
					hook, found := hooks[id]
					if !found {
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(hook)
				case http.MethodPost:
					var body struct {
						Type   string            `json:"type"`
						Events []string          `json:"events"`
						Config map[string]string `json:"config"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if body.Type != "forgejo" || body.Config["secret"] == "" || body.Config["secret"] == "original-secret" {
						t.Error("replacement must use a new native signing secret")
					}
					hook := hooks[11]
					hook.ID = 12
					hook.Events = body.Events
					hooks[12] = hook
					secrets[12] = body.Config["secret"]
					if conflict {
						rotated := record
						rotated.HookID = 999
						rotated.SecretRef = "other-rotation.key"
						if err := repos.WebhookTunnelHooks.SaveIfCurrentHookID(ctx, rotated, 11); err != nil {
							t.Error(err)
						}
					}
					w.WriteHeader(201)
					_ = json.NewEncoder(w).Encode(hook)
				case http.MethodDelete:
					delete(hooks, id)
					delete(secrets, id)
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected %s; PATCH cannot rotate Forgejo secrets", r.Method)
					http.Error(w, "unexpected method", 500)
				}
			}))
			defer server.Close()
			key := server.URL + "/forge/acme/app"
			record = storage.WebhookTunnelHookRecord{Repo: key, HookID: 11, ManagedURL: "https://hooks.example/webhook/forgejo/project/fj", SecretRef: "original.key", CreatedAt: 1, UpdatedAt: 1}
			hook := forge.RepositoryHook{ID: 11, Type: "forgejo", Active: true, Events: forge.ForgejoWebhookEvents()}
			hook.Config.URL = record.ManagedURL
			hook.Config.ContentType = "json"
			hooks[11] = hook
			if err := repos.WebhookTunnelHooks.Upsert(ctx, record); err != nil {
				t.Fatal(err)
			}
			secretPath := webhookTunnelSecretPathForCLI(dbPath, record.SecretRef)
			if err := os.MkdirAll(filepath.Dir(secretPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secretPath, []byte("original-secret"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{"storage": map[string]any{"dbPath": dbPath}, "notifications": map[string]any{"osascript": map[string]any{"enabled": false}}, "providers": []any{map[string]any{"id": "fj", "kind": "forgejo", "baseUrl": server.URL + "/forge", "tokenEnv": "CLI_FORGEJO_WEBHOOK_TOKEN"}}, "projects": []any{map[string]any{"id": "fj", "name": "Forgejo", "repo": "acme/app", "repoPath": dir, "provider": "fj"}}, "webhook": map[string]any{"mode": "tunnel", "listenPort": 8443, "publicBaseUrl": "https://hooks.example"}}
			configPath := filepath.Join(dir, "config.json")
			raw, _ := json.Marshal(cfg)
			if err := os.WriteFile(configPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			app := New(Deps{Stdout: stdout, Stderr: stderr, LookPath: func(command string) (string, error) {
				if command == "gh" {
					return "", errors.New("gh unavailable")
				}
				return "/usr/bin/" + command, nil
			}, RunCommand: func(context.Context, string, []string, time.Duration) (commandExecutionResult, error) {
				return commandExecutionResult{}, errors.New("Forgejo must not invoke gh")
			}})
			exitCode := app.Run(ctx, []string{"webhook", "rotate", key, "--config", configPath})
			current, _, _ := repos.WebhookTunnelHooks.Get(ctx, key)
			if conflict {
				if exitCode == 0 || current.HookID != 999 || current.SecretRef != "other-rotation.key" {
					t.Fatalf("conflict rotation exit=%d record=%#v stderr=%s", exitCode, current, stderr)
				}
				if _, exists := hooks[12]; exists {
					t.Fatal("losing replacement leaked a remote hook")
				}
				if _, exists := hooks[11]; !exists {
					t.Fatal("losing rotation deleted original remote hook")
				}
				return
			}
			if exitCode != 0 {
				t.Fatalf("rotate exit=%d stderr=%s", exitCode, stderr)
			}
			secret, err := os.ReadFile(webhookTunnelSecretPathForCLI(dbPath, current.SecretRef))
			if err != nil {
				t.Fatal(err)
			}
			if current.HookID != 12 || string(secret) != secrets[12] || secrets[11] != "" {
				t.Fatalf("rotation not published atomically: %#v", current)
			}
			if bytes.Contains(stdout.Bytes(), secret) || bytes.Contains(stderr.Bytes(), secret) {
				t.Fatal("rotation output leaked signing secret")
			}
			if exitCode := app.Run(ctx, []string{"webhook", "delete", key, "--confirm", "--config", configPath}); exitCode != 0 {
				t.Fatalf("delete exit=%d stderr=%s", exitCode, stderr)
			}
			if _, found, _ := repos.WebhookTunnelHooks.Get(ctx, key); found {
				t.Fatal("delete retained managed record")
			}
			if len(hooks) != 0 {
				t.Fatalf("remaining hooks = %#v", hooks)
			}
			if strings.Contains(strings.Join(methods, "\n"), "PATCH") {
				t.Fatal("rotation used ineffective PATCH")
			}
		})
	}
}

func TestForgejoTunnelEnableDoesNotRequireGHWebhookExtension(t *testing.T) {
	cfg := config.Config{Webhook: config.WebhookConfig{Mode: config.WebhookModeGHForward}, Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo}}, Projects: []config.ProjectRefConfig{{ID: "fj", Provider: "fj", Webhook: config.ProjectWebhookConfig{Mode: config.WebhookModeTunnel}}}}
	if webhookConfigNeedsGHForward(cfg) {
		t.Fatal("Forgejo tunnel requires GitHub extension")
	}
}

func TestForgejoWebhookRepositoryURLNormalization(t *testing.T) {
	got, err := normalizeManagedWebhookRepo("HTTPS://Code.Example/MyForge/Acme/App/")
	if err != nil || got != "https://code.example/MyForge/acme/app" {
		t.Fatalf("normalized webhook repository = %q, %v", got, err)
	}
}

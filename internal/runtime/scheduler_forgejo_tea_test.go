package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
)

// TestForgejoTeaAuthSchedulerClaimPath covers:
// tea login selection → config/db-registered Forgejo project → forgejoClientForRepo
// (scheduler provider path) → successful provider operation via tea api.
func TestForgejoTeaAuthSchedulerClaimPath(t *testing.T) {
	teaBin := writeFakeTeaBinary(t, "https://code.example.com", "powerformer-code", map[string]fakeTeaAPIRoute{
		"GET /user": {
			Status: "HTTP/1.1 200 OK",
			Body:   `{"id":42,"login":"mrcfps"}`,
		},
		"GET /repos/core/odcrew/issues?limit=50&page=1&state=open": {
			Status: "HTTP/1.1 200 OK",
			Body:   `[{"number":44,"title":"loop","body":"","state":"open","html_url":"https://code.example.com/core/odcrew/issues/44","updated_at":"2026-07-14T00:00:00Z","user":{"id":1,"login":"mrcfps"},"labels":[{"id":1,"name":"looper:plan"}],"assignees":[{"id":42,"login":"mrcfps"}]}]`,
		},
	})
	t.Setenv("PATH", filepath.Dir(teaBin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	repoPath := filepath.Join(t.TempDir(), "odcrew")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Providers: []config.ProviderConfig{{
			ID: "forgejo-main", Kind: config.ProviderKindForgejo,
			BaseURL:  "https://code.example.com",
			Auth:     config.ProviderAuthTea,
			TeaLogin: stringPtr("powerformer-code"),
			// Intentionally no TokenEnv — tea is the sole authority.
		}},
		Projects: []config.ProjectRefConfig{{
			ID: "odcrew", Name: "odcrew", RepoPath: repoPath,
			Provider: "forgejo-main", Repo: "core/odcrew",
		}},
	}

	// Simulate database-registered project metadata used by catalog/scheduler.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "looper.sqlite")
	coordinator := openMigratedCoordinator(t, dbPath, filepath.Join(t.TempDir(), "backups"))
	defer coordinator.Close()
	repos := storage.NewRepositories(coordinator.DB())
	metadata, _ := json.Marshal(map[string]string{"provider": "forgejo-main", "repo": "core/odcrew", "source": "api"})
	metadataStr := string(metadata)
	now := "2026-07-14T00:00:00Z"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: "odcrew", Name: "odcrew", RepoPath: repoPath, MetadataJSON: &metadataStr,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	// Queue claim path uses the same provider-bound repo resolution.
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "q1", ProjectID: stringPtr("odcrew"), Type: "worker", TargetType: "issue", TargetID: "44",
		Repo: stringPtr("core/odcrew"), DedupeKey: "worker:core/odcrew#44", Priority: 1, Status: "queued",
		AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert queue: %v", err)
	}

	// Scheduler path: resolve client for the registered bare repo.
	client, ok, err := forgejoClientForRepo(&cfg, "core/odcrew")
	if err != nil || !ok || client == nil {
		t.Fatalf("forgejoClientForRepo() = ok=%v err=%v client=%v", ok, err, client)
	}
	identity, err := client.CurrentUser(ctx)
	if err != nil || identity.Login != "mrcfps" {
		t.Fatalf("CurrentUser() = %#v, %v", identity, err)
	}
	issues, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{State: "open"})
	if err != nil || len(issues) != 1 || issues[0].Number != 44 {
		t.Fatalf("ListOpenIssues() = %#v, %v", issues, err)
	}

	// Ensure token-env absence is not required and never falls through to GitHub for this repo.
	if _, ok, err := forgejoClientForRepo(&cfg, "core/odcrew"); err != nil || !ok {
		t.Fatalf("second forgejoClientForRepo() failed: ok=%v err=%v", ok, err)
	}

	// Missing login must surface tea_login_missing, not a GitHub client.
	// forgejoClientForRepo returns ok=true with err when the provider matches but auth fails.
	cfg.Providers[0].TeaLogin = stringPtr("does-not-exist")
	_, matched, err := forgejoClientForRepo(&cfg, "core/odcrew")
	if !matched {
		t.Fatal("expected forgejo provider match so routing does not fall through to GitHub")
	}
	if err == nil {
		t.Fatal("expected tea login missing failure")
	}
	if !strings.Contains(err.Error(), forge.TeaErrorLoginMissing) {
		t.Fatalf("error = %v, want tea_login_missing", err)
	}
}

type fakeTeaAPIRoute struct {
	Status string
	Body   string
}

func writeFakeTeaBinary(t *testing.T, loginURL, defaultLogin string, routes map[string]fakeTeaAPIRoute) string {
	t.Helper()
	dir := t.TempDir()
	routesPath := filepath.Join(dir, "routes.json")
	raw, err := json.Marshal(routes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routesPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "tea")
	content := `#!/bin/sh
set -eu
ROUTES_FILE='` + routesPath + `'
LOGIN_URL='` + loginURL + `'
DEFAULT_LOGIN='` + defaultLogin + `'

if [ "${1:-}" = "logins" ] && [ "${2:-}" = "list" ]; then
  # default must be a JSON boolean (not a string); matches current tea CLI output.
  printf '%s\n' "[{\"name\":\"$DEFAULT_LOGIN\",\"url\":\"$LOGIN_URL\",\"user\":\"mrcfps\",\"default\":true},{\"name\":\"other-default\",\"url\":\"https://other.example.com\",\"user\":\"other\",\"default\":false}]"
  exit 0
fi

if [ "${1:-}" != "api" ]; then
  echo "unexpected tea args: $*" >&2
  exit 2
fi

login=""
method="GET"
endpoint=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    api) shift; continue ;;
    --login|-l) login="$2"; shift 2; continue ;;
    -X|--method) method="$2"; shift 2; continue ;;
    -i|--include) shift; continue ;;
    -d|--data)
      # consume body source; read stdin if @-
      if [ "$2" = "@-" ]; then cat >/dev/null; fi
      shift 2
      continue
      ;;
    -*) shift; continue ;;
    *) endpoint="$1"; shift; break ;;
  esac
done

if [ -z "$login" ]; then
  echo "Error: login required" >&2
  exit 1
fi
if [ "$login" != "$DEFAULT_LOGIN" ]; then
  echo "Error: login name '$login' does not exist" >&2
  exit 1
fi

key="$method $endpoint"
python3 - "$ROUTES_FILE" "$key" <<'PY'
import json, sys
routes = json.load(open(sys.argv[1]))
key = sys.argv[2]
route = routes.get(key)
if not route:
    sys.stderr.write("unexpected tea api %s\n" % key)
    sys.exit(3)
sys.stderr.write(route["Status"] + "\nContent-Type: application/json\n\n")
sys.stdout.write(route["Body"])
PY
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

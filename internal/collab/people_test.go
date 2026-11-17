package collab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/infra/feishucli"
)

func TestOwnerAttributionForPersonalPlaneProject(t *testing.T) {
	h := &bridgeHost{cfg: config.Config{HITL: config.HITLConfig{
		Feishu: &config.HITLFeishuConfig{OwnerOpenID: "ou_local_owner"},
	}}}
	if got := h.ownerOpenID("personal-plane-dynamic-project"); got != "ou_local_owner" {
		t.Fatalf("personal Plane project lost the local Looper owner: %q", got)
	}
	h.cfg.Projects = []config.ProjectRefConfig{{ID: "configured", Owner: &config.FeishuActorConfig{FeishuOpenID: "ou_project_owner"}}}
	if got := h.ownerOpenID("configured"); got != "ou_project_owner" {
		t.Fatalf("configured project owner lost precedence: %q", got)
	}
	h.cfg.HITL.Feishu = nil
	if got := h.ownerOpenID("personal-plane-dynamic-project"); got != "" {
		t.Fatalf("unconfigured owner was invented: %q", got)
	}
}

func TestPeopleDirectoryRefreshJoinsNoIdentitiesAndRemovesOldMembers(t *testing.T) {
	phase := "initial"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workspaces/team/projects/project/members/" || r.Header.Get("X-API-Key") != "test-token" || r.Header.Get("User-Agent") == "" {
			t.Errorf("invalid member request: %s", r.URL)
		}
		if phase == "failed" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		id := "plane_old"
		if phase == "new" {
			id = "plane_new"
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": id, "display_name": "同名", "email": "private@example.test"}})
	}))
	defer server.Close()
	plane, err := forge.NewPlaneClient(forge.RepositoryRef{ProviderID: "plane", Kind: forge.ProviderKindPlane, BaseURL: server.URL + "/api/v1"}, "team", "project", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	h := &bridgeHost{dir: t.TempDir(), sources: []planeSource{{project: config.ProjectRefConfig{Provider: "plane"}, client: plane}}}
	if err := h.writeWorkspace(filepath.Join(h.dir, "workspace")); err != nil {
		t.Fatal(err)
	}
	cliAppID := "cli_bot"
	client := feishucli.Client{Run: func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		if args[0] == "auth" {
			return json.Marshal(map[string]string{"appId": cliAppID})
		}
		var data any
		if args[2] == "/open-apis/contact/v3/scopes" {
			if phase == "failed" {
				return []byte(`{"ok":false,"error":{"message":"contact scope missing"}}`), nil
			}
			data = map[string]any{"user_ids": []string{"ou_current"}}
		} else {
			id := "ou_old"
			if phase == "new" {
				id = "ou_new"
			}
			data = map[string]any{"items": []map[string]string{{"open_id": id, "name": "同名", "email": "private@example.test"}}}
		}
		return json.Marshal(map[string]any{"ok": true, "data": data})
	}}
	read := func() PeopleDirectory {
		t.Helper()
		if err := h.syncPeople(context.Background(), "cli_bot", client); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		h.people(response, httptest.NewRequest("GET", "/people", nil))
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "private@example.test") {
			t.Fatalf("directory failed or exposed an unneeded private email: %s", response.Body)
		}
		var directory PeopleDirectory
		if err := json.Unmarshal(response.Body.Bytes(), &directory); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(h.peoplePath())
		if info.Mode().Perm() != 0600 {
			t.Fatalf("directory permissions: %v", info.Mode())
		}
		return directory
	}
	initial := read()
	if initial.Feishu.Users[0].OpenID != "ou_old" || initial.Plane[0].Members[0].ID != "plane_old" {
		t.Fatalf("missing sources: %+v", initial)
	}
	phase = "new"
	updated := read()
	if len(updated.Feishu.Users) != 1 || updated.Feishu.Users[0].OpenID != "ou_new" || updated.Plane[0].Members[0].ID != "plane_new" {
		t.Fatalf("new member not added or old person survived refresh: %+v", updated)
	}
	phase = "failed"
	failed := read()
	if len(failed.Feishu.Users) != 0 || failed.Feishu.Error == "" || len(failed.Plane[0].Members) != 0 || failed.Plane[0].Error == "" {
		t.Fatalf("old people survived failed refresh as current identities: %+v", failed)
	}
	phase, cliAppID = "new", "cli_other_app"
	other := read()
	if other.AppID != "cli_other_app" || len(other.Feishu.Users) != 0 || !strings.Contains(other.Feishu.Error, "differs") {
		t.Fatalf("different bot's open_ids labeled as this bot's: %+v", other)
	}
}

package collab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/infra/feishucli"
)

// PeopleDirectory is a replaceable source snapshot for the agent, not an
// authorization database or a product/design/QA routing table.
type PeopleDirectory struct {
	AppID     string        `json:"app_id"`
	UpdatedAt time.Time     `json:"updated_at"`
	Feishu    FeishuPeople  `json:"feishu"`
	Plane     []PlanePeople `json:"plane"`
}

type FeishuPeople struct {
	Users []feishucli.DirectoryUser `json:"users"`
	Error string                    `json:"error,omitempty"`
}

type PlanePeople struct {
	Connection string              `json:"connection"`
	Members    []forge.PlaneMember `json:"members"`
	Error      string              `json:"error,omitempty"`
}

func (h *bridgeHost) peoplePath() string {
	return filepath.Join(h.dir, "workspace", "people.json")
}

func (h *bridgeHost) ownerOpenID(project string) string {
	if owner := config.ProjectOwner(h.cfg, project); owner != "" {
		return owner
	}
	if h.cfg.HITL.Feishu != nil {
		return h.cfg.HITL.Feishu.OwnerOpenID
	}
	return ""
}

// Use the existing directory snapshot; rendering a card must not wait on a
// contacts API request or turn routine attribution into an @ notification.
func (h *bridgeHost) ownerName(openID string) string {
	data, err := os.ReadFile(h.peoplePath())
	if err != nil {
		return ""
	}
	var directory PeopleDirectory
	if json.Unmarshal(data, &directory) != nil {
		return ""
	}
	for _, user := range directory.Feishu.Users {
		if user.OpenID == openID {
			return user.Name
		}
	}
	return ""
}

// Each refresh replaces the snapshot. Removed people and revoked visibility
// cannot silently survive as successful mappings from an older refresh.
func (h *bridgeHost) syncPeople(ctx context.Context, appID string, client feishucli.Client) error {
	directory := PeopleDirectory{UpdatedAt: time.Now().UTC(), Plane: []PlanePeople{}, Feishu: FeishuPeople{Users: []feishucli.DirectoryUser{}}}
	actualAppID, err := client.ApplicationID(ctx)
	directory.AppID = actualAppID
	if err == nil && actualAppID != appID {
		err = fmt.Errorf("lark-cli app %s differs from bridge app %s; these open_ids are not interchangeable", actualAppID, appID)
	}
	if err != nil {
		directory.Feishu.Error = err.Error()
	} else {
		users, err := client.DirectoryUsers(ctx)
		if users != nil {
			directory.Feishu.Users = users
		}
		if err != nil {
			directory.Feishu.Error = err.Error()
		}
	}
	for _, source := range h.planeSources() {
		people := PlanePeople{Connection: source.project.Provider, Members: []forge.PlaneMember{}}
		members, err := source.client.ProjectMembers(ctx)
		if err != nil {
			people.Error = err.Error()
		} else if members != nil {
			people.Members = members
		}
		directory.Plane = append(directory.Plane, people)
	}
	data, err := json.MarshalIndent(directory, "", "  ")
	if err != nil {
		return err
	}
	path := h.peoplePath()
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (h *bridgeHost) maintainPeople(ctx context.Context, appID string) {
	client := feishucli.Client{Config: *h.cfg.HITL.Feishu}
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		refresh, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := h.syncPeople(refresh, appID, client)
		cancel()
		if err != nil && ctx.Err() == nil {
			h.warn("people directory refresh failed", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *bridgeHost) people(w http.ResponseWriter, _ *http.Request) {
	data, err := os.ReadFile(h.peoplePath())
	if err != nil {
		http.Error(w, fmt.Sprintf("people directory is not available yet: %v", err), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

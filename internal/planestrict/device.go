package planestrict

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// DeviceConnection is the account's pairing, independent of code repositories
// and the projects it is currently allowed to execute in.
type DeviceConnection struct {
	Pairing        string `json:"pairing,omitempty"`
	RelayURL       string `json:"relayUrl,omitempty"`
	WebBaseURL     string `json:"webBaseUrl,omitempty"`
	BaseURL        string `json:"baseUrl"`
	DeviceID       string `json:"deviceId"`
	MemberID       string `json:"memberId"`
	NodeID         string `json:"nodeId"`
	Workspace      string `json:"workspace"`
	ProjectID      string `json:"projectId"` // Initial link-proof context only.
	PrivateKeyFile string `json:"privateKeyFile"`
}

type DeviceProject struct {
	Workspace string `json:"workspace"`
	ID        string `json:"id"`
	Name      string `json:"name"`
}

type DeviceProjects struct {
	RelayURL string          `json:"relay_url,omitempty"`
	State    string          `json:"state"`
	Projects []DeviceProject `json:"projects"`
}

func LoadDeviceConnection(path string) (DeviceConnection, error) {
	var connection DeviceConnection
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &connection)
	}
	return connection, err
}

func (d DeviceConnection) Save(path string) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func (d DeviceConnection) Client(workspace, project string, options ...Option) (*Client, error) {
	if _, err := parseUUID(project); err != nil {
		return nil, err
	}
	credentials, err := LoadCredentials(d.DeviceID, 1, d.NodeID, d.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	// Sessions are per project, although the device key is shared. In particular,
	// do not reuse one globally unique session ID in two project inboxes.
	credentials.SessionID, credentials.InstanceNonce, err = loadOrCreateSessionIdentity(d.PrivateKeyFile + "." + project + ".session.json")
	if err != nil {
		return nil, err
	}
	return NewClient(d.BaseURL, workspace, project, credentials, options...)
}

func (c *Client) DeviceProjects(ctx context.Context) (DeviceProjects, error) {
	var result DeviceProjects
	err := c.do(ctx, http.MethodGet, "/api/looper/devices/projects/", nil, nil, nil, &result)
	return result, err
}

func (c *Client) ProjectMembers(ctx context.Context) ([]byte, error) {
	var result json.RawMessage
	err := c.do(ctx, http.MethodGet, c.projectPath("looper", "members"), nil, nil, nil, &result)
	return result, err
}

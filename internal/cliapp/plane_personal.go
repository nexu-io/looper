package cliapp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/collab"
	"github.com/nexu-io/looper/internal/config"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/version"
	"github.com/spf13/cobra"
)

func (r *commandRuntime) connectPersonalPlane(cmd *cobra.Command, origin, code string, exchange planeConnectExchange, node networkclient.LocalState) error {
	home, err := r.homeDir()
	if err != nil {
		return err
	}
	keyFile := filepath.Join(home, ".looper", "runtime", "plane-device-"+safeFileComponent(exchange.ConnectionID)+".pem")
	pending := keyFile + ".pending"
	if exchange.Status == "created" || exchange.Status == "cli_connected" {
		// Keep the same key when a prior HTTP response was lost.
		var key ed25519.PrivateKey
		if data, err := r.readFile(pending); err == nil {
			block, _ := pem.Decode(data)
			if block == nil {
				return errors.New("invalid pending personal Plane key")
			}
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return err
			}
			var ok bool
			key, ok = parsed.(ed25519.PrivateKey)
			if !ok {
				return errors.New("pending personal Plane key is not Ed25519")
			}
		} else if !os.IsNotExist(err) {
			return err
		} else {
			if _, err := r.readFile(keyFile); err == nil {
				return errors.New("refusing to replace an existing personal Plane key")
			} else if !os.IsNotExist(err) {
				return err
			}
			_, key, err = ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return err
			}
		}
		encoded, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := r.mkdirAll(filepath.Dir(keyFile), 0700); err != nil {
			return err
		}
		if err := r.writeFile(pending, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0600); err != nil {
			return err
		}
		if exchange.Pairing == "device_v1" {
			name, err := os.Hostname()
			if err != nil {
				return err
			}
			linked, err := planestrict.PairDevice(cmd.Context(), origin, exchange.Workspace, exchange.ProjectID, exchange.ConnectionID, code, name, key, planestrict.WithHTTPClient(r.httpClient()))
			if err != nil {
				return fmt.Errorf("pair personal Looper: %w; retry the same connection command", err)
			}
			exchange.DeviceID, exchange.NodeID = linked.Binding.ID, linked.Binding.NodeID
		} else {
			body, _, err := r.buildPlaneLinkRequestWithKey(cmd.Context(), node, planeLinkProofIdentity{
				WorkspaceID: exchange.WorkspaceID, ProjectID: exchange.ProjectID, MemberID: exchange.MemberID,
			}, key)
			if err != nil {
				return err
			}
			var linked struct {
				Binding planeLinkBinding `json:"binding"`
			}
			if err := r.planeRawRequestWithHeaders(cmd.Context(), origin, "", http.MethodPost,
				"/api/looper/connections/link/", planeLinkMediaType, body,
				map[string]string{"X-Looper-Connect-Code": code, "X-Looper-Node-Name": node.NodeName}, &linked); err != nil {
				return fmt.Errorf("pair personal Looper: %w; retry the same connection command", err)
			}
			exchange.DeviceID, exchange.NodeID = linked.Binding.ID, linked.Binding.NodeID
		}
	}
	if (exchange.Pairing != "device_v1" && exchange.NodeID != node.NodeID) || exchange.DeviceID == "" || exchange.NodeID == "" {
		return fmt.Errorf("the Plane device pairing does not belong to this local Looper")
	}
	if _, err := os.Stat(pending); err == nil {
		if err := renameFile(pending, keyFile); err != nil {
			return err
		}
	}
	connection := planestrict.DeviceConnection{BaseURL: origin, WebBaseURL: exchange.WebBaseURL, DeviceID: exchange.DeviceID,
		Pairing:  exchange.Pairing,
		MemberID: exchange.MemberID, NodeID: exchange.NodeID, Workspace: exchange.Workspace,
		ProjectID: exchange.ProjectID, PrivateKeyFile: keyFile}
	client, err := connection.Client(connection.Workspace, connection.ProjectID, planestrict.WithHTTPClient(r.httpClient()))
	if err != nil {
		return err
	}
	if err := connection.Save(filepath.Join(home, ".looper", "plane-device.json")); err != nil {
		return err
	}
	if exchange.Status != "completed" {
		projects, err := client.DeviceProjects(cmd.Context())
		if err != nil {
			return err
		}
		if len(projects.Projects) == 0 {
			return fmt.Errorf("the account no longer has any executable projects")
		}
		first := projects.Projects[0]
		inbox, err := connection.Client(first.Workspace, first.ID, planestrict.WithHTTPClient(r.httpClient()))
		if err != nil {
			return err
		}
		if _, err := inbox.Inbox(cmd.Context(), ""); err != nil {
			return fmt.Errorf("verify personal Looper signed inbox: %w", err)
		}
		body, _ := json.Marshal(map[string]string{"code": code})
		if err := r.planeRawRequest(cmd.Context(), origin, "", http.MethodPost, "/api/looper/connections/complete/", "application/json", body, nil); err != nil {
			return err
		}
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"connected": true, "deviceId": connection.DeviceID, "node": connection.NodeID, "next": "looper collab start"})
	}
	printSection(cmd.OutOrStdout(), "Personal Looper connected", [][2]any{{"deviceId", connection.DeviceID}, {"next", "Run looper collab start. An unconnected running host will discover this device automatically."}})
	return nil
}

// A Plane pairing code already identifies the account. The server supplies only
// a single-use invitation; the network's enrollment credential stays server-side.
func (r *commandRuntime) joinPlaneNetwork(cmd *cobra.Command, enrollment planeEnrollment, home string) (networkclient.LocalState, error) {
	var empty networkclient.LocalState
	if enrollment.BaseURL == "" || enrollment.NetworkID == "" || enrollment.JoinKey == "" {
		return empty, errors.New("Plane returned incomplete network enrollment")
	}
	identity, err := r.currentGitHubIdentity(cmd.Context())
	if err != nil {
		return empty, err
	}
	name, err := os.Hostname()
	if err != nil {
		return empty, err
	}
	name = safeFileComponent(name)
	name = strings.Trim(name, "-.")
	if name == "" {
		name = "looper"
	}
	if len(name) > 32 {
		name = name[:32]
	}
	api := networkclient.New(enrollment.BaseURL, "", r.httpClient())
	joined, err := api.Join(cmd.Context(), protocol.JoinRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: version.Value, JoinKey: enrollment.JoinKey, NodeName: name, GitHub: identity})
	if err != nil {
		return empty, fmt.Errorf("join the Looper network supplied by Plane: %w", err)
	}
	if joined.NetworkID != enrollment.NetworkID {
		return empty, errors.New("Plane enrollment returned the wrong Looper network")
	}
	state := networkclient.LocalState{URL: enrollment.BaseURL, NetworkID: joined.NetworkID, NodeID: joined.NodeID, NodeName: name, NodeToken: joined.NodeToken, GitHub: identity}
	if err := networkclient.SaveState(networkclient.DefaultStatePath(home), state); err != nil {
		return empty, err
	}
	return state, nil
}

func (r *commandRuntime) personalPlaneDoctor(ctx context.Context, cfg config.Config, device planestrict.DeviceConnection, stateDir string, add func(string, string, string, string)) {
	r.planeDoctorToolChecks(ctx, cfg, add, true)
	client, err := device.Client(device.Workspace, device.ProjectID, planestrict.WithHTTPClient(r.httpClient()))
	if err != nil {
		add("personal-device", "failed", err.Error(), "Reconnect from My Looper in personal Plane settings.")
		return
	}
	apiCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	projects, err := client.DeviceProjects(apiCtx)
	cancel()
	if err != nil {
		add("signed-project-directory", "failed", err.Error(), "Check Plane connectivity and your personal device connection.")
	} else if projects.State != "active" {
		add("personal-device", "failed", "device is disconnected", "Reconnect from My Looper in personal Plane settings.")
	} else {
		add("signed-project-directory", "passed", fmt.Sprintf("authenticated personal device · %d permitted projects", len(projects.Projects)), "")
	}
	raw, err := collab.Request(ctx, collab.StateDir(cfg, stateDir), "GET", "/status", nil)
	if err != nil {
		add("collab", "failed", err.Error(), "Run looper collab start with the same config and state directory.")
		return
	}
	var status struct {
		Network networkclient.Status `json:"network"`
		Plane   collab.PlaneStatus   `json:"plane"`
	}
	if err = json.Unmarshal(raw, &status); err != nil {
		add("collab", "failed", err.Error(), "Restart the collaboration host.")
		return
	}
	if device.Pairing == "device_v1" {
		if status.Plane.DeviceID != device.DeviceID || !status.Plane.Reachable || !status.Plane.HeartbeatReachable {
			add("collab", "failed", "Plane connection or device heartbeat is not ready", "Check looper collab status --json and the Plane connection.")
		} else {
			add("collab", "passed", "Plane connected · device heartbeat online", "")
		}
		return
	}
	if !status.Network.Configured || !status.Network.CloudReachable || status.Network.NodeID != device.NodeID || status.Network.IdentityDrift {
		add("collab", "failed", "the host is not connected as this personal device", "Check network access and GitHub login, then restart looper collab start.")
		return
	}
	add("collab", "passed", "independent host is online as "+device.NodeID, "")
}

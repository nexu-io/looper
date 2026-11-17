package planestrict

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
)

type PairedDevice struct {
	Binding struct {
		ID     string `json:"id"`
		NodeID string `json:"node_id"`
	} `json:"binding"`
}

// The one-time Plane code authorizes this key, whose signature proves possession.
// No network invitation, GitHub identity or second device identity is involved.
func PairDevice(ctx context.Context, origin, workspace, project, connectionID, code, name string, key ed25519.PrivateKey, options ...Option) (PairedDevice, error) {
	var result PairedDevice
	id, err := parseUUID(connectionID)
	if err != nil {
		return result, err
	}
	client, err := NewClient(origin, workspace, project, Credentials{BindingID: id, KeyRevision: 1, NodeID: "pairing", PrivateKey: key}, options...)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]string{"public_key": base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), "name": name})
	if err != nil {
		return result, err
	}
	request, err := client.signedRequest(ctx, http.MethodPost, "/api/looper/devices/pair/", nil, body, nil)
	if err != nil {
		return result, err
	}
	request.Header.Set("X-Looper-Connect-Code", code)
	err = client.send(request, &result)
	return result, err
}

// DeviceHeartbeat reports presence directly to the paired Plane origin using
// the same device signature as task requests. CF only relays Feishu messages.
func (c *Client) DeviceHeartbeat(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/looper/devices/heartbeat/", nil, map[string]any{}, nil, nil)
}

package planestrict

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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

// CF forwards this exact signed request to its configured Plane origin for
// authentication. A client cannot nominate a different verification server.
func (c *Client) DeviceHeartbeat(ctx context.Context, relayURL string) error {
	relay, err := url.Parse(strings.TrimRight(relayURL, "/"))
	if err != nil || relay.Host == "" || relay.User != nil || relay.RawQuery != "" || relay.Fragment != "" || (relay.Scheme != "https" && !(relay.Scheme == "http" && (relay.Hostname() == "127.0.0.1" || relay.Hostname() == "localhost"))) {
		return fmt.Errorf("collaboration relay requires HTTPS (HTTP is allowed on localhost)")
	}
	request, err := c.signedRequest(ctx, http.MethodPost, "/api/looper/devices/heartbeat/", nil, []byte(`{}`), nil)
	if err != nil {
		return err
	}
	relay.Path += "/devices/heartbeat"
	request.URL, request.Host = relay, relay.Host
	return c.send(request, nil)
}

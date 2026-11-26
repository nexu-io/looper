package planestrict

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersonalDeviceReusesKeyButKeepsSessionsPerProject(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "device.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: data}), 0600); err != nil {
		t.Fatal(err)
	}
	d := DeviceConnection{BaseURL: "https://plane.example", DeviceID: "55555555-6666-4777-8888-999999999999", NodeID: "node-personal", PrivateKeyFile: path}
	first := "33333333-4444-4555-8666-777777777777"
	second := "44444444-5555-4666-8777-888888888888"
	a, err := d.Client("team", first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Client("team", second)
	if err != nil {
		t.Fatal(err)
	}
	if a.credentials.BindingID != b.credentials.BindingID {
		t.Fatal("pairing key changed across projects")
	}
	if a.credentials.SessionID == b.credentials.SessionID {
		t.Fatal("two projects shared a globally unique inbox session")
	}
	resumed, err := d.Client("team", first)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.credentials.SessionID != a.credentials.SessionID || resumed.credentials.InstanceNonce != a.credentials.InstanceNonce {
		t.Fatal("restart changed the same project's session")
	}
	if _, err = d.Client("team", "../../escape"); err == nil {
		t.Fatal("remote project escaped the session directory")
	}
}

func TestDeviceHeartbeatUsesSignedPlaneEndpoint(t *testing.T) {
	publicKey, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binding := mustUUID(t, "55555555-6666-4777-8888-999999999999")
	now := time.Now().UTC()
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/looper/devices/heartbeat/" {
					t.Errorf("unexpected heartbeat: %s %s", r.Method, r.URL.Path)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				verifyRequestSignature(t, r, raw, publicKey, binding, nil, now)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "team", "project", Credentials{
				BindingID: binding, KeyRevision: 2, PrivateKey: key, NodeID: "collab-session",
			}, WithClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			err = client.DeviceHeartbeat(context.Background())
			if (err == nil) != (status == http.StatusOK) {
				t.Fatalf("status=%d error=%v", status, err)
			}
		})
	}
}

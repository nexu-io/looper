package cloud

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/planeprotocol"
)

func TestLinkChallengeEndpointReturnsAuthenticatedSignedChallenge(t *testing.T) {
	ctx := context.Background()
	privateKey, keyFile := writeTestTrustKey(t)
	cfg := Config{
		DBPath: filepath.Join(t.TempDir(), "net.sqlite"), AdminToken: "admin-token",
		ProtocolVersion: protocol.CurrentVersion, MinimumDaemonVersion: "1.2.0", NetworkID: "net-fixed",
		TrustPrivateKeyFile: keyFile, TrustKeyRevision: 7, LinkChallengeTTL: 90,
	}
	service, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	fixedNow := time.Date(2026, time.July, 21, 10, 0, 0, 123_000_000, time.UTC)
	service.now = func() time.Time { return fixedNow }
	joinKey, err := service.CreateJoinKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	node, err := service.Join(ctx, protocol.JoinRequest{
		ProtocolVersion: protocol.CurrentVersion, DaemonVersion: "1.2.3", JoinKey: joinKey,
		NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(cfg, service).Handler())
	defer server.Close()

	nodePublic := privateKey.Public().(ed25519.PublicKey)
	publicHash := sha256.Sum256(nodePublic)
	requestBody, _ := json.Marshal(protocol.LinkChallengeRequest{
		PublicKeySHA256: base64.RawURLEncoding.EncodeToString(publicHash[:]),
		Audience:        "plane:22222222-3333-4444-8555-666666666666",
	})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/link-challenges", bytes.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+node.NodeToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var body protocol.LinkChallengeResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.KeyRevision != 7 || body.ExpiresAtMS != fixedNow.Add(90*time.Second).UnixMilli() {
		t.Fatalf("response = %#v", body)
	}
	envelope, err := base64.RawURLEncoding.Strict().DecodeString(body.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	revision := uint64(7)
	payload, err := planeprotocol.VerifyEnvelope(privateKey.Public().(ed25519.PublicKey), planeprotocol.LinkChallengeProfile, &revision, envelope)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := planeprotocol.DecodeLinkChallenge(payload)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.NetworkID != "net-fixed" || challenge.NodeID != node.NodeID || challenge.Audience != "plane:22222222-3333-4444-8555-666666666666" {
		t.Fatalf("challenge = %#v", challenge)
	}
	if challenge.PublicKeySHA256 != publicHash || challenge.IssuedAtMS != fixedNow.UnixMilli() || challenge.ExpiresAtMS != body.ExpiresAtMS {
		t.Fatalf("challenge identity/time mismatch: %#v", challenge)
	}
}

func TestLinkChallengeEndpointFailsClosed(t *testing.T) {
	server, service := newTestHTTPServer(t)
	defer server.Close()
	defer service.Close()
	node := joinNode(t, server.URL, createJoinKey(t, server.URL), "worker-1", 101)
	validBody, _ := json.Marshal(protocol.LinkChallengeRequest{
		PublicKeySHA256: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		Audience:        "plane:22222222-3333-4444-8555-666666666666",
	})

	tests := []struct {
		name, token string
		body        []byte
		wantStatus  int
	}{
		{name: "missing auth", body: validBody, wantStatus: http.StatusUnauthorized},
		{name: "unknown node", token: "not-a-node", body: validBody, wantStatus: http.StatusUnauthorized},
		{name: "signer disabled", token: node.NodeToken, body: validBody, wantStatus: http.StatusServiceUnavailable},
		{name: "unknown field", token: node.NodeToken, body: []byte(`{"publicKeySha256":"x","audience":"plane:x","extra":true}`), wantStatus: http.StatusBadRequest},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/link-challenges", bytes.NewReader(item.body))
			if item.token != "" {
				request.Header.Set("Authorization", "Bearer "+item.token)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != item.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, item.wantStatus)
			}
		})
	}
}

func TestLoadConfigRequiresRevisionForTrustKey(t *testing.T) {
	_, err := LoadConfigFromEnv(map[string]string{
		"LOOPERNET_DB_PATH":                filepath.Join(t.TempDir(), "net.sqlite"),
		"LOOPERNET_ADMIN_TOKEN":            "admin",
		"LOOPERNET_TRUST_PRIVATE_KEY_FILE": "/secret/key.pem",
	}, "test")
	if err == nil {
		t.Fatal("missing trust key revision was accepted")
	}
}

func writeTestTrustKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "trust-key.pem")
	raw := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return privateKey, path
}

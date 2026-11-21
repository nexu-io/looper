package cliapp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/planeprotocol"
)

func TestPlaneConnectSelfActivatesAndWaitsForSignedInbox(t *testing.T) {
	homeDir := t.TempDir()
	workspaceIDText := "22222222-3333-4444-8555-666666666666"
	workspaceID := mustCLIProtocolUUID(t, workspaceIDText)
	projectID := "33333333-4444-4555-8666-777777777777"
	projectUUID := mustCLIProtocolUUID(t, projectID)
	memberID := "44444444-5555-4666-8777-888888888888"
	memberUUID := mustCLIProtocolUUID(t, memberID)
	challengeID := mustCLIProtocolUUID(t, "11111111-2222-4333-8444-555555555555")
	bindingID := "55555555-6666-4777-8888-999999999999"
	connectionID := "66666666-7777-4888-9999-aaaaaaaaaaaa"
	connectCode := "one-time-code"
	_, trustPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	completeCalls := 0
	linkCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/looper/connections/exchange/":
			if request.Header.Get("X-API-Key") != "" {
				t.Fatal("public exchange must not send the Plane API key")
			}
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["code"] != connectCode {
				t.Fatalf("exchange body=%#v err=%v", body, err)
			}
			writePlaneCommandTestJSON(t, response, map[string]any{
				"connection_id": connectionID, "status": "cli_connected", "workspace": "open-design",
				"workspace_id": workspaceIDText, "project_id": projectID, "member_id": memberID,
			})
		case "/v1/link-challenges":
			if request.Header.Get("Authorization") != "Bearer node-token" {
				t.Fatal("missing loopernet Node token")
			}
			var input protocol.LinkChallengeRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			decodedHash, err := base64.RawURLEncoding.DecodeString(input.PublicKeySHA256)
			if err != nil || len(decodedHash) != 32 {
				t.Fatalf("public key hash=%q err=%v", input.PublicKeySHA256, err)
			}
			var publicHash [32]byte
			copy(publicHash[:], decodedHash)
			var nonce [16]byte
			copy(nonce[:], []byte("0123456789abcdef"))
			payload, err := planeprotocol.EncodeLinkChallenge(planeprotocol.LinkChallenge{
				NetworkID: "network-1", NodeID: "node-owner", PublicKeySHA256: publicHash,
				Audience: "plane:" + workspaceIDText, ChallengeID: challengeID, Nonce: nonce,
				IssuedAtMS: now.UnixMilli(), ExpiresAtMS: now.Add(time.Minute).UnixMilli(),
			})
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := planeprotocol.SignEnvelope(trustPrivate, planeprotocol.LinkChallengeProfile, 7, payload)
			if err != nil {
				t.Fatal(err)
			}
			writePlaneCommandTestJSON(t, response, protocol.LinkChallengeResponse{
				Challenge: base64.RawURLEncoding.EncodeToString(envelope), ExpiresAtMS: now.Add(time.Minute).UnixMilli(), KeyRevision: 7,
			})
		case "/api/looper/connections/link/":
			linkCalls++
			if request.Header.Get("X-Looper-Connect-Code") != connectCode || request.Header.Get("X-Looper-Node-Name") != "Owner Mac" {
				t.Fatalf("connection headers code=%q node=%q", request.Header.Get("X-Looper-Connect-Code"), request.Header.Get("X-Looper-Node-Name"))
			}
			if request.Header.Get("Content-Type") != planeLinkMediaType {
				t.Fatalf("link content type=%q", request.Header.Get("Content-Type"))
			}
			raw, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			challengeEnvelope, proofEnvelope, err := planeprotocol.DecodeLinkRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			challenge, err := planeprotocol.DecodeEnvelope(challengeEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := planeprotocol.DecodeEnvelope(proofEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			proofValue, err := planeprotocol.DecodeLinkProof(proof.Payload)
			if err != nil {
				t.Fatal(err)
			}
			challengeDigest, _ := planeprotocol.DomainDigest(planeprotocol.LinkChallengeProfile, challenge.Payload)
			if proofValue.ChallengeSHA256 != challengeDigest || proofValue.PlaneWorkspace != workspaceID || proofValue.PlaneProject != projectUUID || proofValue.MemberID != memberUUID {
				t.Fatalf("link proof identity mismatch: %#v", proofValue)
			}
			if !ed25519.Verify(proofValue.PublicKey[:], mustDomainDigest(t, planeprotocol.LinkProofProfile, proof.Payload), proof.Signature[:]) {
				t.Fatal("link proof signature did not verify")
			}
			writePlaneCommandTestJSON(t, response, map[string]any{
				"connection_id": connectionID,
				"binding":       map[string]any{"id": bindingID, "member_id": memberID, "node_id": "node-owner", "state": "active", "revision": 1},
			})
		case "/api/looper/connections/complete/":
			completeCalls++
			if completeCalls == 1 {
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(http.StatusConflict)
				_, _ = response.Write([]byte(`{"error":"node_not_ready","detail":"waiting for signed inbox"}`))
				return
			}
			writePlaneCommandTestJSON(t, response, map[string]any{"connection_id": connectionID, "status": "completed"})
		case "/api/v1/status":
			writePlaneCommandTestJSON(t, response, map[string]any{
				"ok": true, "data": map[string]any{"service": map[string]any{"binary": map[string]any{"name": "looperd"}}},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	serverPort := server.Listener.Addr().(*net.TCPAddr).Port

	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"server": map[string]any{"host": "127.0.0.1", "port": serverPort, "authMode": "none"},
		"providers": []any{map[string]any{
			"id": "plane-main", "kind": "plane", "baseUrl": server.URL + "/api/v1",
			"tokenEnv": "PLANE_TOKEN", "workspace": "open-design", "projectId": projectID,
		}},
		"notifications": map[string]any{"osascript": map[string]any{"enabled": false}},
		"defaults":      map[string]any{"allowRiskyFixes": false, "fixAllPullRequests": false},
	})
	if err := networkclient.SaveState(networkclient.DefaultStatePath(homeDir), networkclient.LocalState{
		URL: server.URL, NetworkID: "network-1", NodeID: "node-owner", NodeName: "Owner Mac", NodeToken: "node-token",
	}); err != nil {
		t.Fatal(err)
	}
	app := New(Deps{
		HomeDir: homeDir, HTTPClient: server.Client(), LookPath: configLookPathForTests(),
		Sleep: func(time.Duration) {},
	})
	exitCode, stdout, stderr := runAppWithDeps(t, app, []string{
		"plane", "connect", server.URL, "--code", connectCode, "--config", configPath,
	})
	if exitCode != 0 {
		t.Fatalf("plane connect exit=%d stdout=%s stderr=%s", exitCode, stdout, stderr)
	}
	if linkCalls != 1 || completeCalls != 2 {
		t.Fatalf("linkCalls=%d completeCalls=%d", linkCalls, completeCalls)
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{ConfigPath: configPath, LookPath: configLookPathForTests()})
	if err != nil {
		t.Fatal(err)
	}
	strict := loaded.Config.Providers[0].StrictDispatch
	if strict == nil || !strict.Enabled || strict.BindingID != bindingID || strict.NodeID != "node-owner" {
		t.Fatalf("strict config=%#v", strict)
	}
	if info, err := os.Stat(strict.PrivateKeyFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private key info=%#v err=%v", info, err)
	}
	if _, err := os.Stat(strict.PrivateKeyFile + ".pending"); !os.IsNotExist(err) {
		t.Fatalf("pending private key was not cleaned up: %v", err)
	}
}

func mustCLIProtocolUUID(t *testing.T, value string) planeprotocol.UUID {
	t.Helper()
	parsed, err := parsePlaneProtocolUUID(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func mustDomainDigest(t *testing.T, profile string, payload []byte) []byte {
	t.Helper()
	digest, err := planeprotocol.DomainDigest(profile, payload)
	if err != nil {
		t.Fatal(err)
	}
	return digest[:]
}

func writePlaneCommandTestJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func TestPlaneConnectFailedLinkLeavesNewProjectUnconfigured(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "trial.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/looper/connections/exchange/" {
			writePlaneCommandTestJSON(t, w, map[string]string{"status": "cli_connected", "workspace": "team", "workspace_id": "22222222-3333-4444-8555-666666666666", "project_id": "33333333-4444-4555-8666-777777777777", "member_id": "44444444-5555-4666-8777-888888888888"})
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if err := networkclient.SaveState(networkclient.DefaultStatePath(home), networkclient.LocalState{URL: server.URL, NodeID: "mac", NodeToken: "node-token"}); err != nil {
		t.Fatal(err)
	}
	app := New(Deps{HomeDir: home, HTTPClient: server.Client(), LookPath: configLookPathForTests()})
	exit, _, stderr := runAppWithDeps(t, app, []string{"plane", "connect", server.URL, "--config", configPath, "--code", "connect-code", "--project-path", home, "--code-repo", "team/repo"})
	if exit == 0 || !strings.Contains(stderr, "challenge") {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("published config before binding: %v", err)
	}
}

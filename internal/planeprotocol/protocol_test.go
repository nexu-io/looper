package planeprotocol

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

type goldenFixture struct {
	FixtureVersion uint64          `json:"fixture_version"`
	Trust          goldenKey       `json:"trust"`
	Node           goldenKey       `json:"node"`
	Challenge      goldenChallenge `json:"challenge"`
	Proof          goldenProof     `json:"proof"`
	LinkRequest    string          `json:"link_request_cbor_b64"`
	NodeRequests   []goldenRequest `json:"node_requests"`
}

type goldenKey struct {
	PrivateSeed string `json:"private_seed_b64"`
	PublicKey   string `json:"public_key_b64"`
	KeyRevision uint64 `json:"key_revision"`
}

type goldenChallenge struct {
	NetworkID          string `json:"network_id"`
	NodeID             string `json:"node_id"`
	PublicKeySHA256    string `json:"public_key_sha256_b64"`
	Audience           string `json:"audience"`
	ChallengeID        string `json:"challenge_id"`
	Nonce              string `json:"nonce_b64"`
	IssuedAtMS         int64  `json:"issued_at_ms"`
	ExpiresAtMS        int64  `json:"expires_at_ms"`
	Payload            string `json:"payload_cbor_b64"`
	Digest             string `json:"digest_b64"`
	Signature          string `json:"signature_b64"`
	Ed25519phSignature string `json:"ed25519ph_signature_b64"`
	Envelope           string `json:"envelope_cbor_b64"`
	EnvelopeBase64URL  string `json:"envelope_base64url"`
}

type goldenProof struct {
	WorkspaceID string `json:"plane_workspace_id"`
	ProjectID   string `json:"plane_project_id"`
	MemberID    string `json:"member_id"`
	Payload     string `json:"payload_cbor_b64"`
	Digest      string `json:"digest_b64"`
	Signature   string `json:"signature_b64"`
	Envelope    string `json:"envelope_cbor_b64"`
}

type goldenRequest struct {
	Name               string  `json:"name"`
	Method             string  `json:"method"`
	RawPath            string  `json:"raw_path"`
	CanonicalPath      string  `json:"canonical_path"`
	RawQuery           string  `json:"raw_query"`
	CanonicalQuery     string  `json:"canonical_query"`
	RawBody            string  `json:"raw_body_b64"`
	BindingID          string  `json:"binding_id"`
	KeyRevision        uint64  `json:"key_revision"`
	DispatchID         *string `json:"dispatch_id"`
	DispatchRevision   *uint64 `json:"dispatch_revision"`
	StateVersion       *uint64 `json:"state_version"`
	ExecutionAttemptID *string `json:"execution_attempt_id"`
	FencingToken       *uint64 `json:"fencing_token"`
	TimestampMS        int64   `json:"timestamp_ms"`
	Nonce              string  `json:"nonce_b64"`
	BodySHA256         string  `json:"body_sha256_b64"`
	Payload            string  `json:"payload_cbor_b64"`
	Digest             string  `json:"digest_b64"`
	Signature          string  `json:"signature_b64"`
	Ed25519phSignature string  `json:"ed25519ph_signature_b64"`
	SignatureHeader    string  `json:"signature_header"`
}

func TestLinkChallengeMatchesGoldenVector(t *testing.T) {
	fixture := loadGoldenFixture(t)
	item := fixture.Challenge
	privateKey := ed25519.NewKeyFromSeed(decodeB64(t, fixture.Trust.PrivateSeed))
	publicKey := ed25519.PublicKey(decodeB64(t, fixture.Trust.PublicKey))
	var publicHash [32]byte
	copy(publicHash[:], decodeB64(t, item.PublicKeySHA256))
	value := LinkChallenge{
		NetworkID: item.NetworkID, NodeID: item.NodeID, PublicKeySHA256: publicHash,
		Audience: item.Audience, ChallengeID: mustParseUUID(t, item.ChallengeID),
		Nonce: must16(t, item.Nonce), IssuedAtMS: item.IssuedAtMS, ExpiresAtMS: item.ExpiresAtMS,
	}
	payload, err := EncodeLinkChallenge(value)
	assertNoError(t, err)
	assertBytes(t, payload, decodeB64(t, item.Payload))
	digest, err := DomainDigest(LinkChallengeProfile, payload)
	assertNoError(t, err)
	assertBytes(t, digest[:], decodeB64(t, item.Digest))
	assertBytes(t, ed25519.Sign(privateKey, digest[:]), decodeB64(t, item.Signature))
	envelope, err := SignEnvelope(privateKey, LinkChallengeProfile, fixture.Trust.KeyRevision, payload)
	assertNoError(t, err)
	assertBytes(t, envelope, decodeB64(t, item.Envelope))
	verified, err := VerifyEnvelope(publicKey, LinkChallengeProfile, uint64Ptr(fixture.Trust.KeyRevision), envelope)
	assertNoError(t, err)
	assertBytes(t, verified, payload)
	decoded, err := DecodeLinkChallenge(payload)
	assertNoError(t, err)
	if decoded.NodeID != item.NodeID {
		t.Fatalf("NodeID = %q, want %q", decoded.NodeID, item.NodeID)
	}
}

func TestLinkProofAndRequestMatchGoldenVector(t *testing.T) {
	fixture := loadGoldenFixture(t)
	nodePrivate := ed25519.NewKeyFromSeed(decodeB64(t, fixture.Node.PrivateSeed))
	nodePublic := ed25519.PublicKey(decodeB64(t, fixture.Node.PublicKey))
	var challengeDigest, publicKey [32]byte
	copy(challengeDigest[:], decodeB64(t, fixture.Challenge.Digest))
	copy(publicKey[:], nodePublic)
	payload, err := EncodeLinkProof(LinkProof{
		ChallengeSHA256: challengeDigest,
		PlaneWorkspace:  mustParseUUID(t, fixture.Proof.WorkspaceID),
		PlaneProject:    mustParseUUID(t, fixture.Proof.ProjectID),
		MemberID:        mustParseUUID(t, fixture.Proof.MemberID), PublicKey: publicKey,
	})
	assertNoError(t, err)
	assertBytes(t, payload, decodeB64(t, fixture.Proof.Payload))
	digest, err := DomainDigest(LinkProofProfile, payload)
	assertNoError(t, err)
	assertBytes(t, digest[:], decodeB64(t, fixture.Proof.Digest))
	proofEnvelope, err := SignEnvelope(nodePrivate, LinkProofProfile, 0, payload)
	assertNoError(t, err)
	assertBytes(t, proofEnvelope, decodeB64(t, fixture.Proof.Envelope))
	verified, err := VerifyEnvelope(nodePublic, LinkProofProfile, uint64Ptr(0), proofEnvelope)
	assertNoError(t, err)
	assertBytes(t, verified, payload)

	challengeEnvelope := decodeB64(t, fixture.Challenge.Envelope)
	request, err := EncodeLinkRequest(challengeEnvelope, proofEnvelope)
	assertNoError(t, err)
	assertBytes(t, request, decodeB64(t, fixture.LinkRequest))
	decodedChallenge, decodedProof, err := DecodeLinkRequest(request)
	assertNoError(t, err)
	assertBytes(t, decodedChallenge, challengeEnvelope)
	assertBytes(t, decodedProof, proofEnvelope)
}

func TestNodeRequestsMatchGoldenVectors(t *testing.T) {
	fixture := loadGoldenFixture(t)
	nodePrivate := ed25519.NewKeyFromSeed(decodeB64(t, fixture.Node.PrivateSeed))
	nodePublic := ed25519.PublicKey(decodeB64(t, fixture.Node.PublicKey))
	for _, item := range fixture.NodeRequests {
		t.Run(item.Name, func(t *testing.T) {
			body := decodeB64(t, item.RawBody)
			bodyHash := sha256.Sum256(body)
			value := NodeRequest{
				Method: item.Method, Path: item.RawPath, Query: item.RawQuery, BodySHA256: bodyHash,
				BindingID: mustParseUUID(t, item.BindingID), KeyRevision: item.KeyRevision,
				DispatchRevision: item.DispatchRevision, StateVersion: item.StateVersion,
				FencingToken: item.FencingToken, TimestampMS: item.TimestampMS, Nonce: must16(t, item.Nonce),
			}
			if item.DispatchID != nil {
				parsed := mustParseUUID(t, *item.DispatchID)
				value.DispatchID = &parsed
			}
			if item.ExecutionAttemptID != nil {
				parsed := mustParseUUID(t, *item.ExecutionAttemptID)
				value.ExecutionAttemptID = &parsed
			}
			payload, err := EncodeNodeRequest(value)
			assertNoError(t, err)
			assertBytes(t, payload, decodeB64(t, item.Payload))
			assertBytes(t, bodyHash[:], decodeB64(t, item.BodySHA256))
			digest, err := DomainDigest(NodeRequestProfile, payload)
			assertNoError(t, err)
			assertBytes(t, digest[:], decodeB64(t, item.Digest))
			signature := ed25519.Sign(nodePrivate, digest[:])
			assertBytes(t, signature, decodeB64(t, item.Signature))
			if !ed25519.Verify(nodePublic, digest[:], signature) {
				t.Fatal("golden signature did not verify")
			}
			decoded, err := DecodeNodeRequest(payload)
			assertNoError(t, err)
			if decoded.Path != item.CanonicalPath || decoded.Query != item.CanonicalQuery {
				t.Fatalf("canonical URL = %q?%q", decoded.Path, decoded.Query)
			}
			header, err := ParseSignatureHeader(item.SignatureHeader)
			assertNoError(t, err)
			if header.BindingID != value.BindingID || header.TimestampMS != value.TimestampMS || header.Nonce != value.Nonce {
				t.Fatalf("parsed signature header = %#v", header)
			}
		})
	}
}

func TestProtocolRejectsTamperWrongRevisionEd25519phAndAmbiguity(t *testing.T) {
	fixture := loadGoldenFixture(t)
	item := fixture.Challenge
	publicKey := ed25519.PublicKey(decodeB64(t, fixture.Trust.PublicKey))
	if _, err := VerifyEnvelope(publicKey, LinkChallengeProfile, uint64Ptr(8), decodeB64(t, item.Envelope)); err == nil {
		t.Fatal("wrong key revision was accepted")
	}
	payload := decodeB64(t, item.Payload)
	payload[len(payload)-1] ^= 1
	var signature [64]byte
	copy(signature[:], decodeB64(t, item.Signature))
	tampered, err := EncodeEnvelope(Envelope{KeyRevision: 7, Algorithm: Algorithm, Payload: payload, Signature: signature})
	assertNoError(t, err)
	if _, err := VerifyEnvelope(publicKey, LinkChallengeProfile, nil, tampered); err == nil {
		t.Fatal("tampered payload was accepted")
	}
	copy(signature[:], decodeB64(t, item.Ed25519phSignature))
	phEnvelope, err := EncodeEnvelope(Envelope{KeyRevision: 7, Algorithm: Algorithm, Payload: decodeB64(t, item.Payload), Signature: signature})
	assertNoError(t, err)
	if _, err := VerifyEnvelope(publicKey, LinkChallengeProfile, nil, phEnvelope); err == nil {
		t.Fatal("Ed25519ph signature was accepted as pure Ed25519")
	}
	if _, err := CanonicalizePath("/safe%2Fescape"); err == nil {
		t.Fatal("encoded slash was accepted")
	}
	if _, err := CanonicalizeQuery("cursor"); err == nil {
		t.Fatal("query without equals was accepted")
	}
	if _, err := ParseSignatureHeader("v=1; v=1; key=55555555-6666-4777-8888-999999999999:2; ts=1; nonce=x; sig=x"); err == nil {
		t.Fatal("duplicate header field was accepted")
	}
}

func loadGoldenFixture(t *testing.T) goldenFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/strict_protocol_v1.json")
	assertNoError(t, err)
	var value goldenFixture
	assertNoError(t, json.Unmarshal(raw, &value))
	if value.FixtureVersion != 1 {
		t.Fatalf("fixture version = %d", value.FixtureVersion)
	}
	return value
}

func decodeB64(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	assertNoError(t, err)
	return decoded
}

func mustParseUUID(t *testing.T, value string) UUID {
	t.Helper()
	parsed, err := parseUUID(value)
	assertNoError(t, err)
	return parsed
}

func must16(t *testing.T, value string) [16]byte {
	t.Helper()
	var result [16]byte
	decoded := decodeB64(t, value)
	if len(decoded) != len(result) {
		t.Fatalf("decoded length = %d", len(decoded))
	}
	copy(result[:], decoded)
	return result
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if string(got) != string(want) {
		t.Fatalf("bytes differ\n got: %x\nwant: %x", got, want)
	}
}

func uint64Ptr(value uint64) *uint64 { return &value }

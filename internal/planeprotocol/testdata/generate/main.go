package main

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/nexu-io/looper/internal/planeprotocol"
)

type keyVector struct {
	PrivateSeedB64 string `json:"private_seed_b64"`
	PublicKeyB64   string `json:"public_key_b64"`
	KeyRevision    uint64 `json:"key_revision"`
}

type challengeVector struct {
	NetworkID             string `json:"network_id"`
	NodeID                string `json:"node_id"`
	PublicKeySHA256B64    string `json:"public_key_sha256_b64"`
	Audience              string `json:"audience"`
	ChallengeID           string `json:"challenge_id"`
	NonceB64              string `json:"nonce_b64"`
	IssuedAtMS            int64  `json:"issued_at_ms"`
	ExpiresAtMS           int64  `json:"expires_at_ms"`
	PayloadCBORB64        string `json:"payload_cbor_b64"`
	DigestB64             string `json:"digest_b64"`
	SignatureB64          string `json:"signature_b64"`
	Ed25519phSignatureB64 string `json:"ed25519ph_signature_b64"`
	EnvelopeCBORB64       string `json:"envelope_cbor_b64"`
	EnvelopeBase64URL     string `json:"envelope_base64url"`
}

type proofVector struct {
	PlaneWorkspaceID string `json:"plane_workspace_id"`
	PlaneProjectID   string `json:"plane_project_id"`
	MemberID         string `json:"member_id"`
	PayloadCBORB64   string `json:"payload_cbor_b64"`
	DigestB64        string `json:"digest_b64"`
	SignatureB64     string `json:"signature_b64"`
	EnvelopeCBORB64  string `json:"envelope_cbor_b64"`
}

type requestVector struct {
	Name                  string  `json:"name"`
	Method                string  `json:"method"`
	RawPath               string  `json:"raw_path"`
	CanonicalPath         string  `json:"canonical_path"`
	RawQuery              string  `json:"raw_query"`
	CanonicalQuery        string  `json:"canonical_query"`
	RawBodyB64            string  `json:"raw_body_b64"`
	BindingID             string  `json:"binding_id"`
	KeyRevision           uint64  `json:"key_revision"`
	DispatchID            *string `json:"dispatch_id"`
	DispatchRevision      *uint64 `json:"dispatch_revision"`
	StateVersion          *uint64 `json:"state_version"`
	ExecutionAttemptID    *string `json:"execution_attempt_id"`
	FencingToken          *uint64 `json:"fencing_token"`
	TimestampMS           int64   `json:"timestamp_ms"`
	NonceB64              string  `json:"nonce_b64"`
	BodySHA256B64         string  `json:"body_sha256_b64"`
	PayloadCBORB64        string  `json:"payload_cbor_b64"`
	DigestB64             string  `json:"digest_b64"`
	SignatureB64          string  `json:"signature_b64"`
	Ed25519phSignatureB64 string  `json:"ed25519ph_signature_b64"`
	SignatureHeader       string  `json:"signature_header"`
}

type fixture struct {
	FixtureVersion  uint64            `json:"fixture_version"`
	Trust           keyVector         `json:"trust"`
	Node            keyVector         `json:"node"`
	Challenge       challengeVector   `json:"challenge"`
	Proof           proofVector       `json:"proof"`
	LinkRequestCBOR string            `json:"link_request_cbor_b64"`
	NodeRequests    []requestVector   `json:"node_requests"`
	NegativeCases   map[string]string `json:"negative_cases"`
}

func main() {
	looperOut := flag.String("looper-out", "internal/planeprotocol/testdata/strict_protocol_v1.json", "Looper fixture path")
	planeOut := flag.String("plane-out", "", "optional Plane fixture path")
	flag.Parse()

	value, err := buildFixture()
	if err != nil {
		panic(err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	raw = append(raw, '\n')
	for _, path := range []string{*looperOut, *planeOut} {
		if path == "" {
			continue
		}
		if err := os.MkdirAll(directory(path), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			panic(err)
		}
	}
}

func buildFixture() (fixture, error) {
	trustSeed := sequentialBytes(0x00, ed25519.SeedSize)
	nodeSeed := sequentialBytes(0x20, ed25519.SeedSize)
	trustPrivate := ed25519.NewKeyFromSeed(trustSeed)
	nodePrivate := ed25519.NewKeyFromSeed(nodeSeed)
	trustPublic := trustPrivate.Public().(ed25519.PublicKey)
	nodePublic := nodePrivate.Public().(ed25519.PublicKey)
	nodePublicHash := sha256.Sum256(nodePublic)

	challengeIDText := "11111111-2222-4333-8444-555555555555"
	challenge := planeprotocol.LinkChallenge{
		NetworkID:       "net_opaque-东京",
		NodeID:          "node_cyan-01",
		PublicKeySHA256: nodePublicHash,
		Audience:        "plane:22222222-3333-4444-8555-666666666666",
		ChallengeID:     mustUUID(challengeIDText),
		Nonce:           fixed16(0xa0),
		IssuedAtMS:      1784563200123,
		ExpiresAtMS:     1784563320123,
	}
	challengePayload, err := planeprotocol.EncodeLinkChallenge(challenge)
	if err != nil {
		return fixture{}, err
	}
	challengeDigest := mustDigest(planeprotocol.LinkChallengeProfile, challengePayload)
	challengeSignature := ed25519.Sign(trustPrivate, challengeDigest[:])
	challengeEnvelope, err := planeprotocol.SignEnvelope(trustPrivate, planeprotocol.LinkChallengeProfile, 7, challengePayload)
	if err != nil {
		return fixture{}, err
	}

	workspaceText := "22222222-3333-4444-8555-666666666666"
	projectText := "33333333-4444-4555-8666-777777777777"
	memberText := "44444444-5555-4666-8777-888888888888"
	var nodePublicArray [32]byte
	copy(nodePublicArray[:], nodePublic)
	proof := planeprotocol.LinkProof{
		ChallengeSHA256: challengeDigest,
		PlaneWorkspace:  mustUUID(workspaceText),
		PlaneProject:    mustUUID(projectText),
		MemberID:        mustUUID(memberText),
		PublicKey:       nodePublicArray,
	}
	proofPayload, err := planeprotocol.EncodeLinkProof(proof)
	if err != nil {
		return fixture{}, err
	}
	proofDigest := mustDigest(planeprotocol.LinkProofProfile, proofPayload)
	proofSignature := ed25519.Sign(nodePrivate, proofDigest[:])
	proofEnvelope, err := planeprotocol.SignEnvelope(nodePrivate, planeprotocol.LinkProofProfile, 0, proofPayload)
	if err != nil {
		return fixture{}, err
	}
	linkRequest, err := planeprotocol.EncodeLinkRequest(challengeEnvelope, proofEnvelope)
	if err != nil {
		return fixture{}, err
	}

	bindingText := "55555555-6666-4777-8888-999999999999"
	dispatchText := "66666666-7777-4888-8999-aaaaaaaaaaaa"
	attemptText := "77777777-8888-4999-8aaa-bbbbbbbbbbbb"
	requests := []struct {
		name, method, path, query string
		body                      []byte
		dispatch                  *string
		dispatchRevision          *uint64
		stateVersion              *uint64
		attempt                   *string
		fencingToken              *uint64
		timestamp                 int64
		nonce                     [16]byte
	}{
		{
			name: "get_unicode_repeated_query", method: "GET",
			path:      "/api/./v1/inbox/%E8%8A%82%E7%82%B9",
			query:     "tag=b&node_id=node_%2Fcyan&tag=a&plus=a+b&empty=&cursor=%E4%BD%A0%E5%A5%BD",
			timestamp: 1784563201123, nonce: fixed16(0x00),
		},
		{
			name: "post_json_with_dispatch", method: "POST",
			path:     "/api/v1/dispatch/66666666-7777-4888-8999-aaaaaaaaaaaa/transition",
			body:     []byte(`{"message":"你好","empty":null}`),
			dispatch: &dispatchText, dispatchRevision: uint64Ptr(3), stateVersion: uint64Ptr(9),
			attempt: &attemptText, fencingToken: uint64Ptr(41),
			timestamp: 1784563202123, nonce: fixed16(0x10),
		},
	}
	requestVectors := make([]requestVector, 0, len(requests))
	for _, item := range requests {
		bodyHash := sha256.Sum256(item.body)
		value := planeprotocol.NodeRequest{
			Method: item.method, Path: item.path, Query: item.query, BodySHA256: bodyHash,
			BindingID: mustUUID(bindingText), KeyRevision: 2,
			DispatchRevision: item.dispatchRevision, StateVersion: item.stateVersion,
			FencingToken: item.fencingToken, TimestampMS: item.timestamp, Nonce: item.nonce,
		}
		if item.dispatch != nil {
			parsed := mustUUID(*item.dispatch)
			value.DispatchID = &parsed
		}
		if item.attempt != nil {
			parsed := mustUUID(*item.attempt)
			value.ExecutionAttemptID = &parsed
		}
		payload, err := planeprotocol.EncodeNodeRequest(value)
		if err != nil {
			return fixture{}, err
		}
		digest := mustDigest(planeprotocol.NodeRequestProfile, payload)
		signature := ed25519.Sign(nodePrivate, digest[:])
		var signatureArray [64]byte
		copy(signatureArray[:], signature)
		header := planeprotocol.FormatSignatureHeader(planeprotocol.SignatureHeader{
			BindingID: mustUUID(bindingText), KeyRevision: 2, TimestampMS: item.timestamp,
			Nonce: item.nonce, Signature: signatureArray,
		})
		canonicalPath, _ := planeprotocol.CanonicalizePath(item.path)
		canonicalQuery, _ := planeprotocol.CanonicalizeQuery(item.query)
		requestVectors = append(requestVectors, requestVector{
			Name: item.name, Method: item.method, RawPath: item.path, CanonicalPath: canonicalPath,
			RawQuery: item.query, CanonicalQuery: canonicalQuery, RawBodyB64: b64(item.body),
			BindingID: bindingText, KeyRevision: 2, DispatchID: item.dispatch,
			DispatchRevision: item.dispatchRevision, StateVersion: item.stateVersion,
			ExecutionAttemptID: item.attempt, FencingToken: item.fencingToken,
			TimestampMS: item.timestamp, NonceB64: b64(item.nonce[:]), BodySHA256B64: b64(bodyHash[:]),
			PayloadCBORB64: b64(payload), DigestB64: b64(digest[:]), SignatureB64: b64(signature),
			Ed25519phSignatureB64: b64(signPH(nodePrivate, digest[:])), SignatureHeader: header,
		})
	}

	return fixture{
		FixtureVersion: 1,
		Trust:          keyVector{PrivateSeedB64: b64(trustSeed), PublicKeyB64: b64(trustPublic), KeyRevision: 7},
		Node:           keyVector{PrivateSeedB64: b64(nodeSeed), PublicKeyB64: b64(nodePublic), KeyRevision: 2},
		Challenge: challengeVector{
			NetworkID: challenge.NetworkID, NodeID: challenge.NodeID,
			PublicKeySHA256B64: b64(challenge.PublicKeySHA256[:]), Audience: challenge.Audience,
			ChallengeID: challengeIDText, NonceB64: b64(challenge.Nonce[:]),
			IssuedAtMS: challenge.IssuedAtMS, ExpiresAtMS: challenge.ExpiresAtMS,
			PayloadCBORB64: b64(challengePayload), DigestB64: b64(challengeDigest[:]),
			SignatureB64: b64(challengeSignature), Ed25519phSignatureB64: b64(signPH(trustPrivate, challengeDigest[:])),
			EnvelopeCBORB64: b64(challengeEnvelope), EnvelopeBase64URL: base64.RawURLEncoding.EncodeToString(challengeEnvelope),
		},
		Proof: proofVector{
			PlaneWorkspaceID: workspaceText, PlaneProjectID: projectText, MemberID: memberText,
			PayloadCBORB64: b64(proofPayload), DigestB64: b64(proofDigest[:]),
			SignatureB64: b64(proofSignature), EnvelopeCBORB64: b64(proofEnvelope),
		},
		LinkRequestCBOR: b64(linkRequest),
		NodeRequests:    requestVectors,
		NegativeCases: map[string]string{
			"tamper":              "flip the final bit of payload or signature",
			"wrong_key_revision":  "verify challenge envelope with revision 8",
			"ed25519ph":           "verify the supplied Ed25519ph signature as pure Ed25519",
			"encoded_slash":       "canonicalize /safe%2Fescape",
			"query_without_equal": "canonicalize cursor",
		},
	}, nil
}

func signPH(privateKey ed25519.PrivateKey, message []byte) []byte {
	digest := sha512.Sum512(message)
	signature, err := privateKey.Sign(nil, digest[:], &ed25519.Options{Hash: crypto.SHA512})
	if err != nil {
		panic(err)
	}
	return signature
}

func mustDigest(profile string, payload []byte) [32]byte {
	digest, err := planeprotocol.DomainDigest(profile, payload)
	if err != nil {
		panic(err)
	}
	return digest
}

func mustUUID(value string) planeprotocol.UUID {
	var result planeprotocol.UUID
	raw, err := hex.DecodeString(removeHyphens(value))
	if err != nil || len(raw) != len(result) {
		panic(fmt.Sprintf("invalid UUID %q", value))
	}
	copy(result[:], raw)
	return result
}

func removeHyphens(value string) string {
	result := make([]byte, 0, len(value))
	for _, char := range []byte(value) {
		if char != '-' {
			result = append(result, char)
		}
	}
	return string(result)
}

func sequentialBytes(start byte, size int) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = start + byte(index)
	}
	return value
}

func fixed16(start byte) [16]byte {
	var value [16]byte
	copy(value[:], sequentialBytes(start, len(value)))
	return value
}

func uint64Ptr(value uint64) *uint64 { return &value }
func b64(value []byte) string        { return base64.StdEncoding.EncodeToString(value) }

func directory(path string) string {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '/' {
			return path[:index]
		}
	}
	return "."
}

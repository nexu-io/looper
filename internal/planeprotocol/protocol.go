package planeprotocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
)

const (
	LinkChallengeProfile = "LOOPER-LINK-CHALLENGE-V1"
	LinkProofProfile     = "LOOPER-LINK-PROOF-V1"
	NodeRequestProfile   = "LOOPER-NODE-REQUEST-V1"
	Algorithm            = "Ed25519"
	maxPayloadBytes      = 64 * 1024
)

var (
	encodeMode cbor.EncMode
	decodeMode cbor.DecMode
)

func init() {
	var err error
	encodeMode, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	decodeMode, err = (cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		UTF8:              cbor.UTF8RejectInvalid,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
	}).DecMode()
	if err != nil {
		panic(err)
	}
}

type UUID [16]byte

type LinkChallenge struct {
	NetworkID       string
	NodeID          string
	PublicKeySHA256 [32]byte
	Audience        string
	ChallengeID     UUID
	Nonce           [16]byte
	IssuedAtMS      int64
	ExpiresAtMS     int64
}

type linkChallengeWire struct {
	Version         uint64   `cbor:"1,keyasint"`
	NetworkID       string   `cbor:"2,keyasint"`
	NodeID          string   `cbor:"3,keyasint"`
	PublicKeySHA256 [32]byte `cbor:"4,keyasint"`
	Audience        string   `cbor:"5,keyasint"`
	ChallengeID     UUID     `cbor:"6,keyasint"`
	Nonce           [16]byte `cbor:"7,keyasint"`
	IssuedAtMS      int64    `cbor:"8,keyasint"`
	ExpiresAtMS     int64    `cbor:"9,keyasint"`
}

type LinkProof struct {
	ChallengeSHA256 [32]byte
	PlaneWorkspace  UUID
	PlaneProject    UUID
	MemberID        UUID
	PublicKey       [32]byte
}

type linkProofWire struct {
	Version          uint64   `cbor:"1,keyasint"`
	ChallengeSHA256  [32]byte `cbor:"2,keyasint"`
	PlaneWorkspaceID UUID     `cbor:"3,keyasint"`
	PlaneProjectID   UUID     `cbor:"4,keyasint"`
	MemberID         UUID     `cbor:"5,keyasint"`
	PublicKey        [32]byte `cbor:"6,keyasint"`
	Algorithm        string   `cbor:"7,keyasint"`
}

type NodeRequest struct {
	Method             string
	Path               string
	Query              string
	BodySHA256         [32]byte
	BindingID          UUID
	KeyRevision        uint64
	DispatchID         *UUID
	DispatchRevision   *uint64
	StateVersion       *uint64
	ExecutionAttemptID *UUID
	FencingToken       *uint64
	TimestampMS        int64
	Nonce              [16]byte
}

type nodeRequestWire struct {
	Version            uint64   `cbor:"1,keyasint"`
	Method             string   `cbor:"2,keyasint"`
	Path               string   `cbor:"3,keyasint"`
	Query              string   `cbor:"4,keyasint"`
	BodySHA256         [32]byte `cbor:"5,keyasint"`
	BindingID          UUID     `cbor:"6,keyasint"`
	KeyRevision        uint64   `cbor:"7,keyasint"`
	DispatchID         *UUID    `cbor:"8,keyasint"`
	DispatchRevision   *uint64  `cbor:"9,keyasint"`
	StateVersion       *uint64  `cbor:"10,keyasint"`
	ExecutionAttemptID *UUID    `cbor:"11,keyasint"`
	FencingToken       *uint64  `cbor:"12,keyasint"`
	TimestampMS        int64    `cbor:"13,keyasint"`
	Nonce              [16]byte `cbor:"14,keyasint"`
}

type Envelope struct {
	KeyRevision uint64
	Algorithm   string
	Payload     []byte
	Signature   [64]byte
}

type envelopeWire struct {
	KeyRevision uint64   `cbor:"1,keyasint"`
	Algorithm   string   `cbor:"2,keyasint"`
	Payload     []byte   `cbor:"3,keyasint"`
	Signature   [64]byte `cbor:"4,keyasint"`
}

type linkRequestWire struct {
	Challenge envelopeWire `cbor:"1,keyasint"`
	Proof     envelopeWire `cbor:"2,keyasint"`
}

func EncodeLinkChallenge(value LinkChallenge) ([]byte, error) {
	if err := validateOpaqueID(value.NetworkID, "network_id"); err != nil {
		return nil, err
	}
	if err := validateOpaqueID(value.NodeID, "node_id"); err != nil {
		return nil, err
	}
	if err := validateOpaqueID(value.Audience, "audience"); err != nil {
		return nil, err
	}
	if value.ExpiresAtMS <= value.IssuedAtMS {
		return nil, errors.New("challenge expiry must be after issuance")
	}
	return encodeMode.Marshal(linkChallengeWire{
		Version: 1, NetworkID: value.NetworkID, NodeID: value.NodeID,
		PublicKeySHA256: value.PublicKeySHA256, Audience: value.Audience,
		ChallengeID: value.ChallengeID, Nonce: value.Nonce,
		IssuedAtMS: value.IssuedAtMS, ExpiresAtMS: value.ExpiresAtMS,
	})
}

func DecodeLinkChallenge(payload []byte) (LinkChallenge, error) {
	var wire linkChallengeWire
	if err := decodeExact(payload, 9, &wire); err != nil {
		return LinkChallenge{}, fmt.Errorf("decode link challenge: %w", err)
	}
	value := LinkChallenge{
		NetworkID: wire.NetworkID, NodeID: wire.NodeID, PublicKeySHA256: wire.PublicKeySHA256,
		Audience: wire.Audience, ChallengeID: wire.ChallengeID, Nonce: wire.Nonce,
		IssuedAtMS: wire.IssuedAtMS, ExpiresAtMS: wire.ExpiresAtMS,
	}
	if wire.Version != 1 {
		return LinkChallenge{}, errors.New("unsupported link challenge version")
	}
	if _, err := EncodeLinkChallenge(value); err != nil {
		return LinkChallenge{}, err
	}
	return value, nil
}

func EncodeLinkProof(value LinkProof) ([]byte, error) {
	return encodeMode.Marshal(linkProofWire{
		Version: 1, ChallengeSHA256: value.ChallengeSHA256,
		PlaneWorkspaceID: value.PlaneWorkspace, PlaneProjectID: value.PlaneProject,
		MemberID: value.MemberID, PublicKey: value.PublicKey, Algorithm: Algorithm,
	})
}

func DecodeLinkProof(payload []byte) (LinkProof, error) {
	var wire linkProofWire
	if err := decodeExact(payload, 7, &wire); err != nil {
		return LinkProof{}, fmt.Errorf("decode link proof: %w", err)
	}
	if wire.Version != 1 {
		return LinkProof{}, errors.New("unsupported link proof version")
	}
	if wire.Algorithm != Algorithm {
		return LinkProof{}, errors.New("unsupported link proof algorithm")
	}
	return LinkProof{
		ChallengeSHA256: wire.ChallengeSHA256, PlaneWorkspace: wire.PlaneWorkspaceID,
		PlaneProject: wire.PlaneProjectID, MemberID: wire.MemberID, PublicKey: wire.PublicKey,
	}, nil
}

func EncodeLinkRequest(challengeEnvelope, proofEnvelope []byte) ([]byte, error) {
	challenge, err := DecodeEnvelope(challengeEnvelope)
	if err != nil {
		return nil, fmt.Errorf("challenge envelope: %w", err)
	}
	proof, err := DecodeEnvelope(proofEnvelope)
	if err != nil {
		return nil, fmt.Errorf("proof envelope: %w", err)
	}
	if proof.KeyRevision != 0 {
		return nil, errors.New("initial link proof key revision must be zero")
	}
	return encodeMode.Marshal(linkRequestWire{Challenge: envelopeToWire(challenge), Proof: envelopeToWire(proof)})
}

func DecodeLinkRequest(payload []byte) ([]byte, []byte, error) {
	var wire linkRequestWire
	if err := decodeExact(payload, 2, &wire); err != nil {
		return nil, nil, fmt.Errorf("decode link request: %w", err)
	}
	challenge, err := EncodeEnvelope(wireToEnvelope(wire.Challenge))
	if err != nil {
		return nil, nil, fmt.Errorf("challenge envelope: %w", err)
	}
	proofValue := wireToEnvelope(wire.Proof)
	if proofValue.KeyRevision != 0 {
		return nil, nil, errors.New("initial link proof key revision must be zero")
	}
	proof, err := EncodeEnvelope(proofValue)
	if err != nil {
		return nil, nil, fmt.Errorf("proof envelope: %w", err)
	}
	return challenge, proof, nil
}

func EncodeNodeRequest(value NodeRequest) ([]byte, error) {
	if !validMethod(value.Method) {
		return nil, errors.New("method must be uppercase ASCII letters")
	}
	path, err := CanonicalizePath(value.Path)
	if err != nil {
		return nil, err
	}
	query, err := CanonicalizeQuery(value.Query)
	if err != nil {
		return nil, err
	}
	if (value.DispatchID == nil) != (value.DispatchRevision == nil) {
		return nil, errors.New("dispatch id and revision must both be null or present")
	}
	if (value.ExecutionAttemptID == nil) != (value.FencingToken == nil) {
		return nil, errors.New("attempt id and fencing token must both be null or present")
	}
	return encodeMode.Marshal(nodeRequestWire{
		Version: 1, Method: value.Method, Path: path, Query: query, BodySHA256: value.BodySHA256,
		BindingID: value.BindingID, KeyRevision: value.KeyRevision,
		DispatchID: value.DispatchID, DispatchRevision: value.DispatchRevision,
		StateVersion: value.StateVersion, ExecutionAttemptID: value.ExecutionAttemptID,
		FencingToken: value.FencingToken, TimestampMS: value.TimestampMS, Nonce: value.Nonce,
	})
}

func DecodeNodeRequest(payload []byte) (NodeRequest, error) {
	var wire nodeRequestWire
	if err := decodeExact(payload, 14, &wire); err != nil {
		return NodeRequest{}, fmt.Errorf("decode node request: %w", err)
	}
	if wire.Version != 1 {
		return NodeRequest{}, errors.New("unsupported node request version")
	}
	value := NodeRequest{
		Method: wire.Method, Path: wire.Path, Query: wire.Query, BodySHA256: wire.BodySHA256,
		BindingID: wire.BindingID, KeyRevision: wire.KeyRevision,
		DispatchID: wire.DispatchID, DispatchRevision: wire.DispatchRevision,
		StateVersion: wire.StateVersion, ExecutionAttemptID: wire.ExecutionAttemptID,
		FencingToken: wire.FencingToken, TimestampMS: wire.TimestampMS, Nonce: wire.Nonce,
	}
	encoded, err := EncodeNodeRequest(value)
	if err != nil {
		return NodeRequest{}, err
	}
	if !bytes.Equal(encoded, payload) {
		return NodeRequest{}, errors.New("node request fields are not canonical")
	}
	return value, nil
}

func EncodeEnvelope(value Envelope) ([]byte, error) {
	if value.Algorithm != Algorithm {
		return nil, errors.New("unsupported signature algorithm")
	}
	if len(value.Payload) == 0 {
		return nil, errors.New("envelope payload must not be empty")
	}
	return encodeMode.Marshal(envelopeWire{
		KeyRevision: value.KeyRevision, Algorithm: value.Algorithm,
		Payload: value.Payload, Signature: value.Signature,
	})
}

func DecodeEnvelope(payload []byte) (Envelope, error) {
	var wire envelopeWire
	if err := decodeExact(payload, 4, &wire); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	value := Envelope{
		KeyRevision: wire.KeyRevision, Algorithm: wire.Algorithm,
		Payload: wire.Payload, Signature: wire.Signature,
	}
	if _, err := EncodeEnvelope(value); err != nil {
		return Envelope{}, err
	}
	return value, nil
}

func DomainDigest(profile string, payload []byte) ([32]byte, error) {
	switch profile {
	case LinkChallengeProfile, LinkProofProfile, NodeRequestProfile:
	default:
		return [32]byte{}, errors.New("unknown signature profile")
	}
	input := make([]byte, 0, len(profile)+1+len(payload))
	input = append(input, []byte(profile)...)
	input = append(input, 0)
	input = append(input, payload...)
	return sha256.Sum256(input), nil
}

func SignEnvelope(privateKey ed25519.PrivateKey, profile string, keyRevision uint64, payload []byte) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	digest, err := DomainDigest(profile, payload)
	if err != nil {
		return nil, err
	}
	signed := ed25519.Sign(privateKey, digest[:])
	var signature [64]byte
	copy(signature[:], signed)
	return EncodeEnvelope(Envelope{KeyRevision: keyRevision, Algorithm: Algorithm, Payload: payload, Signature: signature})
}

func VerifyEnvelope(publicKey ed25519.PublicKey, profile string, expectedKeyRevision *uint64, encoded []byte) ([]byte, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	envelope, err := DecodeEnvelope(encoded)
	if err != nil {
		return nil, err
	}
	if expectedKeyRevision != nil && envelope.KeyRevision != *expectedKeyRevision {
		return nil, errors.New("unexpected key revision")
	}
	digest, err := DomainDigest(profile, envelope.Payload)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(publicKey, digest[:], envelope.Signature[:]) {
		return nil, errors.New("invalid Ed25519 signature")
	}
	return envelope.Payload, nil
}

func envelopeToWire(value Envelope) envelopeWire {
	return envelopeWire{
		KeyRevision: value.KeyRevision,
		Algorithm:   value.Algorithm,
		Payload:     value.Payload,
		Signature:   value.Signature,
	}
}

func wireToEnvelope(value envelopeWire) Envelope {
	return Envelope{
		KeyRevision: value.KeyRevision,
		Algorithm:   value.Algorithm,
		Payload:     value.Payload,
		Signature:   value.Signature,
	}
}

func decodeExact(payload []byte, expectedFields int, target any) error {
	if len(payload) == 0 || len(payload) > maxPayloadBytes {
		return errors.New("CBOR payload size is invalid")
	}
	var fields map[uint64]cbor.RawMessage
	if err := decodeMode.Unmarshal(payload, &fields); err != nil {
		return fmt.Errorf("invalid CBOR payload: %w", err)
	}
	if len(fields) != expectedFields {
		return errors.New("CBOR fields are incomplete or unknown")
	}
	for key := 1; key <= expectedFields; key++ {
		if _, ok := fields[uint64(key)]; !ok {
			return errors.New("CBOR fields are incomplete or unknown")
		}
	}
	if err := decodeMode.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("invalid CBOR value: %w", err)
	}
	canonical, err := encodeMode.Marshal(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, payload) {
		return errors.New("CBOR payload is not deterministic")
	}
	return nil
}

func validateOpaqueID(value, name string) error {
	if value == "" || len([]byte(value)) > 128 || !utf8.ValidString(value) {
		return fmt.Errorf("%s must contain 1..128 UTF-8 bytes", name)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	return nil
}

func validMethod(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range []byte(value) {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

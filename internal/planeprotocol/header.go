package planeprotocol

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type SignatureHeader struct {
	BindingID   UUID
	KeyRevision uint64
	TimestampMS int64
	Nonce       [16]byte
	Signature   [64]byte
}

func FormatSignatureHeader(value SignatureHeader) string {
	return fmt.Sprintf(
		"v=1; key=%s:%d; ts=%d; nonce=%s; sig=%s",
		formatUUID(value.BindingID),
		value.KeyRevision,
		value.TimestampMS,
		base64.RawURLEncoding.EncodeToString(value.Nonce[:]),
		base64.RawURLEncoding.EncodeToString(value.Signature[:]),
	)
}

func ParseSignatureHeader(value string) (SignatureHeader, error) {
	if strings.TrimSpace(value) == "" {
		return SignatureHeader{}, errors.New("Looper-Signature is required")
	}
	fields := make(map[string]string, 5)
	for _, part := range strings.Split(value, ";") {
		item := strings.TrimSpace(part)
		if strings.Count(item, "=") != 1 {
			return SignatureHeader{}, errors.New("invalid Looper-Signature field")
		}
		pieces := strings.SplitN(item, "=", 2)
		key, raw := pieces[0], pieces[1]
		switch key {
		case "v", "key", "ts", "nonce", "sig":
		default:
			return SignatureHeader{}, errors.New("unknown Looper-Signature field")
		}
		if raw == "" {
			return SignatureHeader{}, errors.New("empty Looper-Signature field")
		}
		if _, exists := fields[key]; exists {
			return SignatureHeader{}, errors.New("duplicate Looper-Signature field")
		}
		fields[key] = raw
	}
	if len(fields) != 5 || fields["v"] != "1" {
		return SignatureHeader{}, errors.New("incomplete or unsupported Looper-Signature")
	}
	if strings.Count(fields["key"], ":") != 1 {
		return SignatureHeader{}, errors.New("invalid Looper-Signature key")
	}
	keyParts := strings.SplitN(fields["key"], ":", 2)
	bindingID, err := parseUUID(keyParts[0])
	if err != nil {
		return SignatureHeader{}, err
	}
	keyRevision, err := strconv.ParseUint(keyParts[1], 10, 64)
	if err != nil {
		return SignatureHeader{}, errors.New("invalid Looper-Signature key revision")
	}
	timestampMS, err := strconv.ParseInt(fields["ts"], 10, 64)
	if err != nil {
		return SignatureHeader{}, errors.New("invalid Looper-Signature timestamp")
	}
	nonce, err := decodeRawBase64URL(fields["nonce"], 16)
	if err != nil {
		return SignatureHeader{}, fmt.Errorf("invalid Looper-Signature nonce: %w", err)
	}
	signature, err := decodeRawBase64URL(fields["sig"], 64)
	if err != nil {
		return SignatureHeader{}, fmt.Errorf("invalid Looper-Signature signature: %w", err)
	}
	result := SignatureHeader{BindingID: bindingID, KeyRevision: keyRevision, TimestampMS: timestampMS}
	copy(result.Nonce[:], nonce)
	copy(result.Signature[:], signature)
	return result, nil
}

func formatUUID(value UUID) string {
	raw := hex.EncodeToString(value[:])
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]
}

func parseUUID(value string) (UUID, error) {
	var result UUID
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return result, errors.New("invalid UUID")
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(raw) != len(result) {
		return result, errors.New("invalid UUID")
	}
	copy(result[:], raw)
	return result, nil
}

func decodeRawBase64URL(value string, size int) ([]byte, error) {
	if strings.Contains(value, "=") {
		return nil, errors.New("base64url must be unpadded")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, err
	}
	if len(decoded) != size {
		return nil, fmt.Errorf("decoded size is %d, want %d", len(decoded), size)
	}
	return decoded, nil
}

package planestrict

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/nexu-io/looper/internal/planeprotocol"
)

func LoadCredentials(bindingID string, keyRevision uint64, nodeID, privateKeyFile string) (Credentials, error) {
	parsedBindingID, err := parseUUID(bindingID)
	if err != nil {
		return Credentials{}, fmt.Errorf("Plane strict credentials binding ID: %w", err)
	}
	if keyRevision == 0 || nodeID == "" || privateKeyFile == "" {
		return Credentials{}, errors.New("Plane strict credentials are incomplete")
	}
	info, err := os.Stat(privateKeyFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("stat Plane strict private key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Credentials{}, errors.New("Plane strict private key must not be readable by group or others")
	}
	encoded, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("read Plane strict private key: %w", err)
	}
	block, rest := pem.Decode(encoded)
	if block == nil || len(rest) != 0 || block.Type != "PRIVATE KEY" {
		return Credentials{}, errors.New("Plane strict private key must be one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return Credentials{}, fmt.Errorf("parse Plane strict private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return Credentials{}, errors.New("Plane strict private key is not Ed25519")
	}
	sessionID, instanceNonce, err := loadOrCreateSessionIdentity(privateKeyFile + ".session.json")
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		BindingID: parsedBindingID, KeyRevision: keyRevision,
		PrivateKey: privateKey, NodeID: nodeID,
		SessionID: sessionID, InstanceNonce: instanceNonce,
	}, nil
}

func loadOrCreateSessionIdentity(path string) (planeprotocol.UUID, [16]byte, error) {
	type sessionFile struct {
		SessionID     string `json:"sessionId"`
		InstanceNonce string `json:"instanceNonce"`
	}
	read := func() (planeprotocol.UUID, [16]byte, error) {
		var nonce [16]byte
		info, err := os.Stat(path)
		if err != nil {
			return planeprotocol.UUID{}, nonce, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return planeprotocol.UUID{}, nonce, errors.New("Plane strict session identity must not be readable by group or others")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return planeprotocol.UUID{}, nonce, err
		}
		var value sessionFile
		if err := json.Unmarshal(raw, &value); err != nil {
			return planeprotocol.UUID{}, nonce, err
		}
		sessionID, err := parseUUID(value.SessionID)
		if err != nil {
			return planeprotocol.UUID{}, nonce, err
		}
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(value.InstanceNonce)
		if err != nil || len(decoded) != len(nonce) {
			return planeprotocol.UUID{}, nonce, errors.New("invalid Plane strict session instance nonce")
		}
		copy(nonce[:], decoded)
		return sessionID, nonce, nil
	}
	if sessionID, nonce, err := read(); err == nil {
		return sessionID, nonce, nil
	} else if !os.IsNotExist(err) {
		return planeprotocol.UUID{}, [16]byte{}, fmt.Errorf("read Plane strict session identity: %w", err)
	}

	var sessionID planeprotocol.UUID
	var nonce [16]byte
	if _, err := rand.Read(sessionID[:]); err != nil {
		return sessionID, nonce, err
	}
	sessionID[6] = (sessionID[6] & 0x0f) | 0x40
	sessionID[8] = (sessionID[8] & 0x3f) | 0x80
	if _, err := rand.Read(nonce[:]); err != nil {
		return sessionID, nonce, err
	}
	raw, err := json.Marshal(sessionFile{
		SessionID: UUIDString(sessionID), InstanceNonce: base64.RawURLEncoding.EncodeToString(nonce[:]),
	})
	if err != nil {
		return sessionID, nonce, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return read()
	}
	if err != nil {
		return sessionID, nonce, fmt.Errorf("create Plane strict session identity: %w", err)
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		_ = file.Close()
		return sessionID, nonce, err
	}
	if err := file.Close(); err != nil {
		return sessionID, nonce, err
	}
	return sessionID, nonce, nil
}

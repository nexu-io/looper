package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nexu-io/looper/internal/network/protocol"
)

type LocalState struct {
	URL       string                  `json:"url"`
	NetworkID string                  `json:"networkId"`
	NodeID    string                  `json:"nodeId"`
	NodeName  string                  `json:"nodeName"`
	NodeToken string                  `json:"nodeToken"`
	GitHub    protocol.GitHubIdentity `json:"github"`
}

func DefaultStatePath(homeDir string) string {
	return filepath.Join(homeDir, ".looper", "network.json")
}

func LoadState(path string) (LocalState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return LocalState{}, err
	}
	var state LocalState
	if err := json.Unmarshal(raw, &state); err != nil {
		return LocalState{}, fmt.Errorf("decode network state: %w", err)
	}
	return state, nil
}

func SaveState(path string, state LocalState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create network state directory: %w", err)
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode network state: %w", err)
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func RemoveState(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

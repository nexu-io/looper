package cloud

import (
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/network/protocol"
)

type Config struct {
	ListenAddr           string
	DBPath               string
	AdminToken           string
	NetworkID            string
	ProtocolVersion      string
	MinimumDaemonVersion string
	LeaseTTLSeconds      int
	ServerVersion        string
	AdvertiseURL         string
}

func LoadConfigFromEnv(env map[string]string, serverVersion string) (Config, error) {
	cfg := Config{
		ListenAddr:           strings.TrimSpace(env["LOOPERNET_LISTEN_ADDR"]),
		DBPath:               strings.TrimSpace(env["LOOPERNET_DB_PATH"]),
		AdminToken:           strings.TrimSpace(env["LOOPERNET_ADMIN_TOKEN"]),
		NetworkID:            strings.TrimSpace(env["LOOPERNET_NETWORK_ID"]),
		ProtocolVersion:      strings.TrimSpace(env["LOOPERNET_PROTOCOL_VERSION"]),
		MinimumDaemonVersion: strings.TrimSpace(env["LOOPERNET_MIN_DAEMON_VERSION"]),
		ServerVersion:        strings.TrimSpace(serverVersion),
		AdvertiseURL:         strings.TrimSpace(env["LOOPERNET_ADVERTISE_URL"]),
		LeaseTTLSeconds:      30,
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8089"
	}
	if cfg.DBPath == "" {
		return Config{}, fmt.Errorf("LOOPERNET_DB_PATH is required")
	}
	if cfg.AdminToken == "" {
		return Config{}, fmt.Errorf("LOOPERNET_ADMIN_TOKEN is required")
	}
	if cfg.ProtocolVersion == "" {
		cfg.ProtocolVersion = protocol.CurrentVersion
	}
	return cfg, nil
}

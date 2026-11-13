package cloud

import (
	"fmt"
	"strconv"
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
	TrustPrivateKeyFile  string
	TrustKeyRevision     uint64
	LinkChallengeTTL     int
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
		TrustPrivateKeyFile:  strings.TrimSpace(env["LOOPERNET_TRUST_PRIVATE_KEY_FILE"]),
		LeaseTTLSeconds:      30,
		LinkChallengeTTL:     120,
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
	if raw := strings.TrimSpace(env["LOOPERNET_TRUST_KEY_REVISION"]); raw != "" {
		revision, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || revision == 0 {
			return Config{}, fmt.Errorf("LOOPERNET_TRUST_KEY_REVISION must be a positive integer")
		}
		cfg.TrustKeyRevision = revision
	}
	if cfg.TrustPrivateKeyFile != "" && cfg.TrustKeyRevision == 0 {
		return Config{}, fmt.Errorf("LOOPERNET_TRUST_KEY_REVISION is required with LOOPERNET_TRUST_PRIVATE_KEY_FILE")
	}
	return cfg, nil
}

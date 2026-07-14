package forge

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// TrustedEnvFileEnv is the process env key that points at a KEY=value file of
// secrets for trusted Looper CLI wrappers (for example `looper review submit`).
// The agent process receives only this path, never the secret values themselves.
const TrustedEnvFileEnv = "LOOPER_TRUSTED_ENV_FILE"

var (
	trustedEnvCacheMu sync.Mutex
	trustedEnvCache   map[string]map[string]string
)

// WriteTrustedEnvFile materializes provider secrets into a 0600 temp file and
// returns its path. Callers must remove the file when the agent run ends.
func WriteTrustedEnvFile(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	file, err := os.CreateTemp("", "looper-trusted-env-*")
	if err != nil {
		return "", fmt.Errorf("create trusted env file: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", fmt.Errorf("chmod trusted env file: %w", err)
	}
	for key, value := range env {
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, "=\n\r") {
			continue
		}
		if strings.ContainsAny(value, "\n\r") {
			_ = file.Close()
			cleanup()
			return "", fmt.Errorf("trusted env value for %s contains a newline", key)
		}
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
			_ = file.Close()
			cleanup()
			return "", fmt.Errorf("write trusted env file: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close trusted env file: %w", err)
	}
	return path, nil
}

// LookupProviderToken resolves a provider tokenEnv name from the process
// environment first, then from LOOPER_TRUSTED_ENV_FILE when present.
func LookupProviderToken(tokenEnv string) string {
	tokenEnv = strings.TrimSpace(tokenEnv)
	if tokenEnv == "" {
		return ""
	}
	if value := strings.TrimSpace(os.Getenv(tokenEnv)); value != "" {
		return value
	}
	return strings.TrimSpace(lookupTrustedEnvValue(tokenEnv))
}

func lookupTrustedEnvValue(key string) string {
	path := strings.TrimSpace(os.Getenv(TrustedEnvFileEnv))
	if path == "" {
		return ""
	}
	values, err := loadTrustedEnvFile(path)
	if err != nil {
		return ""
	}
	return values[key]
}

func loadTrustedEnvFile(path string) (map[string]string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return nil, fmt.Errorf("trusted env file path is empty")
	}

	trustedEnvCacheMu.Lock()
	defer trustedEnvCacheMu.Unlock()
	if trustedEnvCache == nil {
		trustedEnvCache = map[string]map[string]string{}
	}
	if cached, ok := trustedEnvCache[path]; ok {
		return cached, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	// Tokens can be long; keep a generous but finite line limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	trustedEnvCache[path] = values
	return values, nil
}

package hostingidentity

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// SanitizeAgentEnv copies an already merged environment, removing hosting
// credentials and inherited transport/configuration capabilities. Model
// credentials are preserved. The caller adds only this execution's broker
// socket and empty GH_CONFIG_DIR after this final scrub.
func SanitizeAgentEnv(cfg config.Config, env map[string]string) map[string]string {
	blocked := make(map[string]bool)
	privatePaths := make(map[string]bool)
	for _, identity := range cfg.Identities {
		blocked[strings.ToUpper(identity.TokenEnv)] = true
		if identity.PrivateKeyFile != "" {
			privatePaths[identity.PrivateKeyFile] = true
		}
	}
	for _, provider := range cfg.Providers {
		if provider.TokenEnv != nil {
			blocked[strings.ToUpper(*provider.TokenEnv)] = true
		}
	}
	clean := make(map[string]string, len(env))
	for name, value := range env {
		upper := strings.ToUpper(name)
		if blocked[upper] || hostingEnvKey(upper) || privatePaths[value] {
			continue
		}
		clean[name] = value
	}
	clean["GIT_TERMINAL_PROMPT"] = "0"
	clean["GIT_SSH_COMMAND"] = "false"
	clean["GIT_CONFIG_NOSYSTEM"] = "1"
	clean["GIT_CONFIG_GLOBAL"] = os.DevNull
	clean["GIT_NO_LAZY_FETCH"] = "1"
	return clean
}

func hostingEnvKey(name string) bool {
	for _, prefix := range []string{"GH_", "GITHUB_", "FORGEJO_", "GITEA_", "TEA_", "SSH_", "GIT_", "LOOPER_TRUSTED_", "LOOPER_HOST_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == "LOOPER_CONFIG" || name == "LOOPER_ENV_FILE" || name == "GITLAB_TOKEN" || strings.Contains(name, "PRIVATE_KEY") || strings.Contains(name, "PRIVATEKEY")
}

// GitAttributionEnv contains no token or private-key path. Git's user identity
// supplies new-commit authors; omitting GIT_AUTHOR_* lets amend/cherry-pick keep
// the original author. Operator commit overrides are already captured by Session.
func GitAttributionEnv(ctx context.Context) (map[string]string, error) {
	session, selected := FromContext(ctx)
	if !selected {
		return nil, nil
	}
	identity, err := session.CommitIdentity(ctx)
	if err != nil {
		return nil, err
	}
	env := map[string]string{"GIT_COMMITTER_NAME": identity.Name, "GIT_COMMITTER_EMAIL": identity.Email}
	gitConfigEnv(env, [][2]string{{"user.name", identity.Name}, {"user.email", identity.Email}, {"commit.gpgSign", "false"}, {"tag.gpgSign", "false"}, {"protocol.allow", "never"}})
	return env, nil
}

func gitConfigEnv(env map[string]string, entries [][2]string) {
	count, _ := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	for _, entry := range entries {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(count)] = entry[0]
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(count)] = entry[1]
		count++
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(count)
}

func privilegedEnv(source map[string]string) map[string]string {
	if source == nil {
		source = make(map[string]string)
		for _, entry := range os.Environ() {
			if name, value, ok := strings.Cut(entry, "="); ok {
				source[name] = value
			}
		}
	}
	clean := make(map[string]string)
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "SYSTEMROOT", "WINDIR"} {
		if value, ok := source[name]; ok {
			clean[name] = value
		}
	}
	clean["GIT_CONFIG_NOSYSTEM"] = "1"
	clean["GIT_CONFIG_GLOBAL"] = os.DevNull
	clean["GIT_TERMINAL_PROMPT"] = "0"
	clean["GIT_SSH_COMMAND"] = "false"
	clean["GIT_NO_LAZY_FETCH"] = "1"
	gitConfigEnv(clean, [][2]string{{"core.hooksPath", os.DevNull}, {"credential.helper", ""}, {"credential.interactive", "false"}, {"protocol.allow", "never"}, {"submodule.recurse", "false"}, {"fetch.recurseSubmodules", "false"}, {"push.recurseSubmodules", "no"}, {"commit.gpgSign", "false"}, {"tag.gpgSign", "false"}})
	return clean
}

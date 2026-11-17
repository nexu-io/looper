package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/processcontainment"
)

// agentCommand is shared by initial spawn and both native-resume fallback paths.
// Wrapping happens after vendor argument shaping, never through params.command.
func (e *ConfiguredExecutor) agentCommand(command string, args []string, input RunInput, prompt string, cfg ExecutorConfig, hosting *hostingExecution) (*exec.Cmd, error) {
	env := buildCommandEnv(input.WorkingDirectory, prompt, cfg.Env, input.Env)
	if hosting != nil {
		env = hosting.commandEnv(input.WorkingDirectory, prompt, cfg.Env, input.Env)
	}
	if e.nono != nil {
		if !filepath.IsAbs(e.nono.Profile) {
			return nil, fmt.Errorf("reviewer nono profile must be an absolute operator-owned path")
		}
		if info, err := os.Stat(e.nono.Profile); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("reviewer nono profile is not a readable regular file: %s", e.nono.Profile)
		}
		wrapped := []string{"run", "--profile", e.nono.Profile, "--workdir", input.WorkingDirectory, "--read", input.WorkingDirectory,
			"--silent", "--no-rollback", "--no-rollback-prompt", "--no-diagnostics", "--startup-timeout", "0"}
		// These grants are per-run capabilities created by Looper, not paths supplied
		// in repository instructions. Never grant a whole socket parent or /tmp.
		if hosting != nil {
			wrapped = append(wrapped, "--read", hosting.dir, "--read-file", hosting.env[forge.HostCLIEnv], "--allow-unix-socket", hosting.env[forge.HostSockEnv])
		}
		if socket := input.Env[forge.TrustedReviewSockEnv]; socket != "" {
			wrapped = append(wrapped, "--allow-unix-socket", socket)
		}
		wrapped = append(wrapped, "--", command)
		args = append(wrapped, args...)
		command = strings.TrimSpace(e.nono.Command)
		if command == "" {
			command = "nono"
		}
		// nono also parses policy flags from environment variables. Neither global
		// agent.env nor per-run env may silently expand the operator's profile.
		clean := make([]string, 0, len(env))
		for _, entry := range env {
			key, _, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(key, "NONO_") || strings.HasPrefix(key, "LD_") || strings.HasPrefix(key, "DYLD_") {
				continue
			}
			clean = append(clean, entry)
		}
		env = clean
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = input.WorkingDirectory
	cmd.Env = env
	processcontainment.Configure(cmd)
	return cmd, nil
}

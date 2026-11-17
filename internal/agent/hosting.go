package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/processcontainment"
)

type hostingExecution struct {
	config        config.Config
	env           map[string]string
	dir           string
	cancel        context.CancelFunc
	cleanupSocket func()
	once          sync.Once
}

func (e *ConfiguredExecutor) prepareHosting(ctx context.Context, input RunInput) (*hostingExecution, error) {
	session, selected := hostingidentity.FromContext(ctx)
	if !selected {
		return nil, nil
	}
	if e.hostingConfig == nil {
		return nil, errors.New("bot agent execution requires its captured hosting configuration")
	}
	if session.ProjectID() != input.ProjectID {
		return nil, errors.New("bot agent execution project differs from its hosting identity")
	}
	cfg := *e.hostingConfig
	effective := e.effectiveConfig(input)
	cfg.Agent.Vendor, cfg.Agent.Model = &effective.Vendor, effective.Model
	// Include the immutable binding even if later configuration no longer names
	// this token variable/private-key path. It must still be scrubbed on resume.
	cfg.Identities = maps.Clone(cfg.Identities)
	if cfg.Identities == nil {
		cfg.Identities = map[string]config.HostingIdentityConfig{}
	}
	cfg.Identities[session.Name()] = session.Snapshot().Definition
	gitCommand := "git"
	if cfg.Tools.GitPath != nil && strings.TrimSpace(*cfg.Tools.GitPath) != "" {
		gitCommand = *cfg.Tools.GitPath
	}
	gitPath, err := exec.LookPath(gitCommand)
	if err != nil {
		return nil, fmt.Errorf("resolve trusted bot git executable: %w", err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		return nil, err
	}
	cfg.Tools.GitPath = &gitPath
	looperPath, err := exec.LookPath(e.trustedLooperPath)
	if err != nil {
		return nil, fmt.Errorf("resolve trusted hosting CLI: %w", err)
	}
	looperPath, err = filepath.Abs(looperPath)
	if err != nil {
		return nil, err
	}
	attribution, err := hostingidentity.GitAttributionEnv(ctx)
	if err != nil {
		return nil, err
	}
	dir, paths, err := prepareHostingPaths(gitPath)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	runtime := &hostingExecution{config: cfg, dir: dir, cancel: cancel, env: attribution}
	maps.Copy(runtime.env, paths)
	failed := true
	defer func() {
		if failed {
			runtime.close(false)
		}
	}()
	socket, cleanup, err := forge.StartHostBroker(runCtx, forge.HostBrokerOptions{Config: cfg, RealLooper: looperPath, CWD: input.WorkingDirectory, Review: input.TrustedReview, Tracker: e.hostingTracker})
	if err != nil {
		return nil, err
	}
	runtime.cleanupSocket = cleanup
	runtime.env[forge.HostSockEnv] = socket
	runtime.env[forge.HostCLIEnv] = looperPath
	failed = false
	return runtime, nil
}

func prepareHostingPaths(gitPath string) (dir string, env map[string]string, err error) {
	dir, err = os.MkdirTemp("", "looper-agent-host-*")
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	ghConfig, bin := filepath.Join(dir, "gh"), filepath.Join(dir, "bin")
	if err = os.Mkdir(ghConfig, 0o700); err != nil {
		return dir, nil, err
	}
	if err = os.Mkdir(bin, 0o700); err != nil {
		return dir, nil, err
	}
	blocked := "#!/bin/sh\nprintf '%s\\n' 'Use \"$LOOPER_HOST_CLI\" host for repository reads inside an agent execution; Looper owns publication.' >&2\nexit 2\n"
	for name, script := range map[string]string{"git": localGitShim(gitPath), "gh": blocked, "tea": blocked} {
		if err = os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			return dir, nil, err
		}
	}
	return dir, map[string]string{"GH_CONFIG_DIR": ghConfig, "LOOPER_HOST_BIN": bin}, nil
}

// PrepareHostingValidationEnv applies the same no-personal-auth environment to
// commands that execute agent-edited tests/build scripts. Validation receives
// no hosting capability or credentials. Cleanup must receive the original
// shell.Run error: unconfirmed process death retains routing paths so surviving
// descendants cannot fall through to personal authentication. Existing shell
// containment/tracking stays with the caller.
func PrepareHostingValidationEnv(cfg config.Config, inherited map[string]string) (map[string]string, func(error), error) {
	command := "git"
	if cfg.Tools.GitPath != nil && strings.TrimSpace(*cfg.Tools.GitPath) != "" {
		command = *cfg.Tools.GitPath
	}
	gitPath, err := exec.LookPath(command)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve trusted validation git: %w", err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		return nil, nil, err
	}
	dir, paths, err := prepareHostingPaths(gitPath)
	if err != nil {
		return nil, nil, err
	}
	env := hostingidentity.SanitizeAgentEnv(cfg, inherited)
	env["GH_CONFIG_DIR"] = paths["GH_CONFIG_DIR"]
	env["PATH"] = paths["LOOPER_HOST_BIN"] + string(os.PathListSeparator) + env["PATH"]
	env["GIT_CONFIG_COUNT"] = "1"
	env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"] = "protocol.allow", "never"
	var once sync.Once
	return env, func(runErr error) {
		once.Do(func() {
			if !errors.Is(runErr, processcontainment.ErrNotConfirmedDead) {
				_ = os.RemoveAll(dir)
			}
		})
	}, nil
}

func (h *hostingExecution) commandEnv(cwd, prompt string, sources ...map[string]string) []string {
	merged := envSliceToMap(buildCommandEnv(cwd, prompt, sources...))
	merged = hostingidentity.SanitizeAgentEnv(h.config, merged)
	// Capability and non-secret attribution are injected only after every
	// caller/config environment source has been merged and scrubbed.
	for key, value := range h.env {
		if key != "LOOPER_HOST_BIN" {
			merged[key] = value
		}
	}
	merged["PATH"] = h.env["LOOPER_HOST_BIN"] + string(os.PathListSeparator) + merged["PATH"]
	return envMapToSlice(merged)
}

func (h *hostingExecution) close(retain bool) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.cancel()
		if h.cleanupSocket != nil {
			h.cleanupSocket()
		}
		if !retain {
			_ = os.RemoveAll(h.dir)
		}
	})
}

func (x *execution) closeHosting() {
	if x.hosting != nil {
		x.hosting.close(x.handle != nil && !x.handle.ConfirmedDead())
	}
}

func localGitShim(realGit string) string {
	quoted := "'" + strings.ReplaceAll(realGit, "'", "'\"'\"'") + "'"
	// Git subcommands are allowlisted so aliases, remote helpers, -c overrides,
	// and new network entrypoints do not inherit a personal hosting login.
	// This is command routing, not a same-UID OS sandbox.
	return fmt.Sprintf(`#!/bin/sh
looper_expect_cwd=
looper_subcommand=
for looper_arg do
  if [ -n "$looper_expect_cwd" ]; then looper_expect_cwd=; continue; fi
  case "$looper_arg" in
    -C) looper_expect_cwd=1 ;;
    --no-pager|--paginate|--literal-pathspecs|--no-optional-locks) ;;
    --version|--help) exec %s "$@" ;;
    -*) printf 'Unsupported Git option in bot execution: %%s\n' "$looper_arg" >&2; exit 2 ;;
    *) looper_subcommand=$looper_arg; break ;;
  esac
done
case "$looper_subcommand" in
  status|diff|log|show|rev-parse|rev-list|branch|checkout|switch|restore|add|commit|reset|rebase|merge|cherry-pick|revert|rm|mv|ls-files|ls-tree|cat-file|check-ignore|check-attr|check-ref-format|describe|for-each-ref|update-index|write-tree|merge-base|symbolic-ref|hash-object|format-patch|apply|am|blame|clean|stash|worktree|config|tag|reflog|show-ref|diff-tree|diff-index|diff-files) exec %s "$@" ;;
  *) printf 'Git %%s is unavailable in bot execution; use "$LOOPER_HOST_CLI" host git fetch <ref> for reads. Looper publishes commits.\n' "$looper_subcommand" >&2; exit 2 ;;
esac
`, quoted, quoted)
}

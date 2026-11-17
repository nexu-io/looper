package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/processcontainment"
)

func TestHostingIdentityWorkerValidationCleanupFollowsProcessDrain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		command      string
		startFailure bool
		unconfirmed  bool
		passed       bool
	}{
		{name: "success", command: "printf complete", passed: true},
		{name: "command_failure", command: "printf failed; exit 1"},
		{name: "start_failure", startFailure: true},
		{name: "unconfirmed_death", command: "printf failed; exit 1", unconfirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := hostingidentity.BindResolved(context.Background(), config.ResolvedHostingIdentity{
				Name: "validation-bot", ProjectID: "project", Role: "worker",
				Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityGitHubApp, BaseURL: "https://github.com", AppID: 1, InstallationID: 2, PrivateKeyFile: "/unused.pem"},
				Target:     config.RepositoryIdentity{Kind: config.ProviderKindGitHub, BaseURL: "https://github.com", Repo: "acme/looper"},
			})
			if err != nil {
				t.Fatal(err)
			}
			var capturedEnv map[string]string
			var routingDir string
			calls := 0
			run := func(ctx context.Context, options shell.Options) (shell.Result, error) {
				calls++
				if calls == 1 {
					capturedEnv = options.Env
					routingDir = filepath.Dir(options.Env["GH_CONFIG_DIR"])
					if !filepath.IsAbs(routingDir) || !strings.HasPrefix(filepath.Base(routingDir), "looper-agent-host-") {
						t.Fatalf("validation did not allocate its private routing directory: %q", routingDir)
					}
					t.Cleanup(func() { _ = os.RemoveAll(routingDir) })
				}
				if options.Env["GH_CONFIG_DIR"] != capturedEnv["GH_CONFIG_DIR"] || !strings.HasPrefix(options.Env["PATH"], filepath.Join(routingDir, "bin")+string(os.PathListSeparator)) {
					t.Fatal("validation lost its private routing environment between commands")
				}
				if calls == 2 && tc.startFailure {
					options.Command = filepath.Join(t.TempDir(), "missing-executable")
				}
				result, err := shell.Run(ctx, options)
				if calls == 2 && tc.unconfirmed {
					// shell.Run joins this existing drain signal with command failures.
					err = errors.Join(err, fmt.Errorf("drain validation: %w", processcontainment.ErrNotConfirmedDead))
				}
				return result, err
			}
			runner := &Runner{}
			result, err := runner.runValidationCommands(ctx, ValidationInput{CWD: t.TempDir(), Commands: []string{"printf first", tc.command}}, run)
			if err != nil || result.Passed != tc.passed || calls != 2 {
				t.Fatalf("validation result = %#v, %v; command calls = %d", result, err, calls)
			}
			if tc.unconfirmed {
				if result.Output != "failed" {
					t.Fatalf("validation did not preserve command output while collapsing the drain error: %#v", result)
				}
				if _, err := os.Stat(filepath.Join(routingDir, "bin", "tea")); err != nil {
					t.Fatalf("unconfirmed validation death removed its blocking tea route: %v", err)
				}
				blocked, err := shell.Run(context.Background(), shell.Options{Command: "/bin/sh", Args: []string{"-c", "tea login list"}, Env: capturedEnv})
				var commandErr *shell.CommandExecutionError
				if !errors.As(err, &commandErr) || blocked.ExitCode != 2 || !strings.Contains(blocked.Stderr, "Looper owns publication") {
					t.Fatalf("retained validation environment no longer blocks personal tea: %#v, %v", blocked, err)
				}
			} else if _, err := os.Stat(routingDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("confirmed validation completion did not remove routing directory: %v", err)
			}
		})
	}
}

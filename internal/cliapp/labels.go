package cliapp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	githubinfra "github.com/powerformer/looper/internal/infra/github"
	"github.com/powerformer/looper/internal/infra/shell"
	"github.com/spf13/cobra"
)

const labelsInitCommandTimeout = 30 * time.Second

func (r *commandRuntime) labelsInit(cmd *cobra.Command, args []string) error {
	_ = args
	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}
	if loaded.Config.Tools.GHPath == nil || strings.TrimSpace(*loaded.Config.Tools.GHPath) == "" {
		return fmt.Errorf("GitHub CLI (gh) not found; install gh or set --gh-path <path>")
	}

	cwd, err := r.getwd()
	if err != nil {
		return fmt.Errorf("determine current working directory: %w", err)
	}

	repo := strings.TrimSpace(getStringFlag(cmd, "repo"))
	gh := githubinfra.New(githubinfra.Options{GHPath: *loaded.Config.Tools.GHPath, CWD: cwd, GHRun: r.runGHCommand})
	if repo == "" {
		detected, err := gh.DetectCurrentRepository(cmd.Context(), cwd)
		if err != nil {
			return fmt.Errorf("detect GitHub repository from current directory: run from a GitHub-backed repository or pass --repo owner/name: %w", err)
		}
		repo = detected
	}
	if authenticated, err := gh.IsAuthenticated(cmd.Context(), cwd, labelsAuthHostname(repo)); err != nil {
		return fmt.Errorf("check gh authentication: %w", err)
	} else if !authenticated {
		return fmt.Errorf("gh is not authenticated; run `gh auth login` and retry")
	}

	result, err := gh.InitializeLabels(cmd.Context(), githubinfra.InitializeLabelsInput{Repo: repo, CWD: cwd, DryRun: getBoolFlag(cmd, "dry-run")})
	if err != nil && result.Repo == "" {
		return fmt.Errorf("initialize labels for %s: %w", repo, err)
	}
	if getBoolFlag(cmd, "json") {
		if writeErr := writeJSON(cmd.OutOrStdout(), result); writeErr != nil {
			return writeErr
		}
	} else if writeErr := writeHumanLabelsInit(cmd.OutOrStdout(), result); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return fmt.Errorf("initialize labels for %s: %w", repo, err)
	}
	return nil
}

func labelsAuthHostname(repo string) string {
	const defaultHost = "github.com"
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return defaultHost
	}
	parts := strings.Split(repo, "/")
	if len(parts) == 3 && strings.TrimSpace(parts[0]) != "" {
		return strings.TrimSpace(parts[0])
	}
	return defaultHost
}

func (r *commandRuntime) runGHCommand(ctx context.Context, options shell.Options) (shell.Result, error) {
	result, err := r.runCommand(ctx, options.Command, options.Args, labelsInitCommandTimeout)
	shellResult := shell.Result{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
	if err != nil {
		return shellResult, err
	}
	if result.ExitCode != 0 {
		message := fmt.Sprintf("gh exited with code %d", result.ExitCode)
		if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
			message += ": " + stderr
		}
		return shellResult, &shell.CommandExecutionError{Message: message, Result: shellResult}
	}
	return shellResult, nil
}

func writeHumanLabelsInit(w io.Writer, result githubinfra.LabelInitResult) error {
	if result.DryRun {
		if _, err := fmt.Fprintf(w, "Previewing Looper labels for %s\n", result.Repo); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(w, "Initialized Looper labels for %s\n", result.Repo); err != nil {
		return err
	}

	for _, label := range result.Labels {
		line := fmt.Sprintf("%s %s", label.Status, label.Name)
		if label.Error != "" {
			line += ": " + label.Error
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintf(w, "Summary: created=%d updated=%d skipped=%d failed=%d\n", result.Summary.Created, result.Summary.Updated, result.Summary.Skipped, result.Summary.Failed)
	return err
}

package cliapp

import (
	"context"
	"fmt"
	"io"
	"sort"

	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreecleanup"
	"github.com/spf13/cobra"
)

func (r *commandRuntime) worktreeCleanup(cmd *cobra.Command, args []string) error {
	_ = args
	confirm := getBoolFlag(cmd, "confirm")
	dryRun := getBoolFlag(cmd, "dry-run")
	if confirm && dryRun {
		return fmt.Errorf("--confirm and --dry-run cannot be used together")
	}
	return r.withWorktreeCleanup(cmd.Context(), !confirm, func(result worktreecleanup.Result) error {
		if getBoolFlag(cmd, "json") {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		return writeHumanWorktreeCleanup(cmd.OutOrStdout(), result)
	})
}

func (r *commandRuntime) withWorktreeCleanup(ctx context.Context, dryRun bool, fn func(worktreecleanup.Result) error) error {
	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}
	db, err := storage.OpenSQLiteDB(ctx, loaded.Config.Storage.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	repos := storage.NewRepositories(db)
	git := gitinfra.New(gitinfra.Options{GitPath: worktreeCleanupDerefString(loaded.Config.Tools.GitPath), Repos: repos})
	result, err := worktreecleanup.Run(ctx, worktreecleanup.Options{
		Config: loaded.Config,
		Repos:  repos,
		Git:    git,
		DryRun: dryRun,
	})
	if err != nil {
		return err
	}
	return fn(result)
}

func worktreeCleanupDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func writeHumanWorktreeCleanup(w io.Writer, result worktreecleanup.Result) error {
	mode := "dry-run"
	if !result.DryRun {
		mode = "confirmed"
	}
	reasonCounts := map[string]int{}
	if _, err := fmt.Fprintf(w, "Worktree cleanup (%s): inspected=%d eligible=%d cleaned=%d skipped=%d errors=%d\n", mode, result.Summary.Inspected, result.Summary.Eligible, result.Summary.Cleaned, result.Summary.Skipped, result.Summary.Errors); err != nil {
		return err
	}
	for _, candidate := range result.Candidates {
		reasonCounts[candidate.Reason]++
		if candidate.Error != "" {
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", candidate.Action, candidate.Reason, candidate.ProjectID, candidate.Branch, candidate.Error); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", candidate.Action, candidate.Reason, candidate.ProjectID, candidate.Branch, candidate.WorktreePath); err != nil {
			return err
		}
	}
	if reasonCounts["project_not_configured"] > 0 {
		projectIDs := unconfiguredProjectIDs(result.Candidates)
		if _, err := fmt.Fprintf(w, "note\tproject_not_configured\t%d candidate(s) belong to project(s) missing from the current config: %s\n", reasonCounts["project_not_configured"], projectIDs); err != nil {
			return err
		}
	}
	return nil
}

func unconfiguredProjectIDs(candidates []worktreecleanup.Candidate) string {
	set := map[string]struct{}{}
	for _, candidate := range candidates {
		if candidate.Reason != "project_not_configured" || candidate.ProjectID == "" {
			continue
		}
		set[candidate.ProjectID] = struct{}{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "unknown"
	}
	return fmt.Sprintf("%v", ids)
}

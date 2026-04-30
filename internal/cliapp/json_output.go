package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/powerformer/looper/internal/version"
	"github.com/spf13/cobra"
)

type commandRuntime struct {
	app               *App
	argv              []string
	startupOutputPath string
	skipAPIStartProbe bool
}

func newCommandRuntime(app *App, argv []string) *commandRuntime {
	return &commandRuntime{app: app, argv: append([]string{}, argv...)}
}

func (r *commandRuntime) status(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		return r.getJSON(ctx, "/api/v1/status")
	}, writeHumanStatus)
}

func (r *commandRuntime) configShow(cmd *cobra.Command, args []string) error {
	if getBoolFlag(cmd, "source") {
		return r.configShowSource(cmd)
	}
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		return r.getJSON(ctx, "/api/v1/config")
	}, func(w io.Writer, payload json.RawMessage) error {
		return writeJSON(w, payload)
	})
}

func (r *commandRuntime) version(cmd *cobra.Command, args []string) error {
	_ = args
	info := version.Current()
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), info)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), info.Version)
	return err
}

func (r *commandRuntime) projectList(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		return r.getJSON(ctx, "/api/v1/projects")
	}, writeHumanProjectList)
}

func (r *commandRuntime) projectAdd(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		repoPath := strings.TrimSpace(getStringFlag(cmd, "repo-path"))
		if repoPath == "" && len(args) > 0 {
			repoPath = strings.TrimSpace(args[0])
		}
		repoPath, err := absolutePathIfSet(repoPath)
		if err != nil {
			return nil, fmt.Errorf("resolve repo path: %w", err)
		}

		worktreeRoot, err := absolutePathIfSet(getStringFlag(cmd, "worktree-root"))
		if err != nil {
			return nil, fmt.Errorf("resolve worktree root: %w", err)
		}

		body := map[string]any{}
		setString(body, "repoPath", repoPath)
		setString(body, "id", getStringFlag(cmd, "id"))
		setString(body, "name", getStringFlag(cmd, "name"))
		setString(body, "baseBranch", getStringFlag(cmd, "base-branch"))
		setString(body, "worktreeRoot", worktreeRoot)
		setString(body, "repo", getStringFlag(cmd, "repo"))
		setString(body, "snapshotMode", getStringFlag(cmd, "snapshot-mode"))

		return r.postJSON(ctx, "/api/v1/projects", body)
	}, writeHumanProjectAdd)
}

func absolutePathIfSet(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", nil
	}
	return filepath.Abs(trimmed)
}

func (r *commandRuntime) projectRemove(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		identifier, err := projectRemoveIdentifier(cmd, args)
		if err != nil {
			return nil, err
		}

		projects, err := r.listProjects(ctx)
		if err != nil {
			return nil, err
		}
		project, err := resolveProjectByIdentifier(projects, identifier)
		if err != nil {
			return nil, err
		}

		if !getBoolFlag(cmd, "force") {
			if err := confirmProjectRemoval(cmd, project); err != nil {
				return nil, err
			}
		}

		return r.deleteJSON(ctx, "/api/v1/projects/"+url.PathEscape(project.ID))
	}, writeHumanProjectRemove)
}

func (r *commandRuntime) loopList(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		return r.getJSON(ctx, "/api/v1/loops")
	}, writeHumanLoopList)
}

func (r *commandRuntime) loopStart(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		loopType := strings.TrimSpace(getStringFlag(cmd, "type"))
		if loopType == "" {
			return nil, fmt.Errorf("loop start requires --type <type>")
		}

		refText := strings.TrimSpace(getStringFlag(cmd, "pr"))
		if refText == "" {
			return nil, fmt.Errorf("loop start requires --pr <repo>#<number>")
		}

		repo, prNumber, err := parsePullRequestRef(refText)
		if err != nil {
			return nil, err
		}

		projectID, err := r.resolveLoopStartProjectID(ctx, repo, strings.TrimSpace(getStringFlag(cmd, "project")))
		if err != nil {
			return nil, err
		}

		body := map[string]any{
			"projectId":  projectID,
			"type":       loopType,
			"targetType": "pull_request",
			"repo":       repo,
			"prNumber":   prNumber,
			"status":     "running",
		}

		return r.postJSON(ctx, "/api/v1/loops", body)
	}, writeHumanLoopStarted)
}

func (r *commandRuntime) loopPause(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		loopID := strings.TrimSpace(getStringFlag(cmd, "id"))
		if loopID == "" && len(args) > 0 {
			loopID = strings.TrimSpace(args[0])
		}
		if loopID == "" {
			return nil, fmt.Errorf("Usage: looper loop pause <id>")
		}

		return r.postJSON(ctx, "/api/v1/loops/"+url.PathEscape(loopID)+"/pause", nil)
	}, writeHumanLoopPaused)
}

func (r *commandRuntime) pullRequestList(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		return r.getJSON(ctx, "/api/v1/pull-requests")
	}, writeHumanPullRequestList)
}

func (r *commandRuntime) pullRequestShow(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		repo, prNumber, err := parsePullRequestRef(args[0])
		if err != nil {
			return nil, err
		}
		return r.getJSON(ctx, pullRequestPath(repo, prNumber))
	}, writeHumanPullRequestShow)
}

func (r *commandRuntime) pullRequestStatus(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		repo, prNumber, err := parsePullRequestRef(args[0])
		if err != nil {
			return nil, err
		}
		return r.getJSON(ctx, pullRequestPath(repo, prNumber)+"/status")
	}, writeHumanPullRequestStatus)
}

func (r *commandRuntime) reviewCreate(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		projectID, repo, prNumber, err := r.resolveReviewTarget(ctx, strings.TrimSpace(args[0]), strings.TrimSpace(getStringFlag(cmd, "project")))
		if err != nil {
			return nil, err
		}

		body := map[string]any{
			"projectId":  projectID,
			"type":       "reviewer",
			"targetType": "pull_request",
			"repo":       repo,
			"prNumber":   prNumber,
			"status":     "running",
			"metadata": map[string]any{
				"followUpdates": getBoolFlag(cmd, "loop"),
				"manual":        true,
			},
		}

		return r.postJSON(ctx, "/api/v1/loops", body)
	}, func(w io.Writer, payload json.RawMessage) error {
		return writeHumanReviewCreate(w, payload, getBoolFlag(cmd, "loop"))
	})
}

func (r *commandRuntime) jump(cmd *cobra.Command, args []string) error {
	shell := strings.TrimSpace(getStringFlag(cmd, "shell-integration"))
	if shell != "" {
		helper, err := buildShellIntegration(shell)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), helper)
		return err
	}

	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("Usage: looper jump <id>")
	}

	payload, err := r.getJSON(cmd.Context(), "/api/v1/runs/active/"+url.PathEscape(strings.TrimSpace(args[0])))
	if err != nil {
		return err
	}

	var data activeRunDetailOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode active run response: %w", err)
	}
	if data.Worktree == nil || strings.TrimSpace(data.Worktree.Path) == "" {
		return fmt.Errorf("Loop %s has no active worktree path", strings.TrimSpace(args[0]))
	}

	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{
			"seq":       data.Seq,
			"loopId":    data.LoopID,
			"projectId": data.ProjectID,
			"worktree":  data.Worktree,
		})
	}

	if getBoolFlag(cmd, "print-path") {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), data.Worktree.Path)
		return err
	}

	_, err = fmt.Fprintln(cmd.OutOrStdout(), "cd -- "+quoteShellArg(data.Worktree.Path))
	return err
}

func (r *commandRuntime) activeRuns(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		query := url.Values{}
		if getBoolFlag(cmd, "all") {
			query.Set("all", "true")
		}
		addQueryString(query, "status", getStringFlag(cmd, "status"))
		addQueryString(query, "type", getStringFlag(cmd, "type"))
		addQueryString(query, "projectId", getStringFlag(cmd, "project"))

		path := "/api/v1/runs/active"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}

		return r.getJSON(ctx, path)
	}, writeHumanActiveRuns)
}

func (r *commandRuntime) loopLogs(cmd *cobra.Command, args []string) error {
	if getBoolFlag(cmd, "follow") {
		if getBoolFlag(cmd, "json") {
			return fmt.Errorf("--json cannot be combined with --follow")
		}
		return r.followLoopLogs(cmd, strings.TrimSpace(args[0]))
	}

	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		return r.getJSON(ctx, "/api/v1/loops/"+url.PathEscape(strings.TrimSpace(args[0]))+"/logs")
	}, func(w io.Writer, payload json.RawMessage) error {
		return writeHumanLoopLogs(w, payload, getBoolFlag(cmd, "stderr"), getBoolFlag(cmd, "full"), getStringFlag(cmd, "tail"))
	})
}

func (r *commandRuntime) stopLoop(cmd *cobra.Command, args []string) error {
	selector := strings.TrimSpace(args[0])
	if selector == "all" {
		payload, err := r.postJSON(cmd.Context(), "/api/v1/runs/active/stop-all", nil)
		if err != nil {
			return err
		}
		if getBoolFlag(cmd, "json") {
			if err := writeJSON(cmd.OutOrStdout(), payload); err != nil {
				return err
			}
		} else if err := writeHumanStopAll(cmd.OutOrStdout(), payload); err != nil {
			return err
		}
		if failed, err := stopAllFailedCount(payload); err != nil {
			return err
		} else if failed > 0 {
			return fmt.Errorf("failed to stop %d running task(s)", failed)
		}
		return nil
	}
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		return r.postJSON(ctx, "/api/v1/runs/active/"+url.PathEscape(selector)+"/stop", nil)
	}, writeHumanStopLoop)
}

func stopAllFailedCount(payload json.RawMessage) (int, error) {
	var data struct {
		Summary struct {
			Failed int `json:"failed"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return 0, fmt.Errorf("decode stop-all response: %w", err)
	}
	return data.Summary.Failed, nil
}

func (r *commandRuntime) runList(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		query := url.Values{}
		addQueryString(query, "loopId", getStringFlag(cmd, "loop"))

		path := "/api/v1/runs"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}

		return r.getJSON(ctx, path)
	}, writeHumanRunList)
}

func (r *commandRuntime) workCreate(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		issueNumberValue := strings.TrimSpace(getStringFlag(cmd, "issue"))
		prompt := strings.TrimSpace(getStringFlag(cmd, "prompt"))
		specPath := strings.TrimSpace(getStringFlag(cmd, "spec"))
		projectID := strings.TrimSpace(getStringFlag(cmd, "project"))
		repo := strings.TrimSpace(getStringFlag(cmd, "repo"))
		resolvedProjectID := projectID

		body := map[string]any{}
		setString(body, "repo", repo)
		setString(body, "baseBranch", getStringFlag(cmd, "base-branch"))

		if issueNumberValue != "" {
			if prompt != "" || specPath != "" {
				return nil, fmt.Errorf("--issue cannot be combined with --prompt or --spec")
			}
			issueNumber, err := parsePositiveInt(issueNumberValue, "--issue")
			if err != nil {
				return nil, err
			}
			body["issueNumber"] = issueNumber

			if resolvedProjectID == "" && repo == "" {
				projects, err := r.listProjects(ctx)
				if err != nil {
					return nil, err
				}

				project, err := r.resolveExplicitOrCurrentProject(projects, "")
				if err != nil {
					return nil, err
				}

				resolvedProjectID = project.ID
			}
		} else {
			setString(body, "title", getStringFlag(cmd, "title"))
			setString(body, "prompt", prompt)
			setString(body, "specPath", specPath)
		}

		setString(body, "projectId", resolvedProjectID)

		if issueNumberValue != "" && strings.TrimSpace(getStringFlag(cmd, "title")) != "" {
			setString(body, "title", getStringFlag(cmd, "title"))
		}

		return r.postJSON(ctx, "/api/v1/workers", body)
	}, writeHumanWorkerCreate)
}

func (r *commandRuntime) planCreate(cmd *cobra.Command, args []string) error {
	return r.outputCommand(cmd, func(ctx context.Context) (json.RawMessage, error) {
		issueNumber, err := parsePositiveInt(strings.TrimSpace(getStringFlag(cmd, "issue")), "--issue")
		if err != nil {
			return nil, err
		}

		resolvedProjectID := strings.TrimSpace(getStringFlag(cmd, "project"))
		body := map[string]any{"issueNumber": issueNumber}

		if resolvedProjectID == "" {
			projects, err := r.listProjects(ctx)
			if err != nil {
				return nil, err
			}

			project, err := r.resolveExplicitOrCurrentProject(projects, resolvedProjectID)
			if err != nil {
				return nil, err
			}

			resolvedProjectID = project.ID
		}

		setString(body, "projectId", resolvedProjectID)

		return r.postJSON(ctx, "/api/v1/planners", body)
	}, writeHumanPlannerCreate)
}

func (r *commandRuntime) outputCommand(cmd *cobra.Command, fn func(ctx context.Context) (json.RawMessage, error), human func(io.Writer, json.RawMessage) error) error {
	payload, err := fn(cmd.Context())
	if err != nil {
		return err
	}

	if !getBoolFlag(cmd, "json") {
		return human(cmd.OutOrStdout(), payload)
	}

	return writeJSON(cmd.OutOrStdout(), payload)
}

func (r *commandRuntime) getJSON(ctx context.Context, path string) (json.RawMessage, error) {
	client, err := r.apiClient()
	if err != nil {
		return nil, err
	}

	var payload json.RawMessage
	if err := client.Get(ctx, path, &payload); err != nil {
		return nil, err
	}

	return payload, nil
}

func (r *commandRuntime) postJSON(ctx context.Context, path string, body any) (json.RawMessage, error) {
	client, err := r.apiClient()
	if err != nil {
		return nil, err
	}

	var payload json.RawMessage
	if err := client.Post(ctx, path, body, &payload); err != nil {
		return nil, err
	}

	return payload, nil
}

func (r *commandRuntime) deleteJSON(ctx context.Context, path string) (json.RawMessage, error) {
	client, err := r.apiClient()
	if err != nil {
		return nil, err
	}

	var payload json.RawMessage
	if err := client.Delete(ctx, path, &payload); err != nil {
		return nil, err
	}

	return payload, nil
}

func (r *commandRuntime) resolveLoopStartProjectID(ctx context.Context, repo string, explicitProjectID string) (string, error) {
	projects, err := r.listProjects(ctx)
	if err != nil {
		return "", err
	}
	project, err := resolveProjectForRepo(projects, repo, explicitProjectID)
	if err != nil {
		return "", err
	}
	return project.ID, nil
}

func writeJSON(w io.Writer, payload any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

func pullRequestPath(repo string, prNumber int64) string {
	return "/api/v1/pull-requests/" + url.PathEscape(repo) + "/" + strconv.FormatInt(prNumber, 10)
}

type activeRunDetailOutput struct {
	Seq       int64  `json:"seq"`
	LoopID    string `json:"loopId"`
	ProjectID string `json:"projectId"`
	Worktree  *struct {
		ID     *string `json:"id"`
		Path   string  `json:"path"`
		Branch *string `json:"branch"`
	} `json:"worktree"`
}

func (r *commandRuntime) resolveReviewTarget(ctx context.Context, value string, explicitProjectID string) (string, string, int64, error) {
	projects, err := r.listProjects(ctx)
	if err != nil {
		return "", "", 0, err
	}

	repo, prNumber, repoQualified, err := parseOptionalPullRequestRef(value)
	if err != nil {
		return "", "", 0, err
	}

	if repoQualified {
		project, err := resolveProjectForRepo(projects, repo, explicitProjectID)
		if err != nil {
			return "", "", 0, err
		}
		return project.ID, repo, prNumber, nil
	}

	project, err := r.resolveExplicitOrCurrentProject(projects, explicitProjectID)
	if err != nil {
		return "", "", 0, err
	}
	if project.Repo == nil || strings.TrimSpace(*project.Repo) == "" {
		return "", "", 0, fmt.Errorf("project %s is missing a configured repo", project.ID)
	}

	return project.ID, strings.TrimSpace(*project.Repo), prNumber, nil
}

func (r *commandRuntime) listProjects(ctx context.Context) ([]projectOutput, error) {
	payload, err := r.getJSON(ctx, "/api/v1/projects")
	if err != nil {
		return nil, err
	}

	var data projectsListOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, fmt.Errorf("decode projects response: %w", err)
	}
	return data.Items, nil
}

func (r *commandRuntime) resolveExplicitOrCurrentProject(projects []projectOutput, explicitProjectID string) (projectOutput, error) {
	if explicitProjectID != "" {
		for _, project := range projects {
			if project.ID == explicitProjectID {
				return project, nil
			}
		}
		return projectOutput{}, fmt.Errorf("project not found: %s", explicitProjectID)
	}

	cwd, err := r.getwd()
	if err != nil {
		return projectOutput{}, fmt.Errorf("determine current working directory: %w", err)
	}
	return resolveProjectForCWD(projects, cwd)
}

func resolveProjectForRepo(projects []projectOutput, repo string, explicitProjectID string) (projectOutput, error) {
	if explicitProjectID != "" {
		for _, project := range projects {
			if project.ID != explicitProjectID {
				continue
			}
			configuredRepo := ""
			if project.Repo != nil {
				configuredRepo = strings.TrimSpace(*project.Repo)
			}
			if configuredRepo != repo {
				if configuredRepo == "" {
					configuredRepo = "no repo"
				}
				return projectOutput{}, fmt.Errorf("project %s is configured for %s, not %s", explicitProjectID, configuredRepo, repo)
			}
			return project, nil
		}
		return projectOutput{}, fmt.Errorf("project not found: %s", explicitProjectID)
	}

	matches := make([]projectOutput, 0, 1)
	for _, project := range projects {
		if project.Repo != nil && strings.TrimSpace(*project.Repo) == repo {
			matches = append(matches, project)
		}
	}
	if len(matches) == 0 {
		return projectOutput{}, fmt.Errorf("--project is required (no project configured for repo %s)", repo)
	}
	if len(matches) > 1 {
		return projectOutput{}, fmt.Errorf("--project is required (multiple projects are configured for repo %s)", repo)
	}
	return matches[0], nil
}

func resolveProjectForCWD(projects []projectOutput, cwd string) (projectOutput, error) {
	normalizedCWD := normalizeComparablePath(cwd)
	type rankedProject struct {
		project            projectOutput
		normalizedRepoPath string
	}

	matches := make([]rankedProject, 0, len(projects))
	for _, project := range projects {
		normalizedRepoPath := normalizeComparablePath(project.RepoPath)
		if isWithinProjectRepo(normalizedCWD, normalizedRepoPath) {
			matches = append(matches, rankedProject{project: project, normalizedRepoPath: normalizedRepoPath})
		}
	}
	if len(matches) == 0 {
		return projectOutput{}, fmt.Errorf("--project is required (no project matched cwd %s)", normalizedCWD)
	}

	best := matches[0]
	for _, candidate := range matches[1:] {
		if len(candidate.normalizedRepoPath) > len(best.normalizedRepoPath) {
			best = candidate
		}
	}
	for _, candidate := range matches {
		if candidate.project.ID != best.project.ID && len(candidate.normalizedRepoPath) == len(best.normalizedRepoPath) {
			return projectOutput{}, fmt.Errorf("--project is required (multiple projects matched cwd %s)", normalizedCWD)
		}
	}

	return best.project, nil
}

func isWithinProjectRepo(cwd string, repoPath string) bool {
	return cwd == repoPath || strings.HasPrefix(cwd, repoPath+string(os.PathSeparator))
}

func normalizeComparablePath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		absPath = filepath.Clean(trimmed)
	}
	if strings.HasPrefix(absPath, "/private/") {
		return strings.TrimPrefix(absPath, "/private")
	}
	return absPath
}

func parsePullRequestRef(value string) (string, int64, error) {
	trimmed := strings.TrimSpace(value)
	parts := strings.Split(trimmed, "#")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}

	repo := strings.TrimSpace(parts[0])
	if repo == "" {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}

	prNumber, err := parsePositiveInt(strings.TrimSpace(parts[1]), "pull request number")
	if err != nil {
		return "", 0, fmt.Errorf("pull request reference must be <repo>#<number>")
	}

	return repo, prNumber, nil
}

func parseOptionalPullRequestRef(value string) (string, int64, bool, error) {
	trimmed := strings.TrimSpace(value)
	if strings.Contains(trimmed, "#") {
		repo, prNumber, err := parsePullRequestRef(trimmed)
		if err != nil {
			return "", 0, false, err
		}
		return repo, prNumber, true, nil
	}

	prNumber, err := parsePositiveInt(trimmed, "pull request number")
	if err != nil {
		return "", 0, false, fmt.Errorf("pull request reference must be <repo>#<number> or <number>")
	}
	return "", prNumber, false, nil
}

func parsePositiveInt(value string, flag string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%s must be a positive integer", flag)
	}

	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", flag)
	}

	return parsed, nil
}

func setString(target map[string]any, key, value string) {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		target[key] = trimmed
	}
}

type projectIdentifierSource string

const (
	projectIdentifierSourceAny  projectIdentifierSource = "any"
	projectIdentifierSourceID   projectIdentifierSource = "id"
	projectIdentifierSourceName projectIdentifierSource = "name"
)

type projectRemoveIdentifierValue struct {
	value  string
	source projectIdentifierSource
}

func projectRemoveIdentifier(cmd *cobra.Command, args []string) (projectRemoveIdentifierValue, error) {
	identifier := ""
	if len(args) > 0 {
		identifier = strings.TrimSpace(args[0])
	}
	id := strings.TrimSpace(getStringFlag(cmd, "id"))
	name := strings.TrimSpace(getStringFlag(cmd, "name"))
	provided := 0
	for _, value := range []string{identifier, id, name} {
		if value != "" {
			provided++
		}
	}
	if provided == 0 {
		return projectRemoveIdentifierValue{}, fmt.Errorf("Usage: looper project remove <id-or-name> [--force]")
	}
	if provided > 1 {
		return projectRemoveIdentifierValue{}, fmt.Errorf("provide only one project identifier using an argument, --id, or --name")
	}
	if id != "" {
		return projectRemoveIdentifierValue{value: id, source: projectIdentifierSourceID}, nil
	}
	if name != "" {
		return projectRemoveIdentifierValue{value: name, source: projectIdentifierSourceName}, nil
	}
	return projectRemoveIdentifierValue{value: identifier, source: projectIdentifierSourceAny}, nil
}

func resolveProjectByIdentifier(projects []projectOutput, identifier projectRemoveIdentifierValue) (projectOutput, error) {
	trimmed := strings.TrimSpace(identifier.value)
	if trimmed == "" {
		return projectOutput{}, fmt.Errorf("project identifier is required")
	}

	if identifier.source == projectIdentifierSourceAny || identifier.source == projectIdentifierSourceID {
		for _, project := range projects {
			if project.ID == trimmed {
				return project, nil
			}
		}
	}
	if identifier.source == projectIdentifierSourceID {
		return projectOutput{}, fmt.Errorf("project not found: %s", trimmed)
	}

	var matched *projectOutput
	if identifier.source == projectIdentifierSourceAny || identifier.source == projectIdentifierSourceName {
		for index := range projects {
			if strings.EqualFold(strings.TrimSpace(projects[index].Name), trimmed) {
				if matched != nil {
					return projectOutput{}, fmt.Errorf("project identifier matches multiple projects: %s", trimmed)
				}
				matched = &projects[index]
			}
		}
	}
	if matched != nil {
		return *matched, nil
	}

	return projectOutput{}, fmt.Errorf("project not found: %s", trimmed)
}

func confirmProjectRemoval(cmd *cobra.Command, project projectOutput) error {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Remove project %s (%s) and associated runtime records? Type %q to confirm: ", project.ID, project.Name, project.ID)
	scanner := bufio.NewScanner(cmd.InOrStdin())
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}
		return fmt.Errorf("project removal cancelled")
	}
	if strings.TrimSpace(scanner.Text()) != project.ID {
		return fmt.Errorf("project removal cancelled")
	}
	return nil
}

func addQueryString(query url.Values, key, value string) {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		query.Set(key, trimmed)
	}
}

func getStringFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func quoteShellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func buildShellIntegration(shell string) (string, error) {
	switch shell {
	case "bash", "zsh":
		return `lj() { eval "$(looper jump "$@")"; }`, nil
	case "fish":
		return "function lj\n  eval (looper jump $argv)\nend", nil
	default:
		return "", fmt.Errorf("Unsupported shell: %s", shell)
	}
}

func getBoolFlag(cmd *cobra.Command, name string) bool {
	value, _ := cmd.Flags().GetBool(name)
	return value
}

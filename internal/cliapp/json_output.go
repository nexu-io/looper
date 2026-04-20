package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/powerformer/looper/internal/config"
	"github.com/spf13/cobra"
)

type commandRuntime struct {
	app  *App
	argv []string
}

func newCommandRuntime(app *App, argv []string) *commandRuntime {
	return &commandRuntime{app: app, argv: append([]string{}, argv...)}
}

func (r *commandRuntime) status(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		return r.getJSON(ctx, "/api/v1/status")
	})
}

func (r *commandRuntime) configShow(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		return r.getJSON(ctx, "/api/v1/config")
	})
}

func (r *commandRuntime) projectList(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		return r.getJSON(ctx, "/api/v1/projects")
	})
}

func (r *commandRuntime) projectAdd(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		repoPath := strings.TrimSpace(getStringFlag(cmd, "repo-path"))
		if repoPath == "" && len(args) > 0 {
			repoPath = strings.TrimSpace(args[0])
		}

		body := map[string]any{}
		setString(body, "repoPath", repoPath)
		setString(body, "id", getStringFlag(cmd, "id"))
		setString(body, "name", getStringFlag(cmd, "name"))
		setString(body, "baseBranch", getStringFlag(cmd, "base-branch"))
		setString(body, "worktreeRoot", getStringFlag(cmd, "worktree-root"))
		setString(body, "repo", getStringFlag(cmd, "repo"))

		return r.postJSON(ctx, "/api/v1/projects", body)
	})
}

func (r *commandRuntime) loopList(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		return r.getJSON(ctx, "/api/v1/loops")
	})
}

func (r *commandRuntime) loopStart(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
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

		projectID, err := r.lookupPullRequestProjectID(ctx, repo, prNumber)
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
	})
}

func (r *commandRuntime) loopPause(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		loopID := strings.TrimSpace(getStringFlag(cmd, "id"))
		if loopID == "" && len(args) > 0 {
			loopID = strings.TrimSpace(args[0])
		}
		if loopID == "" {
			return nil, fmt.Errorf("Usage: looper loop pause <id>")
		}

		return r.postJSON(ctx, "/api/v1/loops/"+url.PathEscape(loopID)+"/pause", nil)
	})
}

func (r *commandRuntime) pullRequestList(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		return r.getJSON(ctx, "/api/v1/pull-requests")
	})
}

func (r *commandRuntime) pullRequestShow(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		repo, prNumber, err := parsePullRequestRef(args[0])
		if err != nil {
			return nil, err
		}
		return r.getJSON(ctx, pullRequestPath(repo, prNumber))
	})
}

func (r *commandRuntime) pullRequestStatus(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		repo, prNumber, err := parsePullRequestRef(args[0])
		if err != nil {
			return nil, err
		}
		return r.getJSON(ctx, pullRequestPath(repo, prNumber)+"/status")
	})
}

func (r *commandRuntime) reviewCreate(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		repo, prNumber, err := parsePullRequestRef(args[0])
		if err != nil {
			return nil, err
		}

		projectID := strings.TrimSpace(getStringFlag(cmd, "project"))
		if projectID == "" {
			projectID, err = r.lookupPullRequestProjectID(ctx, repo, prNumber)
			if err != nil {
				return nil, err
			}
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
	})
}

func (r *commandRuntime) activeRuns(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		query := url.Values{}
		addQueryString(query, "type", getStringFlag(cmd, "type"))
		addQueryString(query, "projectId", getStringFlag(cmd, "project"))

		path := "/api/v1/runs/active"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}

		return r.getJSON(ctx, path)
	})
}

func (r *commandRuntime) loopLogs(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		return r.getJSON(ctx, "/api/v1/loops/"+url.PathEscape(strings.TrimSpace(args[0]))+"/logs")
	})
}

func (r *commandRuntime) stopLoop(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		selector := strings.TrimSpace(args[0])
		return r.postJSON(ctx, "/api/v1/runs/active/"+url.PathEscape(selector)+"/stop", nil)
	})
}

func (r *commandRuntime) runList(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		query := url.Values{}
		addQueryString(query, "loopId", getStringFlag(cmd, "loop"))

		path := "/api/v1/runs"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}

		return r.getJSON(ctx, path)
	})
}

func (r *commandRuntime) workCreate(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		issueNumberValue := strings.TrimSpace(getStringFlag(cmd, "issue"))
		prompt := strings.TrimSpace(getStringFlag(cmd, "prompt"))
		specPath := strings.TrimSpace(getStringFlag(cmd, "spec"))

		body := map[string]any{}
		setString(body, "projectId", getStringFlag(cmd, "project"))
		setString(body, "repo", getStringFlag(cmd, "repo"))
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
		} else {
			setString(body, "title", getStringFlag(cmd, "title"))
			setString(body, "prompt", prompt)
			setString(body, "specPath", specPath)
		}

		if issueNumberValue != "" && strings.TrimSpace(getStringFlag(cmd, "title")) != "" {
			setString(body, "title", getStringFlag(cmd, "title"))
		}

		return r.postJSON(ctx, "/api/v1/workers", body)
	})
}

func (r *commandRuntime) planCreate(cmd *cobra.Command, args []string) error {
	return r.jsonCommand(cmd, func(ctx context.Context) (any, error) {
		issueNumber, err := parsePositiveInt(strings.TrimSpace(getStringFlag(cmd, "issue")), "--issue")
		if err != nil {
			return nil, err
		}

		body := map[string]any{"issueNumber": issueNumber}
		setString(body, "projectId", getStringFlag(cmd, "project"))

		return r.postJSON(ctx, "/api/v1/planners", body)
	})
}

func (r *commandRuntime) jsonCommand(cmd *cobra.Command, fn func(ctx context.Context) (any, error)) error {
	if !getBoolFlag(cmd, "json") {
		return notPortedCommand(cmd, nil)
	}

	payload, err := fn(cmd.Context())
	if err != nil {
		return err
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

func (r *commandRuntime) apiClient() (*DaemonAPIClient, error) {
	loaded, err := config.LoadFile(config.LoadFileOptions{Args: ExtractConfigArgs(r.argv)})
	if err != nil {
		return nil, err
	}

	baseURL := ""
	if loaded.Config.Server.BaseURL != nil && strings.TrimSpace(*loaded.Config.Server.BaseURL) != "" {
		baseURL = strings.TrimSpace(*loaded.Config.Server.BaseURL)
	} else {
		baseURL = fmt.Sprintf("http://%s:%d", loaded.Config.Server.Host, loaded.Config.Server.Port)
	}

	token := ""
	if loaded.Config.Server.AuthMode == config.AuthModeLocalToken && loaded.Config.Server.LocalToken != nil {
		token = strings.TrimSpace(*loaded.Config.Server.LocalToken)
	}

	return NewDaemonAPIClient(DaemonAPIClientOptions{BaseURL: baseURL, Token: token}), nil
}

func (r *commandRuntime) lookupPullRequestProjectID(ctx context.Context, repo string, prNumber int64) (string, error) {
	payload, err := r.getJSON(ctx, pullRequestPath(repo, prNumber))
	if err != nil {
		return "", err
	}

	var pr struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal(payload, &pr); err != nil {
		return "", fmt.Errorf("decode pull request response: %w", err)
	}
	if strings.TrimSpace(pr.ProjectID) == "" {
		return "", fmt.Errorf("pull request response missing projectId")
	}

	return pr.ProjectID, nil
}

func writeJSON(w io.Writer, payload any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

func pullRequestPath(repo string, prNumber int64) string {
	return "/api/v1/pull-requests/" + url.PathEscape(repo) + "/" + strconv.FormatInt(prNumber, 10)
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

func addQueryString(query url.Values, key, value string) {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		query.Set(key, trimmed)
	}
}

func getStringFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func getBoolFlag(cmd *cobra.Command, name string) bool {
	value, _ := cmd.Flags().GetBool(name)
	return value
}

package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// CommitCheck is the current status of an external check or Forgejo Actions
// workflow for one commit. URLs and descriptions are diagnostic context; the
// provider's state is the authority for whether CI is failing.
type CommitCheck struct {
	ID          int64
	Name        string
	State       string
	Description string
	URL         string
	ActionRunID int64
}

// ListCommitChecks selects the newest status for each context, and the newest
// Actions run for each workflow/event/ref. Historical failures must not keep a
// PR in the fixer queue after a successful rerun on the same commit.
func (forgejo *ForgejoClient) ListCommitChecks(ctx context.Context, sha string) ([]CommitCheck, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return nil, nil
	}
	var statuses []forgejoCommitStatus
	if err := forgejo.getPaged(ctx, forgejo.repoPath("statuses", url.PathEscape(sha)), url.Values{"sort": {"highestindex"}}, 0, &statuses); err != nil {
		return nil, err
	}
	latest := make(map[string]forgejoCommitStatus)
	for _, status := range statuses {
		prior, found := latest[status.Context]
		if !found || status.ID > prior.ID {
			latest[status.Context] = status
		}
	}
	checks := make([]CommitCheck, 0, len(latest))
	statusURLs := make(map[string]bool)
	for _, status := range latest {
		checks = append(checks, CommitCheck{ID: status.ID, Name: status.Context, State: strings.ToUpper(status.Status), Description: status.Description, URL: status.TargetURL})
		if status.TargetURL != "" {
			statusURLs[status.TargetURL] = true
		}
	}
	runs, err := forgejo.listCommitActionRuns(ctx, sha)
	if err != nil {
		// Older instances and repositories with Actions disabled can still use
		// external status checks. Authentication and transport errors are not
		// interpreted as an absence of CI.
		var httpErr *ForgejoHTTPError
		if !errors.As(err, &httpErr) || (httpErr.StatusCode != http.StatusNotFound && httpErr.StatusCode != http.StatusMethodNotAllowed) {
			return nil, err
		}
	}
	for _, run := range runs {
		if statusURLs[run.HTMLURL] {
			continue
		}
		name := strings.TrimSpace(run.WorkflowID)
		if name == "" {
			name = run.Title
		}
		if name == "" {
			name = "Forgejo Actions"
		}
		checks = append(checks, CommitCheck{ID: run.ID, Name: name, State: strings.ToUpper(run.Status), Description: run.Title, URL: run.HTMLURL, ActionRunID: run.ID})
	}
	sort.Slice(checks, func(i, j int) bool {
		if checks[i].Name == checks[j].Name {
			return checks[i].ID < checks[j].ID
		}
		return checks[i].Name < checks[j].Name
	})
	return checks, nil
}

func (forgejo *ForgejoClient) listCommitActionRuns(ctx context.Context, sha string) ([]forgejoActionRun, error) {
	latest := make(map[string]forgejoActionRun)
	for page := 1; ; page++ {
		// Do not combine event and head_sha filters: deployed Forgejo versions
		// can incorrectly return no runs when both filters are supplied.
		query := url.Values{"head_sha": {sha}, "page": {strconv.Itoa(page)}, "limit": {"50"}}
		response, err := forgejo.doRaw(ctx, http.MethodGet, forgejo.repoPath("actions", "runs"), query, nil)
		if err != nil {
			return nil, err
		}
		var output struct {
			Runs       *[]forgejoActionRun `json:"workflow_runs"`
			TotalCount int                 `json:"total_count"`
		}
		if err := json.Unmarshal(response.body, &output); err != nil {
			return nil, fmt.Errorf("forgejo API decode action runs: %w", err)
		}
		if output.Runs == nil {
			return nil, fmt.Errorf("forgejo API decode action runs: missing workflow_runs array")
		}
		for _, run := range *output.Runs {
			if !strings.EqualFold(run.CommitSHA, sha) {
				continue
			}
			key := strings.Join([]string{run.WorkflowID, run.Event, run.PrettyRef}, "\x00")
			if run.WorkflowID == "" {
				key = strconv.FormatInt(run.ID, 10)
			}
			if prior, found := latest[key]; !found || run.ID > prior.ID {
				latest[key] = run
			}
		}
		if !hasNextPage(response.header, page) && (len(*output.Runs) == 0 || page*50 >= output.TotalCount) {
			break
		}
	}
	runs := make([]forgejoActionRun, 0, len(latest))
	for _, run := range latest {
		runs = append(runs, run)
	}
	return runs, nil
}

type forgejoCommitStatus struct {
	ID          int64  `json:"id"`
	Context     string `json:"context"`
	Status      string `json:"status"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

type forgejoActionRun struct {
	ID         int64  `json:"id"`
	WorkflowID string `json:"workflow_id"`
	CommitSHA  string `json:"commit_sha"`
	Event      string `json:"event"`
	PrettyRef  string `json:"prettyref"`
	Status     string `json:"status"`
	Title      string `json:"title"`
	HTMLURL    string `json:"html_url"`
}

package cliapp

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type tableRow map[string]any

type statusOutput struct {
	Service struct {
		Healthy    bool    `json:"healthy"`
		Version    string  `json:"version"`
		DaemonMode string  `json:"daemonMode"`
		StartedAt  *string `json:"startedAt"`
	} `json:"service"`
	Storage struct {
		DBPath            string   `json:"dbPath"`
		SchemaVersion     string   `json:"schemaVersion"`
		PendingMigrations []string `json:"pendingMigrations"`
		Healthy           bool     `json:"healthy"`
	} `json:"storage"`
	Scheduler struct {
		Healthy      bool `json:"healthy"`
		QueuedItems  int  `json:"queuedItems"`
		RunningItems int  `json:"runningItems"`
	} `json:"scheduler"`
	Loops struct {
		Planner  statusLoopSummary `json:"planner"`
		Reviewer statusLoopSummary `json:"reviewer"`
		Worker   statusLoopSummary `json:"worker"`
		Fixer    statusLoopSummary `json:"fixer"`
	} `json:"loops"`
	Notifications struct {
		InAppEnabled     bool `json:"inAppEnabled"`
		OsascriptEnabled bool `json:"osascriptEnabled"`
	} `json:"notifications"`
	Tools struct {
		Git       bool `json:"git"`
		GH        bool `json:"gh"`
		Osascript bool `json:"osascript"`
	} `json:"tools"`
}

type statusLoopSummary struct {
	Running int `json:"running"`
	Paused  int `json:"paused"`
	Failed  int `json:"failed"`
}

type projectsListOutput struct {
	Items []projectOutput `json:"items"`
}

type projectOutput struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	RepoPath               string   `json:"repoPath"`
	BaseBranch             string   `json:"baseBranch"`
	Repo                   *string  `json:"repo"`
	UpdatedAt              string   `json:"updatedAt"`
	DiscoveredPullRequests int      `json:"discoveredPullRequests"`
	DiscoveredWorktrees    int      `json:"discoveredWorktrees"`
	Warnings               []string `json:"warnings"`
}

type loopsListOutput struct {
	Items []loopOutput `json:"items"`
}

type loopOutput struct {
	ID         string  `json:"id"`
	ProjectID  string  `json:"projectId"`
	Type       string  `json:"type"`
	TargetType string  `json:"targetType"`
	TargetID   *string `json:"targetId"`
	Repo       *string `json:"repo"`
	PRNumber   *int64  `json:"prNumber"`
	Status     string  `json:"status"`
}

type pullRequestsListOutput struct {
	Items []pullRequestOutput `json:"items"`
}

type pullRequestOutput struct {
	Repo                  string  `json:"repo"`
	PRNumber              int64   `json:"prNumber"`
	Title                 *string `json:"title"`
	ChecksSummary         *string `json:"checksSummary"`
	UnresolvedThreadCount int64   `json:"unresolvedThreadCount"`
	ReviewState           *string `json:"reviewState"`
	Reviewer              *string `json:"reviewer"`
	Fixer                 *string `json:"fixer"`
	ProjectID             *string `json:"projectId"`
	Status                string  `json:"status"`
	Type                  string  `json:"type"`
	ID                    string  `json:"id"`
	IssueNumber           *int64  `json:"issueNumber"`
	LoopStatus            struct {
		LatestRunStatus *string `json:"latestRunStatus"`
	} `json:"loopStatus"`
	Stopped     bool    `json:"stopped"`
	LoopID      string  `json:"loopId"`
	RunID       *string `json:"runId"`
	ExecutionID *string `json:"executionId"`
	Vendor      *string `json:"vendor"`
	PID         *int64  `json:"pid"`
}

type activeRunsOutput struct {
	Items []activeRunOutput `json:"items"`
}

type activeRunOutput struct {
	Seq         int64   `json:"seq"`
	Type        string  `json:"type"`
	Status      string  `json:"status"`
	CurrentStep *string `json:"currentStep"`
	StartedAt   *string `json:"startedAt"`
	Target      struct {
		Label string `json:"label"`
	} `json:"target"`
	Agent *struct {
		Vendor string `json:"vendor"`
		PID    *int64 `json:"pid"`
	} `json:"agent"`
}

type loopLogsOutput struct {
	Seq        int64  `json:"seq"`
	LoopType   string `json:"loopType"`
	LoopStatus string `json:"loopStatus"`
	Run        *struct {
		RunID       string  `json:"runId"`
		Status      string  `json:"status"`
		CurrentStep *string `json:"currentStep"`
	} `json:"run"`
	Agent *struct {
		ExecutionID  string  `json:"executionId"`
		Vendor       string  `json:"vendor"`
		PID          *int64  `json:"pid"`
		Status       string  `json:"status"`
		ErrorMessage *string `json:"errorMessage"`
		Stdout       string  `json:"stdout"`
		Stderr       string  `json:"stderr"`
	} `json:"agent"`
}

type runsListOutput struct {
	Items []runOutput `json:"items"`
}

type runOutput struct {
	ID          string  `json:"id"`
	LoopID      string  `json:"loopId"`
	Status      string  `json:"status"`
	CurrentStep *string `json:"currentStep"`
	StartedAt   string  `json:"startedAt"`
}

func writeHumanStatus(w io.Writer, payload json.RawMessage) error {
	var data statusOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode status response: %w", err)
	}

	printSection(w, "Service", [][2]any{{"healthy", data.Service.Healthy}, {"version", data.Service.Version}, {"daemonMode", data.Service.DaemonMode}, {"startedAt", data.Service.StartedAt}})
	fmt.Fprintln(w)
	printSection(w, "Storage", [][2]any{{"dbPath", data.Storage.DBPath}, {"schemaVersion", data.Storage.SchemaVersion}, {"healthy", data.Storage.Healthy}, {"pendingMigrations", joinOrNone(data.Storage.PendingMigrations)}})
	fmt.Fprintln(w)
	printSection(w, "Scheduler", [][2]any{{"healthy", data.Scheduler.Healthy}, {"queuedItems", data.Scheduler.QueuedItems}, {"runningItems", data.Scheduler.RunningItems}})
	fmt.Fprintln(w)
	printTable(w, []string{"type", "running", "paused", "failed"}, []tableRow{{"type": "planner", "running": data.Loops.Planner.Running, "paused": data.Loops.Planner.Paused, "failed": data.Loops.Planner.Failed}, {"type": "reviewer", "running": data.Loops.Reviewer.Running, "paused": data.Loops.Reviewer.Paused, "failed": data.Loops.Reviewer.Failed}, {"type": "worker", "running": data.Loops.Worker.Running, "paused": data.Loops.Worker.Paused, "failed": data.Loops.Worker.Failed}, {"type": "fixer", "running": data.Loops.Fixer.Running, "paused": data.Loops.Fixer.Paused, "failed": data.Loops.Fixer.Failed}})
	fmt.Fprintln(w)
	printSection(w, "Tools", [][2]any{{"git", data.Tools.Git}, {"gh", data.Tools.GH}, {"osascript", data.Tools.Osascript}})
	fmt.Fprintln(w)
	printSection(w, "Notifications", [][2]any{{"inAppEnabled", data.Notifications.InAppEnabled}, {"osascriptEnabled", data.Notifications.OsascriptEnabled}})
	return nil
}

func writeHumanProjectList(w io.Writer, payload json.RawMessage) error {
	var data projectsListOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode projects response: %w", err)
	}

	rows := make([]tableRow, 0, len(data.Items))
	for _, project := range data.Items {
		rows = append(rows, tableRow{"id": project.ID, "name": project.Name, "repoPath": project.RepoPath, "baseBranch": project.BaseBranch, "repo": project.Repo, "updatedAt": project.UpdatedAt})
	}
	printTable(w, []string{"id", "name", "repoPath", "baseBranch", "repo", "updatedAt"}, rows)
	return nil
}

func writeHumanProjectAdd(w io.Writer, payload json.RawMessage) error {
	var data projectOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode project response: %w", err)
	}

	printSection(w, "Project added", [][2]any{{"id", data.ID}, {"name", data.Name}, {"repoPath", data.RepoPath}, {"baseBranch", data.BaseBranch}, {"repo", data.Repo}, {"discoveredPullRequests", data.DiscoveredPullRequests}, {"discoveredWorktrees", data.DiscoveredWorktrees}})
	if len(data.Warnings) > 0 {
		fmt.Fprintln(w)
		entries := make([][2]any, 0, len(data.Warnings))
		for index, warning := range data.Warnings {
			entries = append(entries, [2]any{fmt.Sprintf("%d", index+1), warning})
		}
		printSection(w, "Warnings", entries)
	}
	return nil
}

func writeHumanLoopList(w io.Writer, payload json.RawMessage) error {
	var data loopsListOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode loops response: %w", err)
	}

	rows := make([]tableRow, 0, len(data.Items))
	for _, loop := range data.Items {
		rows = append(rows, tableRow{"id": loop.ID, "type": loop.Type, "status": loop.Status, "target": formatLoopTarget(loop), "projectId": loop.ProjectID})
	}
	printTable(w, []string{"id", "type", "status", "target", "projectId"}, rows)
	return nil
}

func writeHumanLoopStarted(w io.Writer, payload json.RawMessage) error {
	return writeLoopSummarySection(w, payload, "Loop started")
}

func writeHumanLoopPaused(w io.Writer, payload json.RawMessage) error {
	var data loopOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode loop response: %w", err)
	}
	printSection(w, "Loop paused", [][2]any{{"id", data.ID}, {"status", data.Status}})
	return nil
}

func writeHumanPullRequestList(w io.Writer, payload json.RawMessage) error {
	var data pullRequestsListOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode pull request list response: %w", err)
	}

	rows := make([]tableRow, 0, len(data.Items))
	for _, item := range data.Items {
		rows = append(rows, tableRow{"pr": fmt.Sprintf("%s#%d", item.Repo, item.PRNumber), "title": item.Title, "reviewState": item.ReviewState, "checks": item.ChecksSummary, "reviewer": item.Reviewer, "fixer": item.Fixer})
	}
	printTable(w, []string{"pr", "title", "reviewState", "checks", "reviewer", "fixer"}, rows)
	return nil
}

func writeHumanPullRequestShow(w io.Writer, payload json.RawMessage) error {
	var data pullRequestOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode pull request response: %w", err)
	}
	printSection(w, "Pull request", [][2]any{{"repo", data.Repo}, {"prNumber", data.PRNumber}, {"title", data.Title}, {"reviewState", data.ReviewState}, {"checksSummary", data.ChecksSummary}})
	return nil
}

func writeHumanPullRequestStatus(w io.Writer, payload json.RawMessage) error {
	var data pullRequestOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode pull request status response: %w", err)
	}
	printSection(w, "Pull request status", [][2]any{{"pr", fmt.Sprintf("%s#%d", data.Repo, data.PRNumber)}, {"reviewState", data.ReviewState}, {"checksSummary", data.ChecksSummary}, {"unresolvedThreads", data.UnresolvedThreadCount}, {"latestRunStatus", data.LoopStatus.LatestRunStatus}})
	return nil
}

func writeHumanReviewCreate(w io.Writer, payload json.RawMessage, loopEnabled bool) error {
	var data loopOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode reviewer response: %w", err)
	}
	printSection(w, "Reviewer started", [][2]any{{"id", data.ID}, {"projectId", data.ProjectID}, {"pr", formatPullRequestRef(data.Repo, data.PRNumber)}, {"status", data.Status}, {"loop", fmt.Sprintf("%t", loopEnabled)}})
	return nil
}

func writeHumanActiveRuns(w io.Writer, payload json.RawMessage) error {
	var data activeRunsOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode active runs response: %w", err)
	}

	if len(data.Items) == 0 {
		_, err := fmt.Fprintln(w, "No running or queued loops.")
		return err
	}

	rows := make([]tableRow, 0, len(data.Items))
	for _, item := range data.Items {
		rows = append(rows, tableRow{"#": item.Seq, "type": item.Type, "target": item.Target.Label, "step": item.CurrentStep, "agent": agentVendor(item.Agent), "pid": agentPID(item.Agent), "status": item.Status, "age": formatRelativeAge(item.StartedAt)})
	}
	printTable(w, []string{"#", "type", "target", "step", "agent", "pid", "status", "age"}, rows)
	return nil
}

func writeHumanLoopLogs(w io.Writer, payload json.RawMessage, stderr bool, full bool, tail string) error {
	data, err := decodeLoopLogsOutput(payload)
	if err != nil {
		return err
	}
	return writeHumanLoopLogsSnapshot(w, data, stderr, full, tail, false)
}

func decodeLoopLogsOutput(payload json.RawMessage) (loopLogsOutput, error) {
	var data loopLogsOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return loopLogsOutput{}, fmt.Errorf("decode loop logs response: %w", err)
	}
	return data, nil
}

func writeHumanLoopLogsSnapshot(w io.Writer, data loopLogsOutput, stderr bool, full bool, tail string, follow bool) error {
	if err := writeHumanLoopLogsHeader(w, data); err != nil {
		return err
	}
	if data.Agent == nil {
		message := "No agent output for the current step."
		if follow && loopLogsCanContinue(data) {
			message = "Waiting for agent output..."
		}
		_, err := fmt.Fprintln(w, message)
		return err
	}
	if err := writeHumanLoopLogsRunAgent(w, data); err != nil {
		return err
	}

	content, err := loopLogsInitialContent(data, stderr, full, tail)
	if err != nil {
		return err
	}
	if content == "" {
		message := "No output captured."
		if follow && loopLogsCanContinue(data) {
			message = "Waiting for log output..."
		}
		_, err := fmt.Fprintln(w, message)
		return err
	}

	return writeLoopLogContent(w, content)
}

func writeHumanLoopLogsHeader(w io.Writer, data loopLogsOutput) error {
	if _, err := fmt.Fprintf(w, "Loop #%d · %s · %s\n", data.Seq, data.LoopType, data.LoopStatus); err != nil {
		return err
	}
	if data.Run != nil {
		_, err := fmt.Fprintf(w, "Run %s · step: %s\n", data.Run.RunID, formatScalar(data.Run.CurrentStep))
		return err
	}
	_, err := fmt.Fprintln(w, "Run - · step: -")
	return err
}

func writeHumanLoopLogsRunAgent(w io.Writer, data loopLogsOutput) error {
	if data.Agent == nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, "Agent: %s · pid %s · %s\n", data.Agent.Vendor, formatScalar(data.Agent.PID), data.Agent.Status); err != nil {
		return err
	}
	if data.Agent.ErrorMessage != nil && strings.TrimSpace(*data.Agent.ErrorMessage) != "" {
		if _, err := fmt.Fprintf(w, "Error: %s\n", strings.TrimSpace(*data.Agent.ErrorMessage)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func loopLogsSelectedContent(data loopLogsOutput, stderr bool) string {
	if data.Agent == nil {
		return ""
	}
	if stderr {
		return data.Agent.Stderr
	}
	if shouldDefaultLoopLogsToStderr(data) {
		return data.Agent.Stderr
	}
	return data.Agent.Stdout
}

func shouldDefaultLoopLogsToStderr(data loopLogsOutput) bool {
	if data.Agent == nil {
		return false
	}
	if strings.TrimSpace(data.Agent.Vendor) != "codex" {
		return false
	}
	return strings.TrimSpace(data.Agent.Stdout) == "" && strings.TrimSpace(data.Agent.Stderr) != ""
}

func loopLogsInitialContent(data loopLogsOutput, stderr bool, full bool, tail string) (string, error) {
	content := loopLogsSelectedContent(data, stderr)
	if full {
		return content, nil
	}
	parsed, err := parseOptionalPositiveInt(tail, "--tail")
	if err != nil {
		return "", err
	}
	count := 100
	if parsed != nil {
		count = int(*parsed)
	}
	return tailText(content, count), nil
}

func loopLogsCanContinue(data loopLogsOutput) bool {
	if data.Run == nil {
		switch data.LoopStatus {
		case "idle", "queued", "running", "paused":
			return true
		default:
			return false
		}
	}
	switch data.Run.Status {
	case "queued", "running":
		return true
	default:
		return false
	}
}

func writeLoopLogContent(w io.Writer, content string) error {
	for _, line := range strings.Split(content, "\n") {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func writeHumanStopLoop(w io.Writer, payload json.RawMessage) error {
	var data pullRequestOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode stop response: %w", err)
	}
	printSection(w, "Loop stopped", [][2]any{{"loopId", data.LoopID}, {"runId", data.RunID}, {"executionId", data.ExecutionID}, {"vendor", data.Vendor}, {"pid", data.PID}, {"stopped", data.Stopped}})
	if !data.Stopped {
		return fmt.Errorf("Loop %s could not be stopped", data.LoopID)
	}
	return nil
}

func writeHumanRunList(w io.Writer, payload json.RawMessage) error {
	var data runsListOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode runs response: %w", err)
	}

	rows := make([]tableRow, 0, len(data.Items))
	for _, run := range data.Items {
		rows = append(rows, tableRow{"id": run.ID, "loopId": run.LoopID, "status": run.Status, "currentStep": run.CurrentStep, "startedAt": run.StartedAt})
	}
	printTable(w, []string{"id", "loopId", "status", "currentStep", "startedAt"}, rows)
	return nil
}

func writeHumanWorkerCreate(w io.Writer, payload json.RawMessage) error {
	var data pullRequestOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode worker response: %w", err)
	}
	title := ""
	if data.Title != nil {
		title = *data.Title
	}
	printSection(w, "Worker started", [][2]any{{"id", data.ID}, {"title", title}, {"status", data.Status}})
	return nil
}

func writeHumanPlannerCreate(w io.Writer, payload json.RawMessage) error {
	var data pullRequestOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode planner response: %w", err)
	}
	printSection(w, "Planner started", [][2]any{{"id", data.ID}, {"issueNumber", data.IssueNumber}, {"status", data.Status}})
	return nil
}

func writeLoopSummarySection(w io.Writer, payload json.RawMessage, title string) error {
	var data loopOutput
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("decode loop response: %w", err)
	}
	printSection(w, title, [][2]any{{"id", data.ID}, {"type", data.Type}, {"status", data.Status}})
	return nil
}

func printSection(w io.Writer, title string, entries [][2]any) {
	_, _ = fmt.Fprintln(w, title)
	width := 0
	for _, entry := range entries {
		if len(fmt.Sprint(entry[0])) > width {
			width = len(fmt.Sprint(entry[0]))
		}
	}
	for _, entry := range entries {
		_, _ = fmt.Fprintf(w, "  %-*s : %s\n", width, fmt.Sprint(entry[0]), formatScalar(entry[1]))
	}
}

func printTable(w io.Writer, headers []string, rows []tableRow) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "(none)")
		return
	}

	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = len(header)
		for _, row := range rows {
			if width := len(formatScalar(row[header])); width > widths[index] {
				widths[index] = width
			}
		}
	}

	writeTableLine := func(values []string) {
		parts := make([]string, len(values))
		for index, value := range values {
			parts[index] = fmt.Sprintf("%-*s", widths[index], value)
		}
		_, _ = fmt.Fprintln(w, strings.Join(parts, "  "))
	}

	writeTableLine(headers)
	dividers := make([]string, len(headers))
	for index, width := range widths {
		dividers[index] = strings.Repeat("-", width)
	}
	_, _ = fmt.Fprintln(w, strings.Join(dividers, "  "))
	for _, row := range rows {
		values := make([]string, len(headers))
		for index, header := range headers {
			values[index] = formatScalar(row[header])
		}
		writeTableLine(values)
	}
}

func formatScalar(value any) string {
	switch typed := value.(type) {
	case nil:
		return "-"
	case *string:
		if typed == nil || strings.TrimSpace(*typed) == "" {
			return "-"
		}
		return *typed
	case *int64:
		if typed == nil {
			return "-"
		}
		return fmt.Sprintf("%d", *typed)
	case bool:
		if typed {
			return "yes"
		}
		return "no"
	case string:
		if typed == "" {
			return "-"
		}
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func formatLoopTarget(loop loopOutput) string {
	switch loop.TargetType {
	case "project", "issue":
		return formatScalar(loop.TargetID)
	default:
		return formatPullRequestRef(loop.Repo, loop.PRNumber)
	}
}

func formatPullRequestRef(repo *string, prNumber *int64) string {
	if repo == nil || strings.TrimSpace(*repo) == "" || prNumber == nil {
		return "-"
	}
	return fmt.Sprintf("%s#%d", *repo, *prNumber)
}

func agentVendor(agent *struct {
	Vendor string `json:"vendor"`
	PID    *int64 `json:"pid"`
}) any {
	if agent == nil || strings.TrimSpace(agent.Vendor) == "" {
		return nil
	}
	return agent.Vendor
}

func agentPID(agent *struct {
	Vendor string `json:"vendor"`
	PID    *int64 `json:"pid"`
}) any {
	if agent == nil {
		return nil
	}
	return agent.PID
}

func formatRelativeAge(startedAt *string) string {
	if startedAt == nil || strings.TrimSpace(*startedAt) == "" {
		return "-"
	}
	started, err := time.Parse(time.RFC3339, *startedAt)
	if err != nil {
		return "-"
	}

	diff := time.Since(started)
	if diff < 0 {
		diff = 0
	}
	minutes := int(diff / time.Minute)
	if minutes < 1 {
		return "<1m"
	}
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		remainingMinutes := minutes % 60
		if remainingMinutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, remainingMinutes)
	}
	days := hours / 24
	remainingHours := hours % 24
	if remainingHours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, remainingHours)
}

func parseOptionalPositiveInt(value string, flag string) (*int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := parsePositiveInt(trimmed, flag)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func tailText(content string, lineCount int) string {
	if lineCount <= 0 || content == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) > lineCount {
		lines = lines[len(lines)-lineCount:]
	}
	return strings.Join(lines, "\n")
}

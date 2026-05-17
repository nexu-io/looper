package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	envFakeGHMode        = "LOOPER_E2E_FAKE_GH_MODE"
	envFakeGHArtifactDir = "LOOPER_E2E_FAKE_GH_ARTIFACT_DIR"
	envFakeGHSchemaPath  = "LOOPER_E2E_FAKE_GH_SCHEMA_PATH"
	envFakeGHStatePath   = "LOOPER_E2E_FAKE_GH_STATE_PATH"
	envFakeGHRecordPath  = "LOOPER_E2E_FAKE_GH_RECORD_PATH"
	envFakeGHGitPath     = "LOOPER_E2E_FAKE_GH_GIT_PATH"
)

type schema struct {
	JSONFieldAllowlist map[string][]string `json:"jsonFieldAllowlist"`
}
type response struct {
	Stdout   json.RawMessage `json:"stdout,omitempty"`
	Stderr   string          `json:"stderr,omitempty"`
	ExitCode int             `json:"exitCode,omitempty"`
}
type state struct {
	Commands         map[string]response         `json:"commands,omitempty"`
	Routes           map[string]json.RawMessage  `json:"routes,omitempty"`
	GraphQL          map[string]json.RawMessage  `json:"graphql,omitempty"`
	CurrentUserLogin string                      `json:"currentUserLogin,omitempty"`
	PullRequests     map[string]pullRequestState `json:"pullRequests,omitempty"`
}
type pullRequestState struct {
	Number            int64               `json:"number,omitempty"`
	Repo              string              `json:"repo,omitempty"`
	Title             string              `json:"title,omitempty"`
	Body              string              `json:"body,omitempty"`
	URL               string              `json:"url,omitempty"`
	State             string              `json:"state,omitempty"`
	CreatedAt         string              `json:"createdAt,omitempty"`
	UpdatedAt         string              `json:"updatedAt,omitempty"`
	ClosedAt          string              `json:"closedAt,omitempty"`
	IsDraft           bool                `json:"isDraft,omitempty"`
	ReviewDecision    string              `json:"reviewDecision,omitempty"`
	Labels            []string            `json:"labels,omitempty"`
	HeadRefName       string              `json:"headRefName,omitempty"`
	BaseRefName       string              `json:"baseRefName,omitempty"`
	HeadRef           string              `json:"headRef,omitempty"`
	BaseRef           string              `json:"baseRef,omitempty"`
	HeadSHA           string              `json:"headSha,omitempty"`
	BaseSHA           string              `json:"baseSha,omitempty"`
	GitDir            string              `json:"gitDir,omitempty"`
	Author            string              `json:"author,omitempty"`
	AuthorAssociation string              `json:"authorAssociation,omitempty"`
	ReviewRequests    []string            `json:"reviewRequests,omitempty"`
	IssueComments     []map[string]any    `json:"issueComments,omitempty"`
	Reviews           []map[string]any    `json:"reviews,omitempty"`
	StatusCheckRollup []map[string]any    `json:"statusCheckRollup,omitempty"`
	MergeStateStatus  string              `json:"mergeStateStatus,omitempty"`
	Threads           []reviewThreadState `json:"threads,omitempty"`
}
type reviewThreadState struct {
	ID         string               `json:"id"`
	IsResolved bool                 `json:"isResolved,omitempty"`
	Path       string               `json:"path,omitempty"`
	Line       int64                `json:"line,omitempty"`
	Comments   []reviewCommentState `json:"comments,omitempty"`
}
type reviewCommentState struct {
	ID                string `json:"id"`
	Body              string `json:"body,omitempty"`
	Author            string `json:"author,omitempty"`
	CreatedAt         string `json:"createdAt,omitempty"`
	UpdatedAt         string `json:"updatedAt,omitempty"`
	Path              string `json:"path,omitempty"`
	Line              int64  `json:"line,omitempty"`
	OriginalCommitOID string `json:"originalCommitOid,omitempty"`
	CommitOID         string `json:"commitOid,omitempty"`
	URL               string `json:"url,omitempty"`
}
type invocation struct {
	Timestamp string            `json:"timestamp"`
	CWD       string            `json:"cwd"`
	Argv      []string          `json:"argv"`
	Stdin     string            `json:"stdin"`
	Env       map[string]string `json:"env"`
	Mode      string            `json:"mode"`
}

func main() {
	mode := strings.TrimSpace(os.Getenv(envFakeGHMode))
	if mode == "" {
		mode = "strict"
	}
	artifactDir := strings.TrimSpace(os.Getenv(envFakeGHArtifactDir))
	if artifactDir == "" {
		artifactDir = "."
	}
	_ = os.MkdirAll(artifactDir, 0o755)
	stdin, _ := io.ReadAll(os.Stdin)
	_ = appendJSONL(filepath.Join(artifactDir, "invocations.jsonl"), invocation{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), CWD: mustGetwd(), Argv: os.Args[1:], Stdin: string(stdin), Env: collectEnv(), Mode: mode})
	schemaDoc, err := loadSchema(strings.TrimSpace(os.Getenv(envFakeGHSchemaPath)))
	if err != nil && mode == "strict" {
		fatalf(2, "load fake-gh schema: %v\n", err)
	}
	st, err := loadState(strings.TrimSpace(os.Getenv(envFakeGHStatePath)))
	if err != nil {
		fatalf(2, "load fake-gh state: %v\n", err)
	}
	if mode == "record" {
		_ = appendJSONL(strings.TrimSpace(os.Getenv(envFakeGHRecordPath)), map[string]any{"argv": os.Args[1:], "stdin": string(stdin)})
	}
	if err := dispatch(mode, schemaDoc, st, string(stdin)); err != nil {
		fatalf(1, "%s\n", err.Error())
	}
}

func dispatch(mode string, schemaDoc schema, st state, stdin string) error {
	key := commandKey(os.Args[1:])
	if resp, ok := st.Commands[key]; ok {
		return emitResponse(resp)
	}
	if strings.HasPrefix(key, "api") {
		return handleAPI(mode, st, stdin)
	}
	switch key {
	case "auth status":
		_, _ = fmt.Fprintln(os.Stdout, "github.com\n  ✓ Logged in to github.com as "+firstNonEmpty(strings.TrimSpace(st.CurrentUserLogin), "looper"))
		return nil
	case "pr view":
		fields := requestedJSONFields(os.Args[1:])
		allowed := schemaDoc.JSONFieldAllowlist[key]
		if len(allowed) == 0 && mode == "strict" {
			return fmt.Errorf("missing fake-gh allowlist for %s", key)
		}
		if err := validateFields(key, fields, allowed); err != nil {
			return err
		}
		if payload, ok, err := buildPullRequestViewJSON(st, fields); err != nil {
			return err
		} else if ok {
			_, _ = fmt.Fprintln(os.Stdout, string(payload))
			return nil
		}
		return emitDefaultJSON(key, fields)
	case "issue list", "pr list":
		fields := requestedJSONFields(os.Args[1:])
		allowed := schemaDoc.JSONFieldAllowlist[key]
		if len(allowed) == 0 && mode == "strict" {
			return fmt.Errorf("missing fake-gh allowlist for %s", key)
		}
		if err := validateFields(key, fields, allowed); err != nil {
			return err
		}
		return emitDefaultJSON(key, fields)
	default:
		if mode == "strict" {
			return fmt.Errorf("unsupported fake-gh command: %s", strings.Join(os.Args[1:], " "))
		}
		_, _ = fmt.Fprintln(os.Stdout, "{}")
		return nil
	}
}

func handleAPI(mode string, st state, stdin string) error {
	args := os.Args[1:]
	route := firstNonFlag(args[1:])
	if route == "user" {
		login := firstNonEmpty(strings.TrimSpace(st.CurrentUserLogin), "looper")
		if hasArg(args, "--jq", ".login") {
			_, _ = fmt.Fprintln(os.Stdout, login)
			return nil
		}
		_, _ = fmt.Fprintf(os.Stdout, "{\"login\":%q}\n", login)
		return nil
	}
	if len(args) >= 2 && args[1] == "graphql" {
		if handled, err := handleGraphQLState(st, args, stdin); handled || err != nil {
			return err
		}
		operation := graphqlOperation(args, stdin)
		if payload, ok := st.GraphQL[operation]; ok {
			_, _ = os.Stdout.Write(payload)
			if len(payload) == 0 || payload[len(payload)-1] != '\n' {
				_, _ = fmt.Fprintln(os.Stdout)
			}
			return nil
		}
		_, _ = fmt.Fprintln(os.Stdout, `{"data":{}}`)
		return nil
	}
	if strings.HasSuffix(route, "/comments") && strings.EqualFold(flagValue(args, "--method"), "POST") {
		_, _ = fmt.Fprintln(os.Stdout, `{"id":1,"html_url":"https://example.test/issues/comments/1"}`)
		return nil
	}
	if payload, ok := st.Routes[route]; ok {
		_, _ = os.Stdout.Write(payload)
		if len(payload) == 0 || payload[len(payload)-1] != '\n' {
			_, _ = fmt.Fprintln(os.Stdout)
		}
		return nil
	}
	if slices.Contains(args, "--paginate") {
		_, _ = fmt.Fprintln(os.Stdout, `[]`)
		return nil
	}
	if strings.Contains(route, "/compare/") {
		if payload, ok := buildComparePayload(route); ok {
			_, _ = fmt.Fprintln(os.Stdout, payload)
			return nil
		}
	}
	if mode == "strict" && route == "" {
		return fmt.Errorf("unsupported fake-gh api invocation: %s", strings.Join(args, " "))
	}
	_, _ = fmt.Fprintln(os.Stdout, `{"id":1,"number":1,"title":"fake issue"}`)
	_ = stdin
	return nil
}

func emitResponse(resp response) error {
	if resp.Stderr != "" {
		_, _ = os.Stderr.WriteString(resp.Stderr)
	}
	if len(resp.Stdout) > 0 {
		_, _ = os.Stdout.Write(resp.Stdout)
		if resp.Stdout[len(resp.Stdout)-1] != '\n' {
			_, _ = fmt.Fprintln(os.Stdout)
		}
	}
	if resp.ExitCode != 0 {
		os.Exit(resp.ExitCode)
	}
	return nil
}

func emitDefaultJSON(key string, fields []string) error {
	object := map[string]any{}
	for _, field := range fields {
		object[field] = defaultValue(field)
	}
	var payload any = object
	if strings.HasSuffix(key, "list") {
		payload = []map[string]any{object}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, string(encoded))
	return nil
}

func validateFields(command string, fields []string, allowed []string) error {
	allow := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allow[field] = struct{}{}
	}
	for _, field := range fields {
		if _, ok := allow[field]; ok {
			continue
		}
		available := append([]string{}, allowed...)
		sort.Strings(available)
		return fmt.Errorf("unknown JSON field: %q\nAvailable fields:\n  %s\n", field, strings.Join(available, "\n  "))
	}
	_ = command
	return nil
}

func requestedJSONFields(args []string) []string {
	for index, arg := range args {
		if arg == "--json" && index+1 < len(args) {
			return splitFields(args[index+1])
		}
		if strings.HasPrefix(arg, "--json=") {
			return splitFields(strings.TrimPrefix(arg, "--json="))
		}
	}
	return nil
}

func splitFields(raw string) []string {
	parts := strings.Split(raw, ",")
	fields := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			fields = append(fields, part)
		}
	}
	return fields
}

func commandKey(args []string) string {
	parts := make([]string, 0, 2)
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if takesValue(arg) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		parts = append(parts, arg)
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func firstNonFlag(args []string) string {
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if takesValue(arg) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func graphqlOperation(args []string, stdin string) string {
	reader := strings.NewReader(strings.Join(args, " ") + " " + stdin)
	scanner := bufio.NewScanner(reader)
	scanner.Split(bufio.ScanWords)
	for scanner.Scan() {
		token := scanner.Text()
		if strings.Contains(token, "unresolveReviewThread") {
			return "unresolveReviewThread"
		}
		if strings.Contains(token, "resolveReviewThread") {
			return "resolveReviewThread"
		}
	}
	return "default"
}

func handleGraphQLState(st state, args []string, stdin string) (bool, error) {
	query := strings.Join(args, " ") + " " + stdin
	switch {
	case strings.Contains(query, "addPullRequestReviewThreadReply"):
		threadID := fieldValue(args, "threadId")
		body := fieldValue(args, "body")
		if threadID == "" {
			return false, nil
		}
		commentID, err := appendThreadReply(&st, threadID, body)
		if err != nil {
			return false, nil
		}
		if err := saveState(strings.TrimSpace(os.Getenv(envFakeGHStatePath)), st); err != nil {
			return true, err
		}
		_, _ = fmt.Fprintf(os.Stdout, "{\"data\":{\"addPullRequestReviewThreadReply\":{\"comment\":{\"id\":%q}}}}\n", commentID)
		return true, nil
	case strings.Contains(query, "unresolveReviewThread"):
		threadID := fieldValue(args, "threadId")
		if err := setThreadResolved(&st, threadID, false); err != nil {
			return false, nil
		}
		if err := saveState(strings.TrimSpace(os.Getenv(envFakeGHStatePath)), st); err != nil {
			return true, err
		}
		_, _ = fmt.Fprintf(os.Stdout, "{\"data\":{\"unresolveReviewThread\":{\"thread\":{\"id\":%q,\"isResolved\":false}}}}\n", threadID)
		return true, nil
	case strings.Contains(query, "resolveReviewThread"):
		threadID := fieldValue(args, "threadId")
		if err := setThreadResolved(&st, threadID, true); err != nil {
			return false, nil
		}
		if err := saveState(strings.TrimSpace(os.Getenv(envFakeGHStatePath)), st); err != nil {
			return true, err
		}
		_, _ = fmt.Fprintf(os.Stdout, "{\"data\":{\"resolveReviewThread\":{\"thread\":{\"id\":%q,\"isResolved\":true}}}}\n", threadID)
		return true, nil
	case strings.Contains(query, "reviewThreads("):
		repo := repoFromGraphQLArgs(args)
		prNumber, _ := strconv.ParseInt(fieldValue(args, "prNumber"), 10, 64)
		pr, ok := lookupPullRequest(st, repo, prNumber)
		if !ok {
			return false, nil
		}
		payload, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"nodes": reviewThreadNodes(pr), "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}}}}})
		if err != nil {
			return true, err
		}
		_, _ = fmt.Fprintln(os.Stdout, string(payload))
		return true, nil
	case strings.Contains(query, "PullRequestReviewThread") || strings.Contains(query, "node(id: $threadId)"):
		threadID := fieldValue(args, "threadId")
		thread, ok := lookupThread(st, threadID)
		if !ok {
			return false, nil
		}
		payload, err := json.Marshal(map[string]any{"data": map[string]any{"node": map[string]any{"id": thread.ID, "isResolved": thread.IsResolved, "comments": map[string]any{"nodes": reviewCommentNodes(thread.Comments), "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}}}})
		if err != nil {
			return true, err
		}
		_, _ = fmt.Fprintln(os.Stdout, string(payload))
		return true, nil
	default:
		return false, nil
	}
}

func buildPullRequestViewJSON(st state, fields []string) ([]byte, bool, error) {
	repo := flagValue(os.Args[1:], "--repo")
	prNumber, err := parsePRNumber(os.Args[1:])
	if err != nil {
		return nil, false, err
	}
	pr, ok := lookupPullRequest(st, repo, prNumber)
	if !ok {
		return nil, false, nil
	}
	row := map[string]any{}
	for _, field := range fields {
		row[field] = pullRequestFieldValue(pr, field)
	}
	payload, err := json.Marshal(row)
	return payload, true, err
}

func parsePRNumber(args []string) (int64, error) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if value, err := strconv.ParseInt(arg, 10, 64); err == nil {
			return value, nil
		}
	}
	return 0, fmt.Errorf("missing pull request number")
}

func lookupPullRequest(st state, repo string, prNumber int64) (pullRequestState, bool) {
	if repo != "" {
		if pr, ok := st.PullRequests[fmt.Sprintf("%s#%d", repo, prNumber)]; ok {
			return hydratePullRequest(pr), true
		}
	}
	for _, pr := range st.PullRequests {
		if pr.Number == prNumber && (repo == "" || pr.Repo == repo) {
			return hydratePullRequest(pr), true
		}
	}
	return pullRequestState{}, false
}

func hydratePullRequest(pr pullRequestState) pullRequestState {
	gitPath := firstNonEmpty(strings.TrimSpace(os.Getenv(envFakeGHGitPath)), "git")
	if pr.GitDir != "" {
		if sha := resolveGitRef(gitPath, pr.GitDir, firstNonEmpty(pr.HeadRef, pr.HeadRefName)); sha != "" {
			pr.HeadSHA = sha
		}
		if sha := resolveGitRef(gitPath, pr.GitDir, firstNonEmpty(pr.BaseRef, pr.BaseRefName)); sha != "" {
			pr.BaseSHA = sha
		}
	}
	if pr.State == "" {
		pr.State = "OPEN"
	}
	if pr.URL == "" && pr.Repo != "" && pr.Number > 0 {
		pr.URL = fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.Number)
	}
	if pr.Author == "" {
		pr.Author = "octocat"
	}
	if pr.MergeStateStatus == "" {
		pr.MergeStateStatus = "CLEAN"
	}
	if pr.UpdatedAt == "" {
		pr.UpdatedAt = "2026-05-12T00:00:00Z"
	}
	if pr.CreatedAt == "" {
		pr.CreatedAt = pr.UpdatedAt
	}
	return pr
}

func pullRequestFieldValue(pr pullRequestState, field string) any {
	switch field {
	case "number":
		return pr.Number
	case "title":
		return pr.Title
	case "body":
		return pr.Body
	case "url":
		return pr.URL
	case "state":
		return pr.State
	case "createdAt":
		return pr.CreatedAt
	case "updatedAt":
		return pr.UpdatedAt
	case "closedAt":
		return pr.ClosedAt
	case "isDraft":
		return pr.IsDraft
	case "reviewDecision":
		return pr.ReviewDecision
	case "labels":
		items := make([]map[string]any, 0, len(pr.Labels))
		for _, label := range pr.Labels {
			items = append(items, map[string]any{"name": label})
		}
		return items
	case "headRefName":
		return pr.HeadRefName
	case "baseRefName":
		return pr.BaseRefName
	case "headRefOid":
		return pr.HeadSHA
	case "baseRefOid":
		return pr.BaseSHA
	case "author":
		return map[string]any{"login": pr.Author}
	case "authorAssociation":
		return pr.AuthorAssociation
	case "reviewRequests":
		items := make([]map[string]any, 0, len(pr.ReviewRequests))
		for _, login := range pr.ReviewRequests {
			items = append(items, map[string]any{"login": login})
		}
		return items
	case "comments":
		return pr.IssueComments
	case "reviews":
		return pr.Reviews
	case "statusCheckRollup":
		return pr.StatusCheckRollup
	case "mergeStateStatus":
		return pr.MergeStateStatus
	default:
		return defaultValue(field)
	}
}

func reviewThreadNodes(pr pullRequestState) []map[string]any {
	nodes := make([]map[string]any, 0, len(pr.Threads))
	for _, thread := range pr.Threads {
		nodes = append(nodes, map[string]any{
			"id":         thread.ID,
			"isResolved": thread.IsResolved,
			"path":       thread.Path,
			"line":       thread.Line,
			"comments": map[string]any{
				"nodes":    reviewCommentNodes(thread.Comments),
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			},
		})
	}
	return nodes
}

func reviewCommentNodes(comments []reviewCommentState) []map[string]any {
	nodes := make([]map[string]any, 0, len(comments))
	for _, comment := range comments {
		nodes = append(nodes, map[string]any{
			"id":             comment.ID,
			"body":           comment.Body,
			"createdAt":      firstNonEmpty(comment.CreatedAt, "2026-05-12T00:00:00Z"),
			"updatedAt":      firstNonEmpty(comment.UpdatedAt, "2026-05-12T00:00:00Z"),
			"path":           comment.Path,
			"line":           comment.Line,
			"url":            comment.URL,
			"author":         map[string]any{"login": firstNonEmpty(comment.Author, "octocat")},
			"originalCommit": map[string]any{"oid": firstNonEmpty(comment.OriginalCommitOID, comment.CommitOID)},
			"commit":         map[string]any{"oid": firstNonEmpty(comment.CommitOID, comment.OriginalCommitOID)},
		})
	}
	return nodes
}

func lookupThread(st state, threadID string) (reviewThreadState, bool) {
	for _, pr := range st.PullRequests {
		for _, thread := range pr.Threads {
			if thread.ID == threadID {
				return thread, true
			}
		}
	}
	return reviewThreadState{}, false
}

func setThreadResolved(st *state, threadID string, resolved bool) error {
	for key, pr := range st.PullRequests {
		for index, thread := range pr.Threads {
			if thread.ID != threadID {
				continue
			}
			pr.Threads[index].IsResolved = resolved
			st.PullRequests[key] = pr
			return nil
		}
	}
	return fmt.Errorf("review thread not found: %s", threadID)
}

func appendThreadReply(st *state, threadID, body string) (string, error) {
	for key, pr := range st.PullRequests {
		for index, thread := range pr.Threads {
			if thread.ID != threadID {
				continue
			}
			commentID := fmt.Sprintf("reply-%d", len(thread.Comments)+1)
			thread.Comments = append(thread.Comments, reviewCommentState{ID: commentID, Body: body, Author: firstNonEmpty(st.CurrentUserLogin, "looper"), CreatedAt: "2026-05-12T00:00:00Z", UpdatedAt: "2026-05-12T00:00:00Z", Path: thread.Path, Line: thread.Line, URL: fmt.Sprintf("https://example.test/threads/%s#%s", thread.ID, commentID)})
			pr.Threads[index] = thread
			st.PullRequests[key] = pr
			return commentID, nil
		}
	}
	return "", fmt.Errorf("review thread not found: %s", threadID)
}

func saveState(path string, st state) error {
	if path == "" {
		return nil
	}
	payload, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, payload, 0o644)
}

func fieldValue(args []string, key string) string {
	prefix := key + "="
	for index, arg := range args {
		if (arg == "-F" || arg == "-f") && index+1 < len(args) && strings.HasPrefix(args[index+1], prefix) {
			return strings.TrimPrefix(args[index+1], prefix)
		}
		if strings.HasPrefix(arg, "-F") && strings.HasPrefix(strings.TrimPrefix(arg, "-F"), prefix) {
			return strings.TrimPrefix(strings.TrimPrefix(arg, "-F"), prefix)
		}
	}
	return ""
}

func repoFromGraphQLArgs(args []string) string {
	owner := fieldValue(args, "owner")
	name := fieldValue(args, "name")
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

func resolveGitRef(gitPath, gitDir, ref string) string {
	if gitDir == "" || ref == "" {
		return ""
	}
	output, err := exec.Command(gitPath, "--git-dir", gitDir, "rev-parse", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func buildComparePayload(route string) (string, bool) {
	marker := "/compare/"
	index := strings.Index(route, marker)
	if index < 0 {
		return "", false
	}
	comparison := route[index+len(marker):]
	parts := strings.SplitN(comparison, "...", 2)
	if len(parts) != 2 {
		return "", false
	}
	base := parts[0]
	head := parts[1]
	gitPath := firstNonEmpty(strings.TrimSpace(os.Getenv(envFakeGHGitPath)), "git")
	counts, err := exec.Command(gitPath, "rev-list", "--left-right", "--count", base+"..."+head).Output()
	if err != nil {
		return `{"ahead_by":0,"behind_by":0,"status":"identical","total_commits":0}`, true
	}
	fields := strings.Fields(strings.TrimSpace(string(counts)))
	if len(fields) != 2 {
		return `{"ahead_by":0,"behind_by":0,"status":"identical","total_commits":0}`, true
	}
	behind, _ := strconv.Atoi(fields[0])
	ahead, _ := strconv.Atoi(fields[1])
	status := "identical"
	switch {
	case ahead > 0 && behind > 0:
		status = "diverged"
	case ahead > 0:
		status = "ahead"
	case behind > 0:
		status = "behind"
	}
	return fmt.Sprintf(`{"ahead_by":%d,"behind_by":%d,"status":%q,"total_commits":%d}`, ahead, behind, status, ahead+behind), true
}

func flagValue(args []string, name string) string {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1]
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
	}
	return ""
}

func hasArg(args []string, flag string, value string) bool {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) && args[index+1] == value {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func takesValue(flag string) bool {
	if strings.Contains(flag, "=") {
		return false
	}
	switch flag {
	case "-X", "--method", "-f", "-F", "--field", "--raw-field", "-H", "--header", "--hostname", "--repo", "--json", "--jq", "--template", "--input":
		return true
	default:
		return false
	}
}

func defaultValue(field string) any {
	switch field {
	case "number":
		return 1
	case "title":
		return "fake title"
	case "state":
		return "OPEN"
	case "url":
		return "https://example.test/owner/repo/pull/1"
	case "id":
		return "FAKE_node_id"
	case "body":
		return ""
	case "headRefName":
		return "fake-branch"
	case "headRefOid":
		return "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	case "baseRefOid":
		return "basebeefdeadbeefdeadbeefdeadbeefdeadbeef"
	case "author":
		return map[string]any{"login": "octocat"}
	case "reviewRequests", "reviews", "comments", "statusCheckRollup", "labels", "assignees":
		return []any{}
	case "authorAssociation":
		return "NONE"
	case "updatedAt":
		return "2026-05-12T00:00:00Z"
	case "createdAt":
		return "2026-05-12T00:00:00Z"
	case "closedAt":
		return ""
	case "isDraft":
		return false
	case "reviewDecision":
		return ""
	case "mergeStateStatus":
		return "CLEAN"
	default:
		return field
	}
}

func loadSchema(path string) (schema, error) {
	if path == "" {
		return schema{}, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return schema{}, err
	}
	var decoded schema
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return schema{}, err
	}
	if decoded.JSONFieldAllowlist == nil {
		decoded.JSONFieldAllowlist = map[string][]string{}
	}
	return decoded, nil
}

func loadState(path string) (state, error) {
	if path == "" {
		return state{}, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state{}, nil
		}
		return state{}, err
	}
	var decoded state
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return state{}, err
	}
	if decoded.Commands == nil {
		decoded.Commands = map[string]response{}
	}
	if decoded.Routes == nil {
		decoded.Routes = map[string]json.RawMessage{}
	}
	if decoded.GraphQL == nil {
		decoded.GraphQL = map[string]json.RawMessage{}
	}
	if decoded.PullRequests == nil {
		decoded.PullRequests = map[string]pullRequestState{}
	}
	return decoded, nil
}

func collectEnv() map[string]string {
	keys := []string{envFakeGHMode, envFakeGHArtifactDir, envFakeGHSchemaPath, envFakeGHStatePath, envFakeGHRecordPath, envFakeGHGitPath, "HOME"}
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			result[key] = value
		}
	}
	return result
}

func appendJSONL(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = file.Write(append(encoded, '\n'))
	return err
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func fatalf(code int, format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(code)
}

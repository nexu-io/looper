package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	completionMarkerEnv     = "LOOPER_COMPLETION_MARKER"
	envLooperPrompt         = "LOOPER_PROMPT"
	envFakeAgentMode        = "LOOPER_E2E_FAKE_AGENT_MODE"
	envFakeAgentArtifactDir = "LOOPER_E2E_FAKE_AGENT_ARTIFACT_DIR"
	envFakeAgentStatePath   = "LOOPER_E2E_FAKE_AGENT_STATE_PATH"
	envFakeAgentWriteFile   = "LOOPER_E2E_FAKE_AGENT_WRITE_FILE"
	envFakeAgentModifyFile  = "LOOPER_E2E_FAKE_AGENT_MODIFY_FILE"
	envFakeAgentSleepMS     = "LOOPER_E2E_FAKE_AGENT_SLEEP_MS"
	envFakeAgentGitPath     = "LOOPER_E2E_FAKE_AGENT_GIT_PATH"
	envFakeAgentGHPath      = "LOOPER_E2E_FAKE_AGENT_GH_PATH"
	defaultCompletionMarker = "__LOOPER_RESULT__="
)

type promptFixItem struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type ghThreadCommentsResponse struct {
	Data struct {
		Node struct {
			Comments struct {
				Nodes    []ghThreadComment `json:"nodes"`
				PageInfo struct {
					EndCursor   string `json:"endCursor"`
					HasNextPage bool   `json:"hasNextPage"`
				} `json:"pageInfo"`
			} `json:"comments"`
		} `json:"node"`
	} `json:"data"`
}

type ghThreadComment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	UpdatedAt string `json:"updatedAt"`
}

type evidence struct {
	CWD       string            `json:"cwd"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Timestamp string            `json:"timestamp"`
	Mode      string            `json:"mode"`
	PID       int               `json:"pid"`
}

func main() {
	mode := strings.TrimSpace(os.Getenv(envFakeAgentMode))
	if mode == "" {
		mode = "success-no-diff"
	}
	artifactDir := strings.TrimSpace(os.Getenv(envFakeAgentArtifactDir))
	if artifactDir == "" {
		artifactDir = "."
	}
	_ = os.MkdirAll(artifactDir, 0o755)
	_ = writeEvidence(artifactDir, mode)
	if sleepMS, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(envFakeAgentSleepMS))); sleepMS > 0 {
		time.Sleep(time.Duration(sleepMS) * time.Millisecond)
	}
	marker := strings.TrimSpace(os.Getenv(completionMarkerEnv))
	if marker == "" {
		marker = defaultCompletionMarker
	}
	switch mode {
	case "success-with-diff", "write-file":
		path := envOr(envFakeAgentWriteFile, "agent-output.txt")
		mustWriteFile(path, []byte("changed by fake agent\n"))
		printCompletion(marker, map[string]any{"summary": "fake agent wrote file", "changedFiles": []string{path}})
	case "success-no-diff":
		printCompletion(marker, map[string]any{"summary": "fake agent no diff"})
	case "forgejo-reviewer-open":
		printCompletion(marker, map[string]any{
			"summary": "Found actionable Forgejo review items",
			"outcome": "non_blocking",
			"findings": []map[string]any{{
				"title": "Repair sandbox target file",
				"body":  "The sandbox target file still needs the fake-agent repair applied.",
				"files": []string{"sandbox/forgejo-summary-protocol.txt"},
			}},
		})
	case "forgejo-reviewer-clean":
		printCompletion(marker, map[string]any{
			"summary":  "No actionable findings remain after the Forgejo fixer round",
			"outcome":  "clean",
			"findings": []map[string]any{},
		})
	case "modify-file":
		path := envOr(envFakeAgentModifyFile, "README.md")
		mustAppendFile(path, []byte("modified by fake agent\n"))
		printCompletion(marker, map[string]any{"summary": "fake agent modified file", "changedFiles": []string{path}})
	case "commit":
		path := envOr(envFakeAgentWriteFile, "agent-commit.txt")
		mustWriteFile(path, []byte("commit from fake agent\n"))
		gitPath := envOr(envFakeAgentGitPath, "git")
		mustRun(gitPath, "add", path)
		mustRun(gitPath, "commit", "-m", "fake agent commit")
		sha := strings.TrimSpace(mustOutput(gitPath, "rev-parse", "HEAD"))
		payload := map[string]any{"summary": "fake agent committed changes", "changedFiles": []string{path}, "commits": []string{sha}}
		if replies := promptReviewThreadReplies("Updated fix-target.txt to address the review feedback.", false, true); len(replies) > 0 {
			payload["review_thread_replies"] = replies
		}
		printCompletion(marker, payload)
	case "commit-with-review-replies":
		path := envOr(envFakeAgentWriteFile, "fix-target.txt")
		mustWriteFile(path, []byte("fixed by fake agent\n"))
		gitPath := envOr(envFakeAgentGitPath, "git")
		mustRun(gitPath, "add", path)
		mustRun(gitPath, "commit", "-m", "fake agent commit")
		sha := strings.TrimSpace(mustOutput(gitPath, "rev-parse", "HEAD"))
		replies := promptReviewThreadReplies("Updated fix-target.txt to address the review feedback.", true, true)
		printCompletion(marker, map[string]any{
			"summary":               "fake agent committed changes",
			"changedFiles":          []string{path},
			"commits":               []string{sha},
			"review_thread_replies": replies,
		})
	case "transient-failure":
		statePath := strings.TrimSpace(os.Getenv(envFakeAgentStatePath))
		if firstRun(statePath) {
			_, _ = fmt.Fprintln(os.Stderr, "transient fake-agent failure")
			os.Exit(1)
		}
		printCompletion(marker, map[string]any{"summary": "fake agent recovered"})
	case "malformed-marker":
		_, _ = fmt.Printf("%s{bad json}\n", marker)
	case "timeout", "no-marker":
		_, _ = fmt.Fprintln(os.Stdout, "fake agent finished without completion marker")
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unsupported fake-agent mode %q\n", mode)
		os.Exit(2)
	}
}

func writeEvidence(dir string, mode string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	data := evidence{CWD: cwd, Args: os.Args[1:], Env: collectEnv(), Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Mode: mode, PID: os.Getpid()}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cwd-evidence.json"), payload, 0o644)
}

func collectEnv() map[string]string {
	keys := []string{"HOME", completionMarkerEnv, envFakeAgentMode, envFakeAgentArtifactDir, envFakeAgentStatePath, envFakeAgentWriteFile, envFakeAgentModifyFile, envFakeAgentGitPath, envFakeAgentGHPath}
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			result[key] = value
		}
	}
	return result
}

func hashCommentIDs(ids ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, "|")))
	return hex.EncodeToString(sum[:])
}

func hashObservedThreadComments(comments []ghThreadComment) string {
	type observedThreadComment struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updatedAt,omitempty"`
	}
	observed := make([]observedThreadComment, 0, len(comments))
	for _, comment := range comments {
		body := strings.TrimSpace(comment.Body)
		if body == "" || strings.Contains(body, "looper-fixer-reply") || strings.Contains(body, "looper:fixer-round") || strings.Contains(body, "<!-- looper:stamp v=1 -->") {
			continue
		}
		id := strings.TrimSpace(comment.ID)
		if id == "" {
			continue
		}
		observed = append(observed, observedThreadComment{ID: id, UpdatedAt: strings.TrimSpace(comment.UpdatedAt)})
	}
	payload, err := json.Marshal(observed)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func envOr(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func mustWriteFile(path string, content []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		panic(err)
	}
}

func mustAppendFile(path string, content []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	if _, err := file.Write(content); err != nil {
		panic(err)
	}
}

func mustRun(command string, args ...string) {
	cmd := exec.Command(command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic(err)
	}
}

func mustOutput(command string, args ...string) string {
	cmd := exec.Command(command, args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			panic(fmt.Errorf("%s %v: %w\nstderr: %s", command, args, err, strings.TrimSpace(string(exitErr.Stderr))))
		}
		panic(fmt.Errorf("%s %v: %w", command, args, err))
	}
	return string(output)
}

func firstRun(statePath string) bool {
	if strings.TrimSpace(statePath) == "" {
		return true
	}
	if _, err := os.Stat(statePath); err == nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(statePath, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
		panic(err)
	}
	return true
}

func printCompletion(marker string, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	_, _ = fmt.Printf("%s%s\n", marker, string(encoded))
}

func parsePromptFixItems(prompt string) []promptFixItem {
	lines := strings.Split(prompt, "\n")
	items := make([]promptFixItem, 0)
	seen := map[string]struct{}{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- {") {
			continue
		}
		var item promptFixItem
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "- ")), &item); err != nil {
			continue
		}
		if strings.TrimSpace(item.Type) != "comment" || strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ThreadID) == "" {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		items = append(items, item)
	}
	return items
}

func fetchObservedThreadHashes(items []promptFixItem) map[string]string {
	threadHashes := make(map[string]string, len(items))
	ghPath := strings.TrimSpace(os.Getenv(envFakeAgentGHPath))
	if ghPath == "" {
		return threadHashes
	}
	for _, item := range items {
		threadID := strings.TrimSpace(item.ThreadID)
		if threadID == "" {
			continue
		}
		if _, ok := threadHashes[threadID]; ok {
			continue
		}
		comments := make([]ghThreadComment, 0, 8)
		cursor := ""
		for {
			query := strings.Join([]string{
				"query($threadId: ID!, $after: String) {",
				"  node(id: $threadId) {",
				"    ... on PullRequestReviewThread {",
				"      comments(first: 100, after: $after) {",
				"        nodes { id body updatedAt }",
				"        pageInfo { hasNextPage endCursor }",
				"      }",
				"    }",
				"  }",
				"}",
			}, "\n")
			args := []string{"api", "graphql", "-f", "query=" + query, "-F", "threadId=" + threadID}
			if cursor != "" {
				args = append(args, "-F", "after="+cursor)
			}
			output := mustOutput(ghPath, args...)
			var response ghThreadCommentsResponse
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				panic(err)
			}
			comments = append(comments, response.Data.Node.Comments.Nodes...)
			if !response.Data.Node.Comments.PageInfo.HasNextPage || strings.TrimSpace(response.Data.Node.Comments.PageInfo.EndCursor) == "" {
				break
			}
			cursor = response.Data.Node.Comments.PageInfo.EndCursor
		}
		threadHashes[threadID] = hashObservedThreadComments(comments)
	}
	return threadHashes
}

func buildReviewThreadReplies(items []promptFixItem, explanation string, fallback bool, observedByThread map[string]string) []map[string]any {
	replies := make([]map[string]any, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		reply := map[string]any{
			"fixItemId":   item.ID,
			"threadId":    item.ThreadID,
			"explanation": explanation,
		}
		if observed := strings.TrimSpace(observedByThread[item.ThreadID]); observed != "" {
			reply["threadCommentsObserved"] = observed
		}
		replies = append(replies, reply)
	}
	if len(replies) == 0 && fallback {
		reply := map[string]any{
			"fixItemId":   "comment-1",
			"threadId":    "thread-1",
			"explanation": explanation,
		}
		if observed := strings.TrimSpace(observedByThread["thread-1"]); observed != "" {
			reply["threadCommentsObserved"] = observed
		} else if len(observedByThread) > 0 {
			reply["threadCommentsObserved"] = hashCommentIDs("comment-1")
		}
		return []map[string]any{reply}
	}
	return replies
}

func promptReviewThreadReplies(explanation string, fallback bool, includeObserved bool) []map[string]any {
	items := parsePromptFixItems(os.Getenv(envLooperPrompt))
	observedByThread := map[string]string(nil)
	if includeObserved {
		observedByThread = fetchObservedThreadHashes(items)
	}
	return buildReviewThreadReplies(items, explanation, fallback, observedByThread)
}

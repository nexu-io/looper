package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/e2e/harness"
	"github.com/nexu-io/looper/internal/storage"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

type apiClient struct {
	baseURL string
	client  *http.Client
}

func newAPIClient(baseURL string) apiClient {
	return apiClient{baseURL: baseURL, client: &http.Client{Timeout: 2 * time.Second}}
}

func (c apiClient) get(tb testing.TB, path string, target any) {
	tb.Helper()
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		tb.Fatalf("build GET %s: %v", path, err)
	}
	c.do(tb, req, target)
}

func (c apiClient) post(tb testing.TB, path string, body any, target any) {
	tb.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			tb.Fatalf("marshal POST %s: %v", path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, payload)
	if err != nil {
		tb.Fatalf("build POST %s: %v", path, err)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	c.do(tb, req, target)
}

func (c apiClient) do(tb testing.TB, req *http.Request, target any) {
	tb.Helper()
	resp, err := c.client.Do(req)
	if err != nil {
		tb.Fatalf("%s %s: %v", req.Method, req.URL.String(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		tb.Fatalf("read %s %s: %v", req.Method, req.URL.String(), err)
	}
	if resp.StatusCode != http.StatusOK {
		tb.Fatalf("%s %s status=%d body=%s", req.Method, req.URL.String(), resp.StatusCode, string(body))
	}
	if target == nil {
		return
	}
	var envelope pkgapi.Envelope[json.RawMessage]
	if err := json.Unmarshal(body, &envelope); err != nil {
		tb.Fatalf("decode envelope %s %s: %v\nbody=%s", req.Method, req.URL.String(), err, string(body))
	}
	if !envelope.OK || envelope.Data == nil {
		tb.Fatalf("unexpected error envelope %s %s: %s", req.Method, req.URL.String(), string(body))
	}
	if err := json.Unmarshal(*envelope.Data, target); err != nil {
		tb.Fatalf("decode payload %s %s: %v\nbody=%s", req.Method, req.URL.String(), err, string(body))
	}
}

type loopsListResponse struct {
	Items []struct {
		ID           string  `json:"id"`
		Status       string  `json:"status"`
		ProjectID    string  `json:"projectId"`
		MetadataJSON *string `json:"metadataJson"`
	} `json:"items"`
}

type runsListResponse struct {
	Items []struct {
		ID             string  `json:"id"`
		LoopID         string  `json:"loopId"`
		Status         string  `json:"status"`
		CheckpointJSON *string `json:"checkpointJson"`
		ErrorMessage   *string `json:"errorMessage"`
	} `json:"items"`
}

type runView struct {
	ID             string
	LoopID         string
	Status         string
	CheckpointJSON *string
	ErrorMessage   *string
}

type loopView struct {
	ID           string
	Status       string
	ProjectID    string
	MetadataJSON *string
}

func waitForRunTerminal(tb testing.TB, client apiClient, loopID string, timeout time.Duration) runView {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var runs runsListResponse
		client.get(tb, "/api/v1/runs?loopId="+loopID, &runs)
		if len(runs.Items) > 0 {
			last := runs.Items[len(runs.Items)-1]
			switch last.Status {
			case "success", "failed", "cancelled", "stopped":
				return runView(last)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatalf("timed out waiting for loop %s terminal run", loopID)
	panic("unreachable")
}

func loadSingleLoop(tb testing.TB, client apiClient, loopID string) loopView {
	tb.Helper()
	var loops loopsListResponse
	client.get(tb, "/api/v1/loops", &loops)
	for _, loop := range loops.Items {
		if loop.ID == loopID {
			return loopView(loop)
		}
	}
	tb.Fatalf("loop %s not found", loopID)
	panic("unreachable")
}

func openRepos(tb testing.TB, dbPath string) (*sql.DB, *storage.Repositories) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := storage.OpenSQLiteDB(ctx, dbPath)
	if err != nil {
		tb.Fatalf("open sqlite db: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return db, storage.NewRepositories(db)
}

func parseJSONObject(tb testing.TB, raw *string) map[string]any {
	tb.Helper()
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(*raw), &decoded); err != nil {
		tb.Fatalf("decode json object: %v\njson=%s", err, *raw)
	}
	return decoded
}

func writeProjectConfig(repo harness.SeededRepo, home harness.TempHome) []config.ProjectRefConfig {
	worktreeRoot := filepath.Join(home.WorktreeRoot, "projects", "project_1")
	return []config.ProjectRefConfig{{
		ID:           "project_1",
		Name:         "Looper E2E",
		RepoPath:     repo.Path,
		BaseBranch:   stringPtr(repo.DefaultBranch),
		WorktreeRoot: stringPtr(worktreeRoot),
	}}
}

func stringPtr(value string) *string { return &value }

func requirePathExists(tb testing.TB, path string) {
	tb.Helper()
	if _, err := os.Stat(path); err != nil {
		tb.Fatalf("expected path %s to exist: %v", path, err)
	}
}

func requireFileContains(tb testing.TB, path string, substring string) {
	tb.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(payload), substring) {
		tb.Fatalf("file %s missing %q\ncontent=%s", path, substring, string(payload))
	}
}

func mustMarshal(tb testing.TB, value any) string {
	tb.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		tb.Fatalf("marshal json: %v", err)
	}
	return string(payload)
}

func mustQueryString(tb testing.TB, db *sql.DB, query string, args ...any) string {
	tb.Helper()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		tb.Fatalf("query %q: %v", query, err)
	}
	return value
}

func mustExec(tb testing.TB, db *sql.DB, query string, args ...any) {
	tb.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		tb.Fatalf("exec %q: %v", query, err)
	}
}

func waitForCondition(tb testing.TB, timeout time.Duration, fn func() (bool, string)) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ok, msg := fn()
		if ok {
			return
		}
		last = msg
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatalf("condition not met: %s", last)
}

func loopLogsPath(home harness.TempHome) string {
	return filepath.Join(home.LogDir, "looperd.log")
}

func configWithFakeTools(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, repo harness.SeededRepo, fakeGH harness.FakeGH, fakeAgent harness.FakeAgent, port int) config.Config {
	tb.Helper()
	vendor, command, agentEnv := fakeAgent.AgentConfig("write-file", bins.LooperPath)
	cfg := harness.DefaultConfig(tb, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: fakeGH.Path, Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyManual
	cfg.Defaults.AllowAutoPush = false
	return cfg
}

func readInvocationLog(tb testing.TB, path string) []map[string]any {
	tb.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			tb.Fatalf("decode invocation line %q: %v", line, err)
		}
		out = append(out, item)
	}
	return out
}

func firstLoopID(tb testing.TB, client apiClient) string {
	tb.Helper()
	var loops loopsListResponse
	client.get(tb, "/api/v1/loops", &loops)
	if len(loops.Items) == 0 {
		tb.Fatal("expected at least one loop")
	}
	return loops.Items[0].ID
}

func formatPathError(path string, err error) string {
	return fmt.Sprintf("%s: %v", path, err)
}

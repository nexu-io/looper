package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFakeGHValidatesJSONFieldsAndLogsInvocations(t *testing.T) {
	bins := MustBinaries(t)
	gh := NewFakeGH(t, bins, GHSchema{JSONFieldAllowlist: map[string][]string{"pr list": {"number", "title"}}})
	cmd := exec.Command(gh.Path, "pr", "list", "--json", "number,title")
	cmd.Env = append(os.Environ(), flattenEnv(gh.EnvMap())...)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run fake gh: %v", err)
	}
	if !strings.Contains(string(output), "fake title") {
		t.Fatalf("fake gh output = %q, want fixture payload", string(output))
	}
	content, err := os.ReadFile(gh.InvocationLog)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	if !strings.Contains(string(content), `"argv":["pr","list","--json","number,title"]`) {
		t.Fatalf("invocation log = %q, want argv", string(content))
	}
	cmd = exec.Command(gh.Path, "pr", "list", "--json", "number,authorAssociation")
	cmd.Env = append(os.Environ(), flattenEnv(gh.EnvMap())...)
	output, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected unsupported field failure")
	}
	if !strings.Contains(string(output), `unknown JSON field: "authorAssociation"`) {
		t.Fatalf("fake gh error = %q, want unsupported field message", string(output))
	}
}

func TestFakeGHPaginatedAPIDefaultsToEmptyArray(t *testing.T) {
	bins := MustBinaries(t)
	gh := NewFakeGH(t, bins, GHSchema{JSONFieldAllowlist: map[string][]string{}})
	cmd := exec.Command(gh.Path, "api", "--paginate", "--slurp", "repos/acme/looper/issues/77/comments")
	cmd.Env = append(os.Environ(), flattenEnv(gh.EnvMap())...)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run fake gh paginated api: %v", err)
	}
	if strings.TrimSpace(string(output)) != "[]" {
		t.Fatalf("fake gh paginated output = %q, want empty array", string(output))
	}
}

func TestFakeGHPRViewSupportsCreatedAtAndClosedAt(t *testing.T) {
	bins := MustBinaries(t)
	gh := NewFakeGH(t, bins, GHSchema{JSONFieldAllowlist: map[string][]string{"pr view": {"number", "createdAt", "closedAt"}}})
	gh.WriteState(t, GHState{PullRequests: map[string]GHPullRequest{
		"acme/looper#42": {Number: 42, Repo: "acme/looper", CreatedAt: "2026-05-01T00:00:00Z", ClosedAt: "2026-05-03T00:00:00Z"},
	}})
	cmd := exec.Command(gh.Path, "pr", "view", "42", "--repo", "acme/looper", "--json", "number,createdAt,closedAt")
	cmd.Env = append(os.Environ(), flattenEnv(gh.EnvMap())...)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run fake gh pr view: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("decode fake gh output: %v", err)
	}
	if got["createdAt"] != "2026-05-01T00:00:00Z" {
		t.Fatalf("createdAt = %v, want seeded value", got["createdAt"])
	}
	if got["closedAt"] != "2026-05-03T00:00:00Z" {
		t.Fatalf("closedAt = %v, want seeded value", got["closedAt"])
	}
}

func TestFakeGHMergeClosesLinkedIssueRouteForEnterpriseRepo(t *testing.T) {
	bins := MustBinaries(t)
	gh := NewFakeGH(t, bins, GHSchema{JSONFieldAllowlist: map[string][]string{}})
	gh.WriteState(t, GHState{
		Routes: map[string]any{
			"repos/acme/looper/issues/1": json.RawMessage(`{"number":1,"state":"open","state_reason":""}`),
		},
		PullRequests: map[string]GHPullRequest{
			"github.example.com/acme/looper#42": {
				Number:      42,
				Repo:        "github.example.com/acme/looper",
				Body:        "Implements feature.\n\nCloses #1",
				State:       "OPEN",
				HeadSHA:     "abc123",
				BaseSHA:     "base123",
				HeadRefName: "feature/worker-pr",
				BaseRefName: "main",
			},
		},
	})
	cmd := exec.Command(gh.Path, "pr", "merge", "42", "--repo", "github.example.com/acme/looper", "--squash")
	cmd.Env = append(os.Environ(), flattenEnv(gh.EnvMap())...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run fake gh merge: %v\n%s", err, string(output))
	}
	payload, err := os.ReadFile(gh.StatePath)
	if err != nil {
		t.Fatalf("read fake gh state: %v", err)
	}
	var state GHState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatalf("decode fake gh state: %v", err)
	}
	route, ok := state.Routes["repos/acme/looper/issues/1"]
	if !ok {
		t.Fatal("missing normalized issue route after merge")
	}
	routeJSON, err := json.Marshal(route)
	if err != nil {
		t.Fatalf("marshal issue route: %v", err)
	}
	if !strings.Contains(string(routeJSON), `"state":"closed"`) {
		t.Fatalf("issue route = %s, want closed state", string(routeJSON))
	}
	if !strings.Contains(string(routeJSON), `"state_reason":"completed"`) {
		t.Fatalf("issue route = %s, want completed state_reason", string(routeJSON))
	}
	pr := state.PullRequests["github.example.com/acme/looper#42"]
	if pr.State != "MERGED" {
		t.Fatalf("pull request state = %q, want MERGED", pr.State)
	}
	if pr.MergedAt == "" {
		t.Fatal("pull request mergedAt is empty, want merge timestamp")
	}
	if pr.ClosedAt == "" {
		t.Fatal("pull request closedAt is empty, want close timestamp")
	}
}

func flattenEnv(env map[string]string) []string {
	items := make([]string, 0, len(env))
	for key, value := range env {
		items = append(items, key+"="+value)
	}
	return items
}

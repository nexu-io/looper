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

func flattenEnv(env map[string]string) []string {
	items := make([]string, 0, len(env))
	for key, value := range env {
		items = append(items, key+"="+value)
	}
	return items
}

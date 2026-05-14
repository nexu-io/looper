package harness

import (
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

func flattenEnv(env map[string]string) []string {
	items := make([]string, 0, len(env))
	for key, value := range env {
		items = append(items, key+"="+value)
	}
	return items
}

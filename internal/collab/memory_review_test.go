package collab

import (
	"context"
	"encoding/json"
	"github.com/nexu-io/looper/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentReviewHasNoBusinessToolsOrCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PLANE_API_KEY", "must-not-inherit")
	t.Setenv("FEISHU_APP_SECRET", "must-not-inherit")
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "auth.json"), []byte(`{"fake":"auth"}`), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env python3
import os,sys,pathlib,json
assert 'PLANE_API_KEY' not in os.environ and 'FEISHU_APP_SECRET' not in os.environ
assert '--ephemeral' in sys.argv and '--ignore-user-config' in sys.argv
assert sys.argv[sys.argv.index('--sandbox')+1] == 'read-only'
assert 'shell_tool' in sys.argv and 'plugins' in sys.argv
assert not (pathlib.Path(os.environ['CODEX_HOME'])/'config.toml').exists()
evidence=sys.stdin.read()
assert 'Task evidence' in evidence
pathlib.Path(sys.argv[sys.argv.index('--output-last-message')+1]).write_text(json.dumps({'summary':'No reusable fact proven','proposals':[]}))
print(json.dumps({'type':'turn.completed','usage':{'input_tokens':20,'output_tokens':10}}))
`
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := reviewMemory(context.Background(), json.RawMessage(`{"summary":"Task evidence"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result["host_usage"] == nil {
		t.Fatal("actual usage discarded")
	}
}

func TestIndependentMemoryReviewReal(t *testing.T) {
	if os.Getenv("LOOPER_TEST_MEMORY_MODEL") != "1" {
		t.Skip("explicit real-model smoke only")
	}
	result, err := reviewMemory(context.Background(), json.RawMessage(`{"state":"completed","goal":"Verify a repository map","summary":"Inspected git remote and README; chose two repositories because frontend and API contract both change.","limitations":"No code changes or tests were executed."}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result["proposals"].([]any); !ok {
		t.Fatal("missing structured proposals")
	}
	t.Logf("independent review returned %d candidates; usage available=%t", len(result["proposals"].([]any)), result["host_usage"] != nil)
}

func TestMemoryEvidenceRedactsConfiguredSecretWithoutBreakingJSON(t *testing.T) {
	selector := "MEMORY_TEST_REDACTION_KEY"
	secret := `sensitive"value\with-dollar$`
	t.Setenv(selector, secret)
	h := &bridgeHost{cfg: config.Config{Providers: []config.ProviderConfig{{TokenEnv: &selector}}}}
	raw, _ := json.Marshal(map[string]string{"summary": "literal " + secret + " suffix", "goal": "Keep the useful finding"})
	out := h.redactMemoryEvidence(raw)
	var result map[string]string
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result["summary"], secret) || result["goal"] != "Keep the useful finding" {
		t.Fatalf("invalid redaction: %s", out)
	}
}

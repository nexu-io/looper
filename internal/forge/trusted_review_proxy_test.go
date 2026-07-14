package forge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTrustedReviewProxyArgv(t *testing.T) {
	t.Parallel()
	const allowed = "acme/looper#1"
	tests := []struct {
		name    string
		argv    []string
		allowed string
		wantErr bool
	}{
		{name: "submit", argv: []string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, allowed: allowed, wantErr: false},
		{name: "case-insensitive repo", argv: []string{"review", "submit", "Acme/Looper#1", "--event", "COMMENT"}, allowed: allowed, wantErr: false},
		{name: "harmless global flags then submit", argv: []string{"--json", "review", "submit", "acme/looper#1"}, allowed: allowed, wantErr: false},
		{name: "flags before PR target", argv: []string{"review", "submit", "--event", "COMMENT", "acme/looper#1"}, allowed: allowed, wantErr: false},
		{name: "reject other PR", argv: []string{"review", "submit", "acme/looper#99", "--event", "COMMENT"}, allowed: allowed, wantErr: true},
		{name: "reject other repo", argv: []string{"review", "submit", "evil/other#1", "--event", "COMMENT"}, allowed: allowed, wantErr: true},
		{name: "reject missing PR", argv: []string{"review", "submit", "--event", "COMMENT"}, allowed: allowed, wantErr: true},
		{name: "reject config override", argv: []string{"--config", "/tmp/cfg.json", "review", "submit", "acme/looper#1"}, allowed: allowed, wantErr: true},
		{name: "reject config equals form", argv: []string{"--config=/tmp/cfg.json", "review", "submit", "acme/looper#1"}, allowed: allowed, wantErr: true},
		{name: "reject config after submit", argv: []string{"review", "submit", "acme/looper#1", "--config", "/tmp/cfg.json"}, allowed: allowed, wantErr: true},
		{name: "reject db-path override", argv: []string{"--db-path", "/tmp/evil.sqlite", "review", "submit", "acme/looper#1"}, allowed: allowed, wantErr: true},
		{name: "reject looper-path override", argv: []string{"--looper-path", "/tmp/evil", "review", "submit", "acme/looper#1"}, allowed: allowed, wantErr: true},
		{name: "reject status", argv: []string{"status"}, allowed: allowed, wantErr: true},
		{name: "reject review without submit", argv: []string{"review", "repair"}, allowed: allowed, wantErr: true},
		{name: "reject empty", argv: nil, allowed: allowed, wantErr: true},
		{name: "reject empty allowed binding", argv: []string{"review", "submit", "acme/looper#1"}, allowed: "", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTrustedReviewProxyArgv(test.argv, test.allowed)
			if test.wantErr && err == nil {
				t.Fatalf("validateTrustedReviewProxyArgv(%v, %q) = nil, want error", test.argv, test.allowed)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateTrustedReviewProxyArgv(%v, %q) error = %v", test.argv, test.allowed, err)
			}
		})
	}
}

func TestFormatTrustedReviewPRRef(t *testing.T) {
	t.Parallel()
	if got := FormatTrustedReviewPRRef(" acme/looper ", 42); got != "acme/looper#42" {
		t.Fatalf("FormatTrustedReviewPRRef = %q, want acme/looper#42", got)
	}
	if got := FormatTrustedReviewPRRef("", 1); got != "" {
		t.Fatalf("FormatTrustedReviewPRRef empty repo = %q, want empty", got)
	}
	if got := FormatTrustedReviewPRRef("acme/looper", 0); got != "" {
		t.Fatalf("FormatTrustedReviewPRRef zero PR = %q, want empty", got)
	}
}

func TestStartTrustedReviewProxyInjectsTokensIntoChild(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	outPath := filepath.Join(dir, "out.txt")
	// Child records whether FORGEJO_TOKEN is set and must not see the proxy socket env.
	script := "#!/bin/sh\nprintf 'token=%s sock=%s\\n' \"$FORGEJO_TOKEN\" \"$LOOPER_TRUSTED_REVIEW_SOCK\" > \"" + outPath + "\"\n"
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"}, "acme/looper#1")
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)
	if strings.TrimSpace(sockPath) == "" {
		t.Fatal("sockPath is empty")
	}

	t.Setenv(TrustedReviewSockEnv, sockPath)
	t.Setenv(trustedReviewProxySkipEnv, "")
	// Ensure the client process does not already hold the token.
	t.Setenv("FORGEJO_TOKEN", "")

	if err := ProxyReviewSubmit([]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, []byte(`{"body":"x"}`), dir); err != nil {
		t.Fatalf("ProxyReviewSubmit() error = %v", err)
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(out) error = %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "token=secret-token") {
		t.Fatalf("proxy child output = %q, want injected FORGEJO_TOKEN", got)
	}
	if strings.Contains(got, "sock="+sockPath) || strings.Contains(got, "sock=/") {
		t.Fatalf("proxy child output = %q, want empty LOOPER_TRUSTED_REVIEW_SOCK in child", got)
	}
}

func TestStartTrustedReviewProxyRejectsUnboundPR(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	// Child should never run for a mismatched PR.
	script := "#!/bin/sh\necho should-not-run\nexit 0\n"
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"}, "acme/looper#1")
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)

	t.Setenv(TrustedReviewSockEnv, sockPath)
	t.Setenv(trustedReviewProxySkipEnv, "")
	t.Setenv("FORGEJO_TOKEN", "")

	err = ProxyReviewSubmit([]string{"review", "submit", "acme/looper#99", "--event", "COMMENT"}, []byte(`{"body":"x"}`), dir)
	if err == nil {
		t.Fatal("ProxyReviewSubmit() = nil, want error for unbound PR")
	}
	if !strings.Contains(err.Error(), "bound to") && !strings.Contains(err.Error(), "rejects PR target") {
		t.Fatalf("ProxyReviewSubmit() error = %v, want PR binding rejection", err)
	}
}

func TestStartTrustedReviewProxyRequiresAllowedPR(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	if err := os.WriteFile(realLooper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, ""); err == nil {
		t.Fatal("StartTrustedReviewProxy() with empty allowed PR = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "not-a-ref"); err == nil {
		t.Fatal("StartTrustedReviewProxy() with invalid allowed PR = nil, want error")
	}
}

func TestTrustedReviewSockConfigured(t *testing.T) {
	t.Setenv(TrustedReviewSockEnv, "")
	t.Setenv(trustedReviewProxySkipEnv, "")
	if TrustedReviewSockConfigured() {
		t.Fatal("TrustedReviewSockConfigured() = true, want false when unset")
	}
	t.Setenv(TrustedReviewSockEnv, "/tmp/sock")
	if !TrustedReviewSockConfigured() {
		t.Fatal("TrustedReviewSockConfigured() = false, want true when sock set")
	}
	t.Setenv(trustedReviewProxySkipEnv, "1")
	if TrustedReviewSockConfigured() {
		t.Fatal("TrustedReviewSockConfigured() = true, want false for proxy child")
	}
}

func TestTrustedReviewProxyChildEnvOmitsSocketAndFile(t *testing.T) {
	t.Setenv(TrustedReviewSockEnv, "/tmp/should-not-propagate")
	t.Setenv(TrustedEnvFileEnv, "/tmp/secret-file")
	env := trustedReviewProxyChildEnv(map[string]string{"FORGEJO_TOKEN": "secret"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, TrustedReviewSockEnv+"=") {
		t.Fatalf("child env still has %s", TrustedReviewSockEnv)
	}
	if strings.Contains(joined, TrustedEnvFileEnv+"=") {
		t.Fatalf("child env still has %s", TrustedEnvFileEnv)
	}
	if !strings.Contains(joined, "FORGEJO_TOKEN=secret") {
		t.Fatalf("child env missing provider token: %s", joined)
	}
	if !strings.Contains(joined, trustedReviewProxySkipEnv+"=1") {
		t.Fatalf("child env missing skip marker: %s", joined)
	}
}

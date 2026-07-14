package forge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTrustedReviewProxyArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		argv    []string
		wantErr bool
	}{
		{name: "submit", argv: []string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, wantErr: false},
		{name: "global flags then submit", argv: []string{"--config", "/tmp/cfg.json", "review", "submit", "acme/looper#1"}, wantErr: false},
		{name: "reject status", argv: []string{"status"}, wantErr: true},
		{name: "reject review without submit", argv: []string{"review", "repair"}, wantErr: true},
		{name: "reject empty", argv: nil, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTrustedReviewProxyArgv(test.argv)
			if test.wantErr && err == nil {
				t.Fatalf("validateTrustedReviewProxyArgv(%v) = nil, want error", test.argv)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateTrustedReviewProxyArgv(%v) error = %v", test.argv, err)
			}
		})
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

	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"})
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

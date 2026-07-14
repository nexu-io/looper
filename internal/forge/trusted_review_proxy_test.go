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
		// Local policy flags are accepted for prompted command shape; the proxy
		// rewrites them to the daemon-bound policy before spawning the child.
		{name: "allow clean-review-event local policy shape", argv: []string{"review", "submit", "acme/looper#1", "--event", "APPROVE", "--clean-review-event", "APPROVE", "--blocking-review-event", "COMMENT"}, allowed: allowed, wantErr: false},
		{name: "allow blocking-review-event equals form shape", argv: []string{"review", "submit", "acme/looper#1", "--blocking-review-event=REQUEST_CHANGES", "--event", "REQUEST_CHANGES"}, allowed: allowed, wantErr: false},
		{name: "reject global review-events-clean override", argv: []string{"--roles-reviewer-behavior-review-events-clean", "APPROVE", "review", "submit", "acme/looper#1"}, allowed: allowed, wantErr: true},
		{name: "reject global reviewer-clean-review-event", argv: []string{"--reviewer-clean-review-event", "APPROVE", "review", "submit", "acme/looper#1"}, allowed: allowed, wantErr: true},
		{name: "reject allow-auto-approve override", argv: []string{"--allow-auto-approve", "true", "review", "submit", "acme/looper#1"}, allowed: allowed, wantErr: true},
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

func TestApplyTrustedReviewProxyPolicyRewritesAgentFlags(t *testing.T) {
	t.Parallel()
	policy := TrustedReviewProxyPolicy{Clean: "APPROVE", Blocking: "REQUEST_CHANGES"}
	got := applyTrustedReviewProxyPolicy(
		[]string{"review", "submit", "acme/looper#1", "--event", "COMMENT", "--clean-review-event", "COMMENT", "--blocking-review-event=COMMENT", "--commit-id", "abc"},
		policy,
	)
	want := []string{"review", "submit", "acme/looper#1", "--event", "COMMENT", "--commit-id", "abc", "--clean-review-event", "APPROVE", "--blocking-review-event", "REQUEST_CHANGES"}
	if len(got) != len(want) {
		t.Fatalf("applyTrustedReviewProxyPolicy() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applyTrustedReviewProxyPolicy() = %#v, want %#v", got, want)
		}
	}
	// Missing agent policy flags still get daemon injection.
	got = applyTrustedReviewProxyPolicy([]string{"review", "submit", "acme/looper#1"}, policy)
	want = []string{"review", "submit", "acme/looper#1", "--clean-review-event", "APPROVE", "--blocking-review-event", "REQUEST_CHANGES"}
	if len(got) != len(want) {
		t.Fatalf("applyTrustedReviewProxyPolicy(no flags) = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applyTrustedReviewProxyPolicy(no flags) = %#v, want %#v", got, want)
		}
	}
}

func testTrustedReviewPolicy() TrustedReviewProxyPolicy {
	return TrustedReviewProxyPolicy{Clean: "COMMENT", Blocking: "COMMENT"}
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

func TestStartTrustedReviewProxyAllowsEmptyTrustedEnv(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	outPath := filepath.Join(dir, "out.txt")
	// Tea-backed providers have no tokenEnv; proxy still binds PR/CWD/config.
	script := "#!/bin/sh\ntouch ./proxy-child-ran\nprintf 'sock=%s config=%s\\n' \"$LOOPER_TRUSTED_REVIEW_SOCK\" \"$LOOPER_CONFIG\" > \"" + outPath + "\"\n"
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	daemonConfig := filepath.Join(dir, "daemon-config.json")
	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, nil, "acme/looper#1", dir, daemonConfig, testTrustedReviewPolicy())
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)
	if strings.TrimSpace(sockPath) == "" {
		t.Fatal("sockPath is empty for empty trustedEnv")
	}

	t.Setenv(TrustedReviewSockEnv, sockPath)
	t.Setenv(trustedReviewProxySkipEnv, "")
	t.Setenv("LOOPER_CONFIG", filepath.Join(dir, "ambient-config.json"))

	if err := ProxyReviewSubmit([]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, []byte(`{"body":"x"}`), filepath.Join(dir, "not-bound")); err != nil {
		t.Fatalf("ProxyReviewSubmit() error = %v", err)
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(out) error = %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "config="+daemonConfig) {
		t.Fatalf("proxy child output = %q, want injected LOOPER_CONFIG=%s", got, daemonConfig)
	}
	if strings.Contains(got, "sock="+sockPath) || strings.Contains(got, "sock=/") {
		t.Fatalf("proxy child output = %q, want empty LOOPER_TRUSTED_REVIEW_SOCK in child", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "proxy-child-ran")); err != nil {
		t.Fatalf("proxy child did not run in bound cwd %q: %v", dir, err)
	}
}

func TestStartTrustedReviewProxyInjectsTokensIntoChild(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	outPath := filepath.Join(dir, "out.txt")
	// Child records token/socket/config env and leaves a marker in its process
	// CWD so daemon-bound workdir is observable without relying on pwd symlink form.
	script := "#!/bin/sh\ntouch ./proxy-child-ran\nprintf 'token=%s sock=%s config=%s\\n' \"$FORGEJO_TOKEN\" \"$LOOPER_TRUSTED_REVIEW_SOCK\" \"$LOOPER_CONFIG\" > \"" + outPath + "\"\n"
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	daemonConfig := filepath.Join(dir, "daemon-config.json")
	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"}, "acme/looper#1", dir, daemonConfig, testTrustedReviewPolicy())
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
	// Ambient LOOPER_CONFIG must not win over the daemon-bound path.
	t.Setenv("LOOPER_CONFIG", filepath.Join(dir, "ambient-config.json"))

	// Request cwd is intentionally wrong; daemon-bound cwd must win.
	if err := ProxyReviewSubmit([]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, []byte(`{"body":"x"}`), filepath.Join(dir, "not-bound")); err != nil {
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
	if !strings.Contains(got, "config="+daemonConfig) {
		t.Fatalf("proxy child output = %q, want injected LOOPER_CONFIG=%s", got, daemonConfig)
	}
	if strings.Contains(got, "sock="+sockPath) || strings.Contains(got, "sock=/") {
		t.Fatalf("proxy child output = %q, want empty LOOPER_TRUSTED_REVIEW_SOCK in child", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "proxy-child-ran")); err != nil {
		t.Fatalf("proxy child did not run in bound cwd %q: %v", dir, err)
	}
}

func TestStartTrustedReviewProxyRewritesPolicyFlags(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	outPath := filepath.Join(dir, "argv.txt")
	// Record argv so we can assert daemon-bound policy replaced agent values.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + outPath + "\"\n"
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	policy := TrustedReviewProxyPolicy{Clean: "APPROVE", Blocking: "REQUEST_CHANGES"}
	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"}, "acme/looper#1", dir, "", policy)
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)

	t.Setenv(TrustedReviewSockEnv, sockPath)
	t.Setenv(trustedReviewProxySkipEnv, "")
	t.Setenv("FORGEJO_TOKEN", "")

	// Agent attempts to downgrade blocking/clean policy via argv.
	if err := ProxyReviewSubmit([]string{
		"review", "submit", "acme/looper#1",
		"--event", "COMMENT",
		"--clean-review-event", "COMMENT",
		"--blocking-review-event", "COMMENT",
	}, []byte(`{"body":"x"}`), dir); err != nil {
		t.Fatalf("ProxyReviewSubmit() error = %v", err)
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(argv) error = %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "--clean-review-event\nAPPROVE\n") {
		t.Fatalf("child argv = %q, want bound --clean-review-event APPROVE", got)
	}
	if !strings.Contains(got, "--blocking-review-event\nREQUEST_CHANGES\n") {
		t.Fatalf("child argv = %q, want bound --blocking-review-event REQUEST_CHANGES", got)
	}
	// Downgraded agent values must not remain as the sole/authoritative policy.
	if strings.Count(got, "COMMENT") > 1 {
		// event COMMENT is fine once; clean/blocking must not still be COMMENT.
		lines := strings.Split(strings.TrimSpace(got), "\n")
		for i, line := range lines {
			if line == "--clean-review-event" && i+1 < len(lines) && lines[i+1] == "COMMENT" {
				t.Fatalf("child argv retained agent clean policy COMMENT: %q", got)
			}
			if line == "--blocking-review-event" && i+1 < len(lines) && lines[i+1] == "COMMENT" {
				t.Fatalf("child argv retained agent blocking policy COMMENT: %q", got)
			}
		}
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

	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"}, "acme/looper#1", dir, "", testTrustedReviewPolicy())
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
	policy := testTrustedReviewPolicy()
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "", dir, "", policy); err == nil {
		t.Fatal("StartTrustedReviewProxy() with empty allowed PR = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "not-a-ref", dir, "", policy); err == nil {
		t.Fatal("StartTrustedReviewProxy() with invalid allowed PR = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "acme/looper#1", "", "", policy); err == nil {
		t.Fatal("StartTrustedReviewProxy() with empty allowed CWD = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "acme/looper#1", "relative/path", "", policy); err == nil {
		t.Fatal("StartTrustedReviewProxy() with relative allowed CWD = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "acme/looper#1", dir, "", TrustedReviewProxyPolicy{}); err == nil {
		t.Fatal("StartTrustedReviewProxy() with empty policy = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "acme/looper#1", dir, "", TrustedReviewProxyPolicy{Clean: "APPROVE", Blocking: "APPROVE"}); err == nil {
		t.Fatal("StartTrustedReviewProxy() with invalid blocking policy = nil, want error")
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
	t.Setenv("LOOPER_CONFIG", "/tmp/ambient.json")
	env := trustedReviewProxyChildEnv(map[string]string{"FORGEJO_TOKEN": "secret"}, "/tmp/daemon-config.json")
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
	if !strings.Contains(joined, "LOOPER_CONFIG=/tmp/daemon-config.json") {
		t.Fatalf("child env missing daemon LOOPER_CONFIG: %s", joined)
	}
	if strings.Contains(joined, "LOOPER_CONFIG=/tmp/ambient.json") {
		t.Fatalf("child env retained ambient LOOPER_CONFIG: %s", joined)
	}
	// Empty daemon config path leaves ambient LOOPER_CONFIG alone.
	env = trustedReviewProxyChildEnv(map[string]string{"FORGEJO_TOKEN": "secret"}, "")
	joined = strings.Join(env, "\n")
	if !strings.Contains(joined, "LOOPER_CONFIG=/tmp/ambient.json") {
		t.Fatalf("child env without bound config path dropped ambient LOOPER_CONFIG: %s", joined)
	}
}

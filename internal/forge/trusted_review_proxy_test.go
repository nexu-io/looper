package forge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
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
	policy := TrustedReviewProxyPolicy{Clean: "APPROVE", Blocking: "REQUEST_CHANGES", ExpectedCommitID: "bound-head", ReviewerManual: true, ReviewerRunID: "run_bound"}
	got := applyTrustedReviewProxyPolicy(
		[]string{"review", "submit", "acme/looper#1", "--event", "COMMENT", "--clean-review-event", "COMMENT", "--blocking-review-event=COMMENT", "--commit-id", "agent-head", "--reviewer-manual", "--reviewer-run-id", "run_agent"},
		policy,
	)
	want := []string{"review", "submit", "acme/looper#1", "--event", "COMMENT", "--clean-review-event", "APPROVE", "--blocking-review-event", "REQUEST_CHANGES", "--commit-id", "bound-head", "--reviewer-manual", "--reviewer-run-id", "run_bound"}
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
	want = []string{"review", "submit", "acme/looper#1", "--clean-review-event", "APPROVE", "--blocking-review-event", "REQUEST_CHANGES", "--commit-id", "bound-head", "--reviewer-manual", "--reviewer-run-id", "run_bound"}
	if len(got) != len(want) {
		t.Fatalf("applyTrustedReviewProxyPolicy(no flags) = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applyTrustedReviewProxyPolicy(no flags) = %#v, want %#v", got, want)
		}
	}

	// An automatic run must strip an agent attempt to opt into manual bypass
	// or substitute another run identity.
	automatic := TrustedReviewProxyPolicy{Clean: "COMMENT", Blocking: "COMMENT", ExpectedCommitID: "automatic-head"}
	got = applyTrustedReviewProxyPolicy([]string{"review", "submit", "acme/looper#1", "--reviewer-manual", "--reviewer-run-id=run_agent"}, automatic)
	want = []string{"review", "submit", "acme/looper#1", "--clean-review-event", "COMMENT", "--blocking-review-event", "COMMENT", "--commit-id", "automatic-head"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("applyTrustedReviewProxyPolicy(automatic) = %#v, want %#v", got, want)
	}
}

func testTrustedReviewPolicy() TrustedReviewProxyPolicy {
	return TrustedReviewProxyPolicy{Clean: "COMMENT", Blocking: "COMMENT", ExpectedCommitID: "abc123"}
}

// trustedReviewProxyStubScript builds a fake looper child that first drains the
// inherited config snapshot pipe (fd 3). Without that drain, a fast-exiting
// stub races the proxy's snapshot writer and can surface "broken pipe" on CI.
func trustedReviewProxyStubScript(body string) string {
	return "#!/bin/sh\ncat <&3 >/dev/null 2>&1 || true\n" + body
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
	script := trustedReviewProxyStubScript("touch ./proxy-child-ran\nprintf 'sock=%s config=%s fd=%s\\n' \"$LOOPER_TRUSTED_REVIEW_SOCK\" \"$LOOPER_CONFIG\" \"$LOOPER_TRUSTED_REVIEW_CONFIG_FD\" > \"" + outPath + "\"\n")
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, nil, "acme/looper#1", dir, config.Config{}, testTrustedReviewPolicy())
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
	if !strings.Contains(got, "config= fd=3") || strings.Contains(got, "ambient-config.json") {
		t.Fatalf("proxy child output = %q, want descriptor-backed config without LOOPER_CONFIG", got)
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
	script := trustedReviewProxyStubScript("touch ./proxy-child-ran\nprintf 'token=%s sock=%s config=%s fd=%s\\n' \"$FORGEJO_TOKEN\" \"$LOOPER_TRUSTED_REVIEW_SOCK\" \"$LOOPER_CONFIG\" \"$LOOPER_TRUSTED_REVIEW_CONFIG_FD\" > \"" + outPath + "\"\n")
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"}, "acme/looper#1", dir, config.Config{}, testTrustedReviewPolicy())
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
	if !strings.Contains(got, "config= fd=3") || strings.Contains(got, "ambient-config.json") {
		t.Fatalf("proxy child output = %q, want descriptor-backed config without LOOPER_CONFIG", got)
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
	script := trustedReviewProxyStubScript("printf '%s\\n' \"$@\" > \"" + outPath + "\"\n")
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	policy := TrustedReviewProxyPolicy{Clean: "APPROVE", Blocking: "REQUEST_CHANGES", ExpectedCommitID: "bound-head", ReviewerManual: true, ReviewerRunID: "run_bound"}
	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"}, "acme/looper#1", dir, config.Config{}, policy)
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
		"--commit-id", "agent-head",
		"--reviewer-run-id", "run_agent",
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
	if !strings.Contains(got, "--commit-id\nbound-head\n") || strings.Contains(got, "agent-head") {
		t.Fatalf("child argv = %q, want only bound commit id", got)
	}
	if !strings.Contains(got, "--reviewer-manual\n") || !strings.Contains(got, "--reviewer-run-id\nrun_bound\n") || strings.Contains(got, "run_agent") {
		t.Fatalf("child argv = %q, want bound manual run identity", got)
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
	script := trustedReviewProxyStubScript("echo should-not-run\nexit 0\n")
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "secret-token"}, "acme/looper#1", dir, config.Config{}, testTrustedReviewPolicy())
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
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "", dir, config.Config{}, policy); err == nil {
		t.Fatal("StartTrustedReviewProxy() with empty allowed PR = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "not-a-ref", dir, config.Config{}, policy); err == nil {
		t.Fatal("StartTrustedReviewProxy() with invalid allowed PR = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "acme/looper#1", "", config.Config{}, policy); err == nil {
		t.Fatal("StartTrustedReviewProxy() with empty allowed CWD = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "acme/looper#1", "relative/path", config.Config{}, policy); err == nil {
		t.Fatal("StartTrustedReviewProxy() with relative allowed CWD = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "acme/looper#1", dir, config.Config{}, TrustedReviewProxyPolicy{}); err == nil {
		t.Fatal("StartTrustedReviewProxy() with empty policy = nil, want error")
	}
	if _, _, err := StartTrustedReviewProxy(realLooper, map[string]string{"FORGEJO_TOKEN": "x"}, "acme/looper#1", dir, config.Config{}, TrustedReviewProxyPolicy{Clean: "APPROVE", Blocking: "APPROVE"}); err == nil {
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
	t.Setenv(trustedReviewProxySkipEnv, "")
	t.Setenv("LOOPER_CONFIG", "/tmp/ambient.json")
	env := trustedReviewProxyChildEnv(map[string]string{
		"FORGEJO_TOKEN":           "secret",
		TrustedReviewSockEnv:      "/tmp/agent-controlled-sock",
		TrustedEnvFileEnv:         "/tmp/agent-controlled-secret-file",
		trustedReviewProxySkipEnv: "",
		"LOOPER_CONFIG":           "/tmp/agent-controlled-config.json",
	}, 3)
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
	if !strings.Contains(joined, TrustedReviewConfigFDEnv+"=3") {
		t.Fatalf("child env missing trusted config descriptor: %s", joined)
	}
	if strings.Contains(joined, "LOOPER_CONFIG=") {
		t.Fatalf("child env retained a named config override: %s", joined)
	}
	// Missing descriptor still strips named config sources in a proxy child.
	env = trustedReviewProxyChildEnv(map[string]string{"FORGEJO_TOKEN": "secret"}, 0)
	joined = strings.Join(env, "\n")
	if strings.Contains(joined, "LOOPER_CONFIG=") || strings.Contains(joined, TrustedReviewConfigFDEnv+"=") {
		t.Fatalf("child env without descriptor retained config selectors: %s", joined)
	}
}

func TestMarshalTrustedReviewConfigSnapshotSanitizesSecretsAndPreservesMaterializedPolicy(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	token := "daemon-local-token"
	vendor := config.AgentVendorCodex
	model := "run-model"
	requireReviewRequest := false
	labels := []string{"run-policy"}
	labelMode := config.LabelModeAny
	cfg.Server.AuthMode = config.AuthModeLocalToken
	cfg.Server.LocalToken = &token
	cfg.Agent.Vendor = &vendor
	cfg.Agent.Model = &model
	cfg.Agent.Env = map[string]string{"AGENT_SECRET": "secret"}
	cfg.Agent.Params = map[string]any{"api_key": "secret"}
	cfg.Daemon.Environment = map[string]string{"DAEMON_SECRET": "secret"}
	cfg.Disclosure = config.DisclosureConfig{Enabled: true, IncludeAgent: true, Channels: config.DisclosureChannelsConfig{ReviewComment: true}}
	cfg.Projects = []config.ProjectRefConfig{{
		ID: "project_1", Name: "Project", Repo: "acme/looper", RepoPath: t.TempDir(),
		Roles: &config.PartialRoleConfigs{Reviewer: &config.PartialReviewerRoleConfig{Discovery: &config.PartialReviewerRoleDiscoveryConfig{Triggers: &config.PartialReviewerRoleTriggersConfig{
			RequireReviewRequest: &requireReviewRequest,
			Labels:               &labels,
			LabelMode:            &labelMode,
		}}}},
	}}

	raw, err := marshalTrustedReviewConfigSnapshot(cfg)
	if err != nil {
		t.Fatalf("marshalTrustedReviewConfigSnapshot() error = %v", err)
	}
	var snapshot config.Config
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("Unmarshal(snapshot) error = %v", err)
	}
	if snapshot.Server.AuthMode != config.AuthModeNone || snapshot.Server.LocalToken != nil {
		t.Fatalf("snapshot server auth = %#v, want auth none without local token", snapshot.Server)
	}
	if len(snapshot.Agent.Env) != 0 || len(snapshot.Agent.Params) != 0 || len(snapshot.Daemon.Environment) != 0 {
		t.Fatalf("snapshot retained process secrets: agentEnv=%#v agentParams=%#v daemonEnv=%#v", snapshot.Agent.Env, snapshot.Agent.Params, snapshot.Daemon.Environment)
	}
	if snapshot.Agent.Vendor == nil || *snapshot.Agent.Vendor != vendor || snapshot.Agent.Model == nil || *snapshot.Agent.Model != model || !snapshot.Disclosure.Enabled {
		t.Fatalf("snapshot dropped run identity/disclosure policy: agent=%#v disclosure=%#v", snapshot.Agent, snapshot.Disclosure)
	}
	roles := config.ProjectRoleConfigs(snapshot, "project_1")
	if roles.Reviewer.Discovery.Triggers.RequireReviewRequest || strings.Join(roles.Reviewer.Discovery.Triggers.Labels, ",") != "run-policy" || roles.Reviewer.Discovery.Triggers.LabelMode != config.LabelModeAny {
		t.Fatalf("snapshot project roles = %#v, want materialized run-policy/any override", roles.Reviewer.Discovery.Triggers)
	}
}

func TestTrustedReviewProxyKeepsRunConfigWhenLiveFileChanges(t *testing.T) {
	dir := t.TempDir()
	realLooper := filepath.Join(dir, "real-looper")
	capturedPath := filepath.Join(dir, "captured-config.json")
	configPathOutput := filepath.Join(dir, "config-path.txt")
	// This stub intentionally consumes fd 3 into a file for assertions.
	script := "#!/bin/sh\ncat <&3 > \"" + capturedPath + "\"\nprintf '%s' \"$LOOPER_CONFIG\" > \"" + configPathOutput + "\"\n"
	if err := os.WriteFile(realLooper, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}

	runConfig, err := config.DefaultConfig(dir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	runVendor := config.AgentVendorCodex
	runModel := "run-model"
	requireReviewRequest := false
	runLabels := []string{"run-policy"}
	runLabelMode := config.LabelModeAny
	runConfig.Agent.Vendor = &runVendor
	runConfig.Agent.Model = &runModel
	runConfig.Disclosure.Enabled = true
	runConfig.Projects = []config.ProjectRefConfig{{
		ID: "project_1", Name: "Project", Repo: "acme/looper", RepoPath: dir,
		Roles: &config.PartialRoleConfigs{Reviewer: &config.PartialReviewerRoleConfig{Discovery: &config.PartialReviewerRoleDiscoveryConfig{Triggers: &config.PartialReviewerRoleTriggersConfig{
			RequireReviewRequest: &requireReviewRequest,
			Labels:               &runLabels,
			LabelMode:            &runLabelMode,
		}}}},
	}}

	livePath := filepath.Join(dir, "live-config.json")
	runRaw, err := json.Marshal(runConfig)
	if err != nil {
		t.Fatalf("Marshal(run config) error = %v", err)
	}
	var liveConfig config.Config
	if err := json.Unmarshal(runRaw, &liveConfig); err != nil {
		t.Fatalf("Unmarshal(live config clone) error = %v", err)
	}
	liveVendor := config.AgentVendorOpenCode
	liveModel := "live-model"
	liveLabels := []string{"live-policy"}
	liveConfig.Agent.Vendor = &liveVendor
	liveConfig.Agent.Model = &liveModel
	liveConfig.Disclosure.Enabled = false
	liveConfig.Projects[0].Roles.Reviewer.Discovery.Triggers.Labels = &liveLabels
	liveRaw, err := json.Marshal(liveConfig)
	if err != nil {
		t.Fatalf("Marshal(live config) error = %v", err)
	}
	if err := os.WriteFile(livePath, runRaw, 0o600); err != nil {
		t.Fatalf("WriteFile(initial live config) error = %v", err)
	}

	sockPath, cleanup, err := StartTrustedReviewProxy(realLooper, nil, "acme/looper#1", dir, runConfig, testTrustedReviewPolicy())
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv(TrustedReviewSockEnv, sockPath)
	t.Setenv(trustedReviewProxySkipEnv, "")
	// Simulate the daemon's live source changing after this run's proxy exists.
	// The trusted child must load the already-captured run snapshot instead.
	if err := os.WriteFile(livePath, liveRaw, 0o600); err != nil {
		t.Fatalf("WriteFile(changed live config) error = %v", err)
	}
	t.Setenv("LOOPER_CONFIG", livePath)
	if err := ProxyReviewSubmit([]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, []byte(`{"body":"x"}`), dir); err != nil {
		t.Fatalf("ProxyReviewSubmit() error = %v", err)
	}
	capturedRaw, err := os.ReadFile(capturedPath)
	if err != nil {
		t.Fatalf("ReadFile(captured config) error = %v", err)
	}
	var captured config.Config
	if err := json.Unmarshal(capturedRaw, &captured); err != nil {
		t.Fatalf("Unmarshal(captured config) error = %v", err)
	}
	if captured.Agent.Vendor == nil || *captured.Agent.Vendor != runVendor || captured.Agent.Model == nil || *captured.Agent.Model != runModel || !captured.Disclosure.Enabled {
		t.Fatalf("captured config = agent %#v disclosure %#v, want active run snapshot", captured.Agent, captured.Disclosure)
	}
	roles := config.ProjectRoleConfigs(captured, "project_1")
	if strings.Join(roles.Reviewer.Discovery.Triggers.Labels, ",") != "run-policy" || roles.Reviewer.Discovery.Triggers.LabelMode != config.LabelModeAny {
		t.Fatalf("captured project roles = %#v, want active run-policy/any", roles.Reviewer.Discovery.Triggers)
	}
	childConfigPath, err := os.ReadFile(configPathOutput)
	if err != nil {
		t.Fatalf("ReadFile(config path) error = %v", err)
	}
	if strings.TrimSpace(string(childConfigPath)) != "" {
		t.Fatalf("trusted child retained named config path %q; want descriptor-only snapshot", strings.TrimSpace(string(childConfigPath)))
	}
}

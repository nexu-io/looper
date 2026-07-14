package outboundguard

import (
	"strings"
	"testing"
)

func TestValidateRejectsUnsafeOutboundContentWithoutEchoingIt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "entropy", text: "The process returned q8Kz1Wm9P2vR7xL4nB6cD0fH3jS5uY+/ unexpectedly.", want: "high-entropy"},
		{name: "api key assignment", text: "OPENAI_API_KEY=sk-sensitive", want: "credential-shaped"},
		{name: "token assignment", text: "TOKEN=short", want: "credential-shaped"},
		{name: "trailing token", text: "SERVICE_TOKEN=secret-value", want: "credential-shaped"},
		{name: "secret assignment", text: "SECRET_KEY=1234", want: "credential-shaped"},
		{name: "password assignment", text: "PASSWORD=abc", want: "credential-shaped"},
		{name: "auth password", text: "AUTH_PASSWORD=abc", want: "credential-shaped"},
		{name: "api key", text: "API_KEY=deadbeef", want: "credential-shaped"},
		{name: "database URL userinfo", text: "DATABASE_URL=postgres://app:pw@db.example/prod", want: "credential-bearing"},
		{name: "exported connection URL", text: "export CACHE_URL=redis://worker:p%40ss@cache.example/0", want: "credential-bearing"},
		{name: "connection URL in prose", text: "Connect with mongodb+srv://agent:short@db.example/prod.", want: "credential-bearing"},
		{name: "password-only redis URL", text: "REDIS_URL=redis://:pw@cache.example/0", want: "credential-bearing"},
		{name: "query password URL", text: "DATABASE_URL=redis://cache.example/0?password=pw", want: "credential-bearing"},
		{name: "query client_secret URL", text: "AUTH_URL=https://auth.example/token?client_secret=s3cret&grant_type=client_credentials", want: "credential-bearing"},
		{name: "query access_token URL", text: "Hit https://api.example/v1?access_token=abc123 to reproduce.", want: "credential-bearing"},
		{name: "private key PEM", text: "key material:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", want: "private key"},
		{name: "shell prompt credential", text: "$ SERVICE_TOKEN=secret-value", want: "credential-shaped"},
		{name: "xtrace export credential", text: "+ export TOKEN=short", want: "credential-shaped"},
		{name: "declare -x credential", text: `declare -x SERVICE_TOKEN="secret-value"`, want: "credential-shaped"},
		{name: "typeset -x credential", text: "typeset -x PASSWORD=abc", want: "credential-shaped"},
		{name: "environment dump", text: "HOME=/tmp\nPATH=/bin\nSHELL=/bin/sh\nLANG=C\nTERM=dumb", want: "environment-dump-shaped"},
		{name: "declare -x environment dump", text: "declare -x HOME=/tmp\ndeclare -x PATH=/bin\ndeclare -x SHELL=/bin/sh\ndeclare -x LANG=C\ndeclare -x TERM=dumb", want: "environment-dump-shaped"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Field{Name: "comment body", Text: tc.text})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want %q rejection", err, tc.want)
			}
			if !IsRejection(err) {
				t.Fatalf("Validate() error type = %T, want *Rejection", err)
			}
			if !strings.Contains(err.Error(), RecoveryGuidance) {
				t.Fatalf("error %q missing recovery guidance", err)
			}
			if strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error %q echoed rejected content", err)
			}
		})
	}
}

func TestValidateAllowsCommonPublicationIdentifiers(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"Commit f81d0caa4db2a28627accfd89ad29af292291097 introduces the regression.",
		"Trace ID 019f5693-81ce-4893-8df5-89db82778ac7 identifies the request.",
		"Use ${OPENAI_API_KEY} from the process environment.",
		"The configuration example is FEATURE_FLAG=true.",
		"DATABASE_URL=postgres://db.example/prod",
		"DATABASE_URL=redis://cache.example/0?db=0",
		"Repository URL https://git.example/org/repo and contact ops@example.com.",
		"Docs: https://example.com/auth?passwordless=true for the login flow.",
		"Docs: https://example.com/oauth?token_type=bearer&expires_in=3600",
		"-----BEGIN CERTIFICATE-----\nMIIBkTCB+wIJAKH\n-----END CERTIFICATE-----",
	} {
		if err := Validate(Field{Name: "body", Text: text}); err != nil {
			t.Errorf("Validate(%q) error = %v, want safe", text, err)
		}
	}
}

func TestValidateReviewThreadReplyAllowsExactOpaqueThreadID(t *testing.T) {
	t.Parallel()
	for _, threadID := range []string{"PRRT_kwDOSOgY8s6QeKwr", "MDQ6UHVsbFJlcXVlc3RSZXZpZXdUaHJlYWQxMjM0NTY="} {
		body := "Looper checked this thread.\n<!-- looper:thread-resolution thread=" + threadID + " head=0dd6a5019812fc422f9f20626530758ad67ad66e decision=objectively_fixed -->"
		if err := ValidateReviewThreadReply(body, threadID); err != nil {
			t.Errorf("ValidateReviewThreadReply(%q) error = %v, want safe opaque thread ID", threadID, err)
		}
	}
}

func TestThreadIDExemptionIsScopedToMatchingReviewThreadReply(t *testing.T) {
	threadID := "MDQ6UHVsbFJlcXVlc3RSZXZpZXdUaHJlYWQxMjM0NTY="
	marker := "<!-- looper:thread-resolution thread=" + threadID + " head=0dd6a5019812fc422f9f20626530758ad67ad66e decision=objectively_fixed -->"
	tests := []struct {
		name string
		err  error
	}{
		{name: "generic publication", err: Validate(Field{Name: "pull request body", Text: marker})},
		{name: "different active thread", err: ValidateReviewThreadReply(marker, "PRRT_kwDOSOgY8s6QeKwr")},
		{name: "high entropy prose", err: ValidateReviewThreadReply("Agent evidence q8Kz1Wm9P2vR7xL4nB6cD0fH3jS5uY+/\n"+marker, threadID)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil || !strings.Contains(tc.err.Error(), "high-entropy") {
				t.Fatalf("validation error = %v, want high-entropy rejection", tc.err)
			}
		})
	}
}

func TestValidatePrefersLowFalsePositivesOnReviewProse(t *testing.T) {
	t.Parallel()
	// Guard is best-effort: ambiguous short names and non-secret config stay open.
	for _, text := range []string{
		"password = request.FormValue(\"password\")",
		"api_key = loadFromEnv()",
		"token = \"example\" in the docs",
		"Change:\n```\napi_key = loadFromEnv()\n```",
		"TOKENIZATION=enabled",
		"PASSWORDLESS=true",
		"BYPASS=enabled",
		"COMPASS_DIRECTION=north",
		"PASS_RATE=0.95",
		"PASS_COUNT=3",
		"PASS_THROUGH=enabled",
		"DB_PASS=pw",
		"REDIS_PASS=s3",
		"PGPASSWORD=pw",
		"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		"CREDENTIAL_PROVIDER=aws",
		"AUTHENTICATION_MODE=oauth",
		"SECRETS_MANAGER=aws",
		"refresh_token_ttl=3600",
		"has_password_field=true",
		"const TOKEN = 'x'",
		"export const API_KEY = process.env.API_KEY",
		"Do not hardcode token = value in production.",
		`{"password": "secret"}`,
		"password: hunter2",
		`declare -x FEATURE_FLAG=true`,
		`declare -x PATH="/usr/bin"`,
		// Query aliases that are too short / ambiguous for the high-confidence list.
		"CACHE_URL=redis://cache.example/0?pass=s3cret",
		"https://auth.example/callback?refresh_token=abc123",
	} {
		if err := Validate(Field{Name: "body", Text: text}); err != nil {
			t.Errorf("Validate(%q) error = %v, want safe", text, err)
		}
	}
}

func TestValidateStillRejectsHighConfidenceCredentialNames(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"MY_TOKEN=short",
		"AUTH_PASSWORD=abc",
		"app-secret=value",
		"export APIKEY=deadbeef",
		"X_API_KEY=deadbeef",
	} {
		err := Validate(Field{Name: "body", Text: text})
		if err == nil || !strings.Contains(err.Error(), "credential-shaped") {
			t.Errorf("Validate(%q) error = %v, want credential-shaped rejection", text, err)
		}
	}
}

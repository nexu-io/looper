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
		{name: "credential", text: "OPENAI_API_KEY=sk-sensitive", want: "credential-shaped"},
		{name: "token at start", text: "TOKEN=short", want: "credential-shaped"},
		{name: "secret at start", text: "SECRET_KEY=1234", want: "credential-shaped"},
		{name: "password at start", text: "PASSWORD=abc", want: "credential-shaped"},
		{name: "DB_PASS short", text: "DB_PASS=pw", want: "credential-shaped"},
		{name: "PGPASSWORD short", text: "PGPASSWORD=pw", want: "credential-shaped"},
		{name: "REDIS_PASS short", text: "REDIS_PASS=s3", want: "credential-shaped"},
		{name: "API key at start", text: "API_KEY=deadbeef", want: "credential-shaped"},
		{name: "database URL", text: "DATABASE_URL=postgres://app:pw@db.example/prod", want: "credential-bearing"},
		{name: "exported connection URL", text: "export CACHE_URL=redis://worker:p%40ss@cache.example/0", want: "credential-bearing"},
		{name: "connection URL in prose", text: "Connect with mongodb+srv://agent:short@db.example/prod.", want: "credential-bearing"},
		{name: "password-only redis URL", text: "REDIS_URL=redis://:pw@cache.example/0", want: "credential-bearing"},
		{name: "password-only postgres URL", text: "DATABASE_URL=postgres://:secret@db.example/prod", want: "credential-bearing"},
		{name: "password-only URL in prose", text: "Connect with redis://:s3cret@cache.example/0.", want: "credential-bearing"},
		{name: "query password redis URL", text: "DATABASE_URL=redis://cache.example/0?password=pw", want: "credential-bearing"},
		{name: "query password jdbc URL", text: "jdbc:postgresql://db.example/prod?user=app&password=pw", want: "credential-bearing"},
		{name: "query token URL in prose", text: "Hit https://api.example/v1?access_token=abc123 to reproduce.", want: "credential-bearing"},
		{name: "query client_secret URL", text: "AUTH_URL=https://auth.example/token?client_secret=s3cret&grant_type=client_credentials", want: "credential-bearing"},
		{name: "query pass alias", text: "CACHE_URL=redis://cache.example/0?pass=s3cret", want: "credential-bearing"},
		{name: "query refresh_token", text: "https://auth.example/callback?refresh_token=abc123", want: "credential-bearing"},
		{name: "query api-key", text: "https://api.example/v1?api-key=deadbeef", want: "credential-bearing"},
		{name: "private key PEM", text: "key material:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", want: "private key"},
		{name: "aws access key assignment", text: "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE", want: "credential-shaped"},
		{name: "shell prompt credential", text: "$ SERVICE_TOKEN=secret-value", want: "credential-shaped"},
		{name: "xtrace export credential", text: "+ export TOKEN=short", want: "credential-shaped"},
		{name: "stacked xtrace credential", text: "++ PASSWORD=abc", want: "credential-shaped"},
		{name: "declare -x credential", text: `declare -x SERVICE_TOKEN="secret-value"`, want: "credential-shaped"},
		{name: "declare -px credential", text: `declare -px TOKEN=short`, want: "credential-shaped"},
		{name: "declare -x api key", text: `declare -x OPENAI_API_KEY="sk-sensitive"`, want: "credential-shaped"},
		{name: "typeset -x credential", text: "typeset -x PASSWORD=abc", want: "credential-shaped"},
		{name: "environment", text: "HOME=/tmp\nPATH=/bin\nSHELL=/bin/sh\nLANG=C\nTERM=dumb", want: "environment-dump-shaped"},
		{name: "shell-prefixed environment dump", text: "$ HOME=/tmp\n$ PATH=/bin\n$ SHELL=/bin/sh\n$ LANG=C\n$ TERM=dumb", want: "environment-dump-shaped"},
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

func TestValidateAllowsCodeReviewAndCompoundWordAssignments(t *testing.T) {
	t.Parallel()
	// These previously false-positived when sensitive keywords were matched as
	// bare substrings or when code-style "name = value" spacing was accepted.
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
		"PASS_THRESHOLD=0.8",
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
	} {
		if err := Validate(Field{Name: "body", Text: text}); err != nil {
			t.Errorf("Validate(%q) error = %v, want safe", text, err)
		}
	}
}

func TestValidateStillRejectsSegmentBoundedCredentialNames(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"MY_TOKEN=short",
		"AUTH_PASSWORD=abc",
		"app-secret=value",
		"DB_CREDENTIALS=abc",
		"export APIKEY=deadbeef",
		"X_API_KEY=deadbeef",
	} {
		err := Validate(Field{Name: "body", Text: text})
		if err == nil || !strings.Contains(err.Error(), "credential-shaped") {
			t.Errorf("Validate(%q) error = %v, want credential-shaped rejection", text, err)
		}
	}
}

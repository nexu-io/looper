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
		{name: "API key at start", text: "API_KEY=deadbeef", want: "credential-shaped"},
		{name: "environment", text: "HOME=/tmp\nPATH=/bin\nSHELL=/bin/sh\nLANG=C\nTERM=dumb", want: "environment-dump-shaped"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Field{Name: "comment body", Text: tc.text})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want %q rejection", err, tc.want)
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
	} {
		if err := Validate(Field{Name: "body", Text: text}); err != nil {
			t.Errorf("Validate(%q) error = %v, want safe", text, err)
		}
	}
}

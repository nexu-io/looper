package github

import "testing"

func TestUnsafeReviewTextRejectsHighEntropyToken(t *testing.T) {
	t.Parallel()
	text := "The process returned q8Kz1Wm9P2vR7xL4nB6cD0fH3jS5uY+/ unexpectedly."
	if got := unsafeReviewText(text); got != "contains a high-entropy credential-shaped token" {
		t.Fatalf("unsafeReviewText() = %q, want high-entropy rejection", got)
	}
}

func TestUnsafeReviewTextAllowsCommonReviewIdentifiers(t *testing.T) {
	t.Parallel()
	tests := []string{
		"Commit f81d0caa4db2a28627accfd89ad29af292291097 introduces the regression.",
		"Trace ID 019f5693-81ce-4893-8df5-89db82778ac7 identifies the request.",
		"Use ${OPENAI_API_KEY} from the process environment.",
		"The configuration example is FEATURE_FLAG=true.",
	}
	for _, text := range tests {
		if got := unsafeReviewText(text); got != "" {
			t.Errorf("unsafeReviewText(%q) = %q, want safe", text, got)
		}
	}
}

func TestValidateReviewContentSafetyDoesNotEchoSecret(t *testing.T) {
	t.Parallel()
	secret := "q8Kz1Wm9P2vR7xL4nB6cD0fH3jS5uY+/"
	err := validateReviewContentSafety("Finding: "+secret, nil)
	if err == nil {
		t.Fatal("validateReviewContentSafety() error = nil, want rejection")
	}
	if got := err.Error(); got == "" || containsText(got, secret) {
		t.Fatalf("error %q must describe rejection without echoing secret", got)
	}
}

func containsText(value, substring string) bool {
	for i := 0; i+len(substring) <= len(value); i++ {
		if value[i:i+len(substring)] == substring {
			return true
		}
	}
	return false
}

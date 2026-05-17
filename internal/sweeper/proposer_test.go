package sweeper

import (
	"strings"
	"testing"
)

func TestParseNormalizedProposalRejectsTrailingContent(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":80,"summary":"s","rationale":"r"} trailing`,
		`{"schemaVersion":2,"decision":"warn","category":"stale","confidenceScore":80,"summary":"s","rationale":"r"}{"extra":true}`,
	} {
		_, err := parseNormalizedProposal(raw)
		if err == nil {
			t.Fatalf("parseNormalizedProposal(%q) error = nil, want trailing content rejection", raw)
		}
		if !strings.Contains(err.Error(), "trailing content") {
			t.Fatalf("parseNormalizedProposal(%q) error = %v, want trailing content rejection", raw, err)
		}
	}
}

func TestValidateNormalizedProposalRejectsQuarantineOutput(t *testing.T) {
	t.Parallel()

	err := validateNormalizedProposal(normalizedProposal{
		SchemaVersion: 2,
		Decision:      "quarantine",
		Category:      categoryRouteSecurity,
		Confidence:    100,
		Summary:       "security route",
		Rationale:     "detected security-sensitive content",
	}, "warn")
	if err == nil {
		t.Fatal("validateNormalizedProposal() error = nil, want quarantine rejection")
	}
	if !strings.Contains(err.Error(), "prefilter-only") {
		t.Fatalf("validateNormalizedProposal() error = %v, want prefilter-only rejection", err)
	}
}

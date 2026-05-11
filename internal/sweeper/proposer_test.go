package sweeper

import (
	"strings"
	"testing"
)

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

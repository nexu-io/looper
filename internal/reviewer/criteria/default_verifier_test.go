package criteria

import "testing"

func TestDefaultVerifierDoesNotPassSingleTokenCriterionOnOverlapAlone(t *testing.T) {
	t.Parallel()

	assessment, err := NewDefaultVerifier().VerifyCriterion(
		AcceptanceCriterion("add tests"),
		PRDiff{Files: []DiffFile{{Path: "app_test.go", Patch: "@@ -1,1 +1,1 @@\n-old\n+table driven tests cover another path\n"}}},
	)
	if err != nil {
		t.Fatalf("VerifyCriterion() error = %v", err)
	}
	if assessment.Verdict != VerdictUnverifiable {
		t.Fatalf("VerifyCriterion().Verdict = %q, want %q", assessment.Verdict, VerdictUnverifiable)
	}
}

func TestDefaultVerifierStillPassesExactCriterionTextMatch(t *testing.T) {
	t.Parallel()

	assessment, err := NewDefaultVerifier().VerifyCriterion(
		AcceptanceCriterion("add tests"),
		PRDiff{Files: []DiffFile{{Path: "app_test.go", Patch: "@@ -1,1 +1,1 @@\n-old\n+add tests for reviewer auto-merge\n"}}},
	)
	if err != nil {
		t.Fatalf("VerifyCriterion() error = %v", err)
	}
	if assessment.Verdict != VerdictPass {
		t.Fatalf("VerifyCriterion().Verdict = %q, want %q", assessment.Verdict, VerdictPass)
	}
}

func TestDefaultVerifierRequiresDistinctOverlapTokens(t *testing.T) {
	t.Parallel()

	assessment, err := NewDefaultVerifier().VerifyCriterion(
		AcceptanceCriterion("update tests docs"),
		PRDiff{Files: []DiffFile{{Path: "app_test.go", Patch: "@@ -1,1 +1,1 @@\n-old\n+tests tests cover another path\n"}}},
	)
	if err != nil {
		t.Fatalf("VerifyCriterion() error = %v", err)
	}
	if assessment.Verdict != VerdictUnverifiable {
		t.Fatalf("VerifyCriterion().Verdict = %q, want %q", assessment.Verdict, VerdictUnverifiable)
	}
}

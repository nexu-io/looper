package reviewer

import "testing"

func TestDecideSpecReviewGate(t *testing.T) {
	cases := []struct {
		name          string
		requireHuman  bool
		needsHuman    bool
		humanApproved bool
		want          specReviewGateDecision
	}{
		{"gate off → auto-approve", false, false, false, specReviewProceed},
		{"gate off ignores everything", false, true, false, specReviewProceed},
		{"gate on, first clean pass → request human", true, false, false, specReviewRequestHuman},
		{"gate on, already requested, waiting → hold", true, true, false, specReviewHold},
		{"gate on, human approved → proceed", true, false, true, specReviewProceed},
		{"gate on, requested + human approved → proceed", true, true, true, specReviewProceed},
	}
	for _, tc := range cases {
		if got := decideSpecReviewGate(tc.requireHuman, tc.needsHuman, tc.humanApproved); got != tc.want {
			t.Fatalf("%s: decideSpecReviewGate(%v,%v,%v) = %v, want %v", tc.name, tc.requireHuman, tc.needsHuman, tc.humanApproved, got, tc.want)
		}
	}
}

func TestHasHumanApproval(t *testing.T) {
	approved := []map[string]any{
		{"state": "COMMENTED"},
		{"state": "APPROVED"},
	}
	if !hasHumanApproval(approved) {
		t.Fatal("hasHumanApproval = false, want true when an APPROVED review is present")
	}
	// tolerate the alternate "event" key + lowercase
	if !hasHumanApproval([]map[string]any{{"event": "approved"}}) {
		t.Fatal("hasHumanApproval = false, want true for event=approved")
	}
	none := []map[string]any{{"state": "COMMENTED"}, {"state": "CHANGES_REQUESTED"}}
	if hasHumanApproval(none) {
		t.Fatal("hasHumanApproval = true, want false with no APPROVED review")
	}
	if hasHumanApproval(nil) {
		t.Fatal("hasHumanApproval(nil) = true, want false")
	}
}

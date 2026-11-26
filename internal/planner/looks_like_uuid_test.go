package planner

import "testing"

func TestLooksLikeUUID(t *testing.T) {
	yes := []string{"67d4b01c-fea2-4b8f-ac14-a6fff9c9e71b", "DB35F0E7-5004-4632-BA84-074164C95491"}
	no := []string{"lefarcen", "octocat", "", "67d4b01c-fea2-4b8f-ac14", "not-a-uuid-at-all-really-nope-xxxx-yy"}
	for _, s := range yes {
		if !looksLikeUUID(s) {
			t.Fatalf("looksLikeUUID(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikeUUID(s) {
			t.Fatalf("looksLikeUUID(%q) = true, want false", s)
		}
	}
}

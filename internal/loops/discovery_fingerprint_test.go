package loops

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestComputeDiscoveryFingerprintIsStableAndOrderSensitive(t *testing.T) {
	t.Parallel()
	a := ComputeDiscoveryFingerprint("repo", "1", "head")
	b := ComputeDiscoveryFingerprint("repo", "1", "head")
	if a != b {
		t.Fatalf("expected stable fingerprint, got %q vs %q", a, b)
	}
	c := ComputeDiscoveryFingerprint("1", "repo", "head")
	if a == c {
		t.Fatalf("expected order-sensitive fingerprint, got equal")
	}
	if !strings.HasPrefix(a, DiscoveryFingerprintVersion+":") {
		t.Fatalf("expected version prefix on fingerprint, got %q", a)
	}
}

func TestCanonicalSortedStringsTrimsLowercasesDedupesAndSorts(t *testing.T) {
	t.Parallel()
	got := CanonicalSortedStrings([]string{"  Bug ", "feature", "BUG", "", "feature"})
	want := []string{"bug", "feature"}
	if len(got) != len(want) {
		t.Fatalf("CanonicalSortedStrings = %#v, want %#v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("CanonicalSortedStrings = %#v, want %#v", got, want)
		}
	}
}

func TestMergeAndReadLastFailedDiscoveryFingerprintRoundTrip(t *testing.T) {
	t.Parallel()
	merged, err := MergeLastFailedDiscoveryFingerprint(nil, "v1:abc")
	if err != nil {
		t.Fatalf("merge into nil error = %v", err)
	}
	got := LastFailedDiscoveryFingerprint(&merged)
	if got != "v1:abc" {
		t.Fatalf("LastFailedDiscoveryFingerprint = %q, want %q", got, "v1:abc")
	}

	// Existing unrelated metadata must survive.
	existing := `{"prUrl":"https://example.com/pr/1"}`
	merged2, err := MergeLastFailedDiscoveryFingerprint(&existing, "v1:def")
	if err != nil {
		t.Fatalf("merge into existing error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(merged2), &decoded); err != nil {
		t.Fatalf("merged JSON invalid: %v", err)
	}
	if decoded["prUrl"] != "https://example.com/pr/1" {
		t.Fatalf("prUrl lost after merge, got %#v", decoded)
	}
	if got := LastFailedDiscoveryFingerprint(&merged2); got != "v1:def" {
		t.Fatalf("LastFailedDiscoveryFingerprint after merge = %q, want %q", got, "v1:def")
	}

	// Clearing should remove the field but keep other metadata.
	cleared, err := MergeLastFailedDiscoveryFingerprint(&merged2, "")
	if err != nil {
		t.Fatalf("clear merge error = %v", err)
	}
	if got := LastFailedDiscoveryFingerprint(&cleared); got != "" {
		t.Fatalf("LastFailedDiscoveryFingerprint after clear = %q, want empty", got)
	}
	if !strings.Contains(cleared, "prUrl") {
		t.Fatalf("expected prUrl preserved after clear, got %q", cleared)
	}
}

func TestShouldSuppressFailedRediscoveryRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  string
		stored  string
		current string
		want    bool
	}{
		{name: "failed equal", status: "failed", stored: "v1:a", current: "v1:a", want: true},
		{name: "failed mismatch", status: "failed", stored: "v1:a", current: "v1:b", want: false},
		{name: "failed missing stored", status: "failed", stored: "", current: "v1:a", want: false},
		{name: "failed missing current", status: "failed", stored: "v1:a", current: "", want: false},
		{name: "queued never suppress", status: "queued", stored: "v1:a", current: "v1:a", want: false},
		{name: "running never suppress", status: "running", stored: "v1:a", current: "v1:a", want: false},
		{name: "paused never suppress", status: "paused", stored: "v1:a", current: "v1:a", want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ShouldSuppressFailedRediscovery(tc.status, tc.stored, tc.current); got != tc.want {
				t.Fatalf("ShouldSuppressFailedRediscovery(%q,%q,%q) = %v, want %v", tc.status, tc.stored, tc.current, got, tc.want)
			}
		})
	}
}

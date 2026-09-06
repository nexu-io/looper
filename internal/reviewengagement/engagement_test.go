package reviewengagement

import "testing"

func TestResolveUsesStoredAuthorityAndPreservesColdStart(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		loads           int
	}{
		{"stored", `{"followUpdates":true,"lastPublishedHeadSha":"stored"}`, "stored", 0},
		{"recover", `{"followUpdates":true}`, "remote", 1},
		{"disabled", `{"followUpdates":false}`, "", 0},
		{"loop disabled", `{"followUpdates":true,"loop":{"enabled":false}}`, "", 0},
		{"manual", `{"followUpdates":true,"manual":true}`, "", 0},
		{"missing", `{}`, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loads := 0
			got, err := Resolve(&tc.raw, "new", func() (string, error) { loads++; return "remote", nil })
			if err != nil || got != tc.want || loads != tc.loads {
				t.Fatalf("Resolve = %q, %v, loads %d", got, err, loads)
			}
		})
	}
}

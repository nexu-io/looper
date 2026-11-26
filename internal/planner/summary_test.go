package planner

import "testing"

// A grill/review conclusion that came back as a bare number — a stray token/byte count
// grabbed from the agent's last log line when it emitted no __LOOPER_RESULT__ summary —
// must be replaced with a placeholder, never posted as "🔬 GRILL 拷问结论: 105,940".
func TestCleanAgentSummaryRejectsBareNumber(t *testing.T) {
	const placeholder = "(本轮 agent 未产出可展示的结论)"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare number with comma", "105,940", placeholder},
		{"padded number", "  42  ", placeholder},
		{"decimal + percent", "3.14%", placeholder},
		{"empty", "", placeholder},
		{"ansi reset with esc", "\x1b[0m", placeholder},
		{"csi residue esc stripped", "[0m", placeholder},
		{"multiple csi fragments", "[2K[1G[0m", placeholder},
		{"prose after stripping ansi kept", "\x1b[0mTightened the acceptance criteria.\x1b[0m", "Tightened the acceptance criteria."},
		{"real transcript kept", "Challenged the CI claim, tightened the acceptance criteria, one open question remains.", "Challenged the CI claim, tightened the acceptance criteria, one open question remains."},
		{"number inside prose kept", "Reduced the diff to 42 lines and verified runs.ts:199.", "Reduced the diff to 42 lines and verified runs.ts:199."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanAgentSummary(c.in); got != c.want {
				t.Fatalf("cleanAgentSummary(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

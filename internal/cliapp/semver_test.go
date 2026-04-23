package cliapp

import "testing"

func TestIsSemverUpgradeAvailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{name: "equal", current: "0.2.1", latest: "0.2.1", want: false},
		{name: "older current", current: "0.2.1", latest: "0.3.0", want: true},
		{name: "newer current", current: "0.4.0", latest: "0.3.0", want: false},
		{name: "stable newer than prerelease", current: "0.3.0", latest: "0.3.0-rc.1", want: false},
		{name: "prerelease older than stable", current: "0.3.0-rc.1", latest: "0.3.0", want: true},
		{name: "numeric prerelease newer", current: "0.3.0-rc.2", latest: "0.3.0-rc.10", want: true},
		{name: "numeric prerelease older", current: "0.3.0-rc.10", latest: "0.3.0-rc.2", want: false},
		{name: "numeric identifier lower precedence than text", current: "0.3.0-1", latest: "0.3.0-alpha", want: true},
		{name: "longer prerelease newer when prefix equal", current: "0.3.0-alpha", latest: "0.3.0-alpha.1", want: true},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := isSemverUpgradeAvailable(testCase.current, testCase.latest)
			if err != nil {
				t.Fatalf("isSemverUpgradeAvailable(%q, %q) error = %v", testCase.current, testCase.latest, err)
			}
			if got != testCase.want {
				t.Fatalf("isSemverUpgradeAvailable(%q, %q) = %v, want %v", testCase.current, testCase.latest, got, testCase.want)
			}
		})
	}
}

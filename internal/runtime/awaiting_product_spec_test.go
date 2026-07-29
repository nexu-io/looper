package runtime

import "testing"

func TestShouldResumeForProductSpec(t *testing.T) {
	cases := []struct {
		name                            string
		loopType, status                string
		hasSpecPR, hasProductSpec, want bool
	}{
		{"planner paused, spec appeared, no PR yet", "planner", "paused", false, true, true},
		{"no product spec yet → wait", "planner", "paused", false, false, false},
		{"already opened a spec PR → past the gate", "planner", "paused", true, true, false},
		{"not paused → not waiting", "planner", "running", false, true, false},
		{"not a planner loop", "worker", "paused", false, true, false},
	}
	for _, tc := range cases {
		if got := shouldResumeForProductSpec(tc.loopType, tc.status, tc.hasSpecPR, tc.hasProductSpec); got != tc.want {
			t.Fatalf("%s: shouldResumeForProductSpec = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLoopIssueURLAndSpecPR(t *testing.T) {
	meta := `{"loopType":"planner","issueUrl":"https://plane.x/w/projects/pp/issues/wi-9","prUrl":"https://github.com/o/r/pull/3"}`
	url, hasPR := loopIssueURLAndSpecPR(&meta)
	if url != "https://plane.x/w/projects/pp/issues/wi-9" || !hasPR {
		t.Fatalf("got %q, %v; want the issue url + hasSpecPR=true", url, hasPR)
	}
	meta2 := `{"issueUrl":"https://plane.x/w/projects/pp/issues/wi-9"}`
	if url, hasPR := loopIssueURLAndSpecPR(&meta2); url == "" || hasPR {
		t.Fatalf("got %q, %v; want url + hasSpecPR=false (no prUrl)", url, hasPR)
	}
	if url, hasPR := loopIssueURLAndSpecPR(nil); url != "" || hasPR {
		t.Fatalf("nil metadata = %q, %v; want empty", url, hasPR)
	}
}

package loops

import "testing"

func TestNormalizeResumePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		failureKind  string
		resumePolicy string
		want         string
	}{
		{name: "preserves explicit", failureKind: FailureKindManualIntervention, resumePolicy: ResumePolicyRestartFromDiscover, want: ResumePolicyRestartFromDiscover},
		{name: "retryable after resume defaults to checkpoint advance", failureKind: FailureKindRetryableAfterResume, want: ResumePolicyAdvanceFromCheckpoint},
		{name: "manual intervention defaults to manual hold", failureKind: FailureKindManualIntervention, want: ResumePolicyManualIntervention},
		{name: "other failures default to replay", failureKind: "retryable_transient", want: ResumePolicyReplayStep},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeResumePolicy(tt.failureKind, tt.resumePolicy); got != tt.want {
				t.Fatalf("NormalizeResumePolicy(%q, %q) = %q, want %q", tt.failureKind, tt.resumePolicy, got, tt.want)
			}
		})
	}
}

func TestShouldRestartFromDiscover(t *testing.T) {
	t.Parallel()

	if !ShouldRestartFromDiscover("failed", ResumePolicyRestartFromDiscover) {
		t.Fatal("ShouldRestartFromDiscover() = false, want true for restart_from_discover")
	}
	if ShouldRestartFromDiscover("failed", ResumePolicyManualIntervention) {
		t.Fatal("ShouldRestartFromDiscover() = true, want false for manual_intervention")
	}
}

func TestManualHoldPolicy(t *testing.T) {
	t.Parallel()

	if !SuppressesAutonomousRecovery(FailureKindManualIntervention, "") {
		t.Fatal("SuppressesAutonomousRecovery() = false, want true for hard hold")
	}
	if SuppressesAutonomousRecovery(FailureKindRetryableAfterResume, ResumePolicyRestartFromDiscover) {
		t.Fatal("SuppressesAutonomousRecovery() = true, want false for safe rediscovery")
	}
	if !IsManualHoldResumePolicy(ResumePolicyManualIntervention) {
		t.Fatal("IsManualHoldResumePolicy() = false, want true")
	}
}

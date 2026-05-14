package loops

import (
	"strings"

	"github.com/nexu-io/looper/internal/storage"
)

const (
	FailureKindRetryableAfterResume = "retryable_after_resume"
	FailureKindManualIntervention   = "manual_intervention"

	ResumePolicyAdvanceFromCheckpoint = "advance_from_checkpoint"
	ResumePolicyManualIntervention    = "manual_intervention"
	ResumePolicyReplayStep            = "replay_step"
	ResumePolicyRestartFromDiscover   = "restart_from_discover"
)

func NormalizeResumePolicy(failureKind, resumePolicy string) string {
	policy := strings.TrimSpace(resumePolicy)
	if policy != "" {
		return policy
	}
	switch strings.TrimSpace(failureKind) {
	case FailureKindRetryableAfterResume:
		return ResumePolicyAdvanceFromCheckpoint
	case FailureKindManualIntervention:
		return ResumePolicyManualIntervention
	default:
		return ResumePolicyReplayStep
	}
}

func IsManualHoldResumePolicy(resumePolicy string) bool {
	return strings.TrimSpace(resumePolicy) == ResumePolicyManualIntervention
}

func IsHardHold(failureKind, resumePolicy string) bool {
	if IsManualHoldResumePolicy(resumePolicy) {
		return true
	}
	return strings.TrimSpace(failureKind) == FailureKindManualIntervention
}

func SuppressesAutonomousRecovery(failureKind, resumePolicy string) bool {
	return IsHardHold(failureKind, resumePolicy)
}

func ShouldRestartFromDiscover(status, resumePolicy string) bool {
	if status != "failed" && status != "interrupted" {
		return false
	}
	return strings.TrimSpace(resumePolicy) == ResumePolicyRestartFromDiscover
}

func ShouldPauseLoopAfterFailure(failureKind string, failedQueue *storage.QueueItemRecord, resumePolicy string) bool {
	if failedQueue != nil && failedQueue.Status == "cancelled" {
		return true
	}
	return IsHardHold(failureKind, resumePolicy)
}

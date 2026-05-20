package networkpolicy

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestEvaluateWorkerIgnoresTargetLabelsForLocalOnlyProjects(t *testing.T) {
	t.Parallel()
	decision := EvaluateWorker(ProjectPolicy{Mode: config.NetworkModeOff}, []string{"looper:worker-ready", "looper:target:red"}, nil)
	if !decision.Allowed {
		t.Fatalf("decision = %#v, want allowed local-only worker claim", decision)
	}
}

func TestEvaluateWorkerRequiresExactOneMatchingTargetAndCoarseAuthority(t *testing.T) {
	t.Parallel()
	policy := ProjectPolicy{Mode: config.NetworkModeRouted, NodeName: "red", GitHubLogin: "worker", GitHubUserID: 42}
	decision := EvaluateWorker(policy, []string{"looper:worker-ready", "looper:target:red"}, []GitHubUser{{Login: "worker", ID: 42}})
	if !decision.Allowed || decision.MatchMode != MatchModeNumeric {
		t.Fatalf("decision = %#v, want allowed numeric worker match", decision)
	}

	blocked := EvaluateWorker(policy, []string{"looper:worker-ready", "looper:target:red", "looper:target:blue"}, []GitHubUser{{Login: "worker", ID: 42}})
	if blocked.Allowed || blocked.Reason == "" {
		t.Fatalf("blocked = %#v, want multiple-target failure", blocked)
	}
}

func TestEvaluateReviewerFallsBackToLoginWhenNumericIDsUnavailable(t *testing.T) {
	t.Parallel()
	policy := ProjectPolicy{Mode: config.NetworkModeRouted, NodeName: "red", GitHubLogin: "reviewer", GitHubUserID: 42}
	decision := EvaluateReviewer(policy, []string{"looper:target:red"}, []GitHubUser{{Login: "reviewer"}})
	if !decision.Allowed || decision.MatchMode != MatchModeLoginFallback {
		t.Fatalf("decision = %#v, want allowed login fallback reviewer match", decision)
	}
}

func TestEvaluateTargetAcceptsCaseInsensitiveTargetPrefix(t *testing.T) {
	t.Parallel()
	policy := ProjectPolicy{Mode: config.NetworkModeRouted, NodeName: "red", GitHubLogin: "worker"}
	decision := EvaluateWorker(policy, []string{"looper:worker-ready", "Looper:Target:red"}, []GitHubUser{{Login: "worker"}})
	if !decision.Allowed {
		t.Fatalf("decision = %#v, want allowed worker claim for mixed-case target prefix", decision)
	}
	if decision.TargetLabel != "Looper:Target:red" {
		t.Fatalf("decision.TargetLabel = %q, want original label preserved", decision.TargetLabel)
	}
	if decision.MatchMode != MatchModeLoginFallback {
		t.Fatalf("decision.MatchMode = %q, want login fallback", decision.MatchMode)
	}
}

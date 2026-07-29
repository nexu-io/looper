package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestDecideAutoIntakeRoute(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		decision       triage.Decision
		hasProductSpec bool
		preSpecGrill   bool
		want           intakeRoute
	}{
		{
			name:     "noop classification leaves the item untouched",
			decision: triage.Decision{NoOp: true},
			want:     intakeSkip,
		},
		{
			name:     "out of scope is marked and stopped",
			decision: triage.Decision{Disposition: triage.DispositionOutOfScope},
			want:     intakeMarkOutOfScope,
		},
		{
			name:     "unclear holds for a human",
			decision: triage.Decision{Disposition: triage.DispositionUnclear},
			want:     intakeHoldUnclear,
		},
		{
			name:           "feature without a product spec holds at node E",
			decision:       triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/feature", "dispatch/plan"}},
			hasProductSpec: false,
			want:           intakeHoldForProductSpec,
		},
		{
			name:           "feature with a product spec routes to the planner",
			decision:       triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/feature", "dispatch/plan"}},
			hasProductSpec: true,
			want:           intakeRouteToPlan,
		},
		{
			name:         "V2 feature without product spec researches before deciding the formal spec gate",
			decision:     triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/feature", "dispatch/plan"}},
			preSpecGrill: true,
			want:         intakeRouteToPlan,
		},
		{
			name:     "simple bug (dispatch/implement) goes straight to the worker",
			decision: triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/bug", "complexity/s", "dispatch/implement"}},
			want:     intakeRouteToImplement,
		},
		{
			name:     "complex bug (dispatch/plan) writes a tech spec first",
			decision: triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/bug", "complexity/l", "dispatch/plan"}},
			want:     intakeRouteToPlan,
		},
		{
			name:     "valid with no dispatch label falls back to spec-first",
			decision: triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/docs"}},
			want:     intakeRouteToPlan,
		},
		{
			name:     "dispatch label matched case-insensitively",
			decision: triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"Dispatch/Implement"}},
			want:     intakeRouteToImplement,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideAutoIntakeRoute(c.decision, c.hasProductSpec, c.preSpecGrill); got != c.want {
				t.Fatalf("decideAutoIntakeRoute() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestLabelsContainFold(t *testing.T) {
	t.Parallel()

	labels := []string{"looper:auto", " Kind/Bug ", "dispatch/implement"}
	if !labelsContainFold(labels, "kind/bug") {
		t.Fatal("labelsContainFold should match case-insensitively and trim whitespace")
	}
	if !labelsContainFold(labels, "LOOPER:AUTO") {
		t.Fatal("labelsContainFold should match looper:auto")
	}
	if labelsContainFold(labels, "looper:plan") {
		t.Fatal("labelsContainFold should not match an absent label")
	}
}

func TestIntakeOutcomeKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		route intakeRoute
		want  string
	}{
		{intakeRouteToPlan, "routed_plan"},
		{intakeRouteToImplement, "routed_worker"},
		{intakeHoldForProductSpec, "hold_product"},
		{intakeMarkOutOfScope, "out_of_scope"},
		{intakeHoldUnclear, "needs_human"},
		{intakeSkip, "needs_human"},
	}
	for _, tc := range cases {
		if got := intakeOutcomeKey(tc.route); got != tc.want {
			t.Fatalf("intakeOutcomeKey(%d) = %q, want %q", tc.route, got, tc.want)
		}
	}
}

func TestAutoIntakeEnabledGate(t *testing.T) {
	t.Setenv(autoIntakeEnvVar, "")
	if autoIntakeEnabled() {
		t.Fatal("auto-intake must be off when the env var is unset")
	}
	t.Setenv(autoIntakeEnvVar, "1")
	if !autoIntakeEnabled() {
		t.Fatal("auto-intake must be on when the env var is 1")
	}
}

func TestAutoIntakeProjectDisabledWhenPlannerAndWorkerDiscoveryAreOff(t *testing.T) {
	cfg := config.Config{Roles: config.RoleConfigs{}}
	if autoIntakeProjectEnabled(cfg, "isolated") {
		t.Fatal("planner-only isolated project must not run auto-intake")
	}
	cfg.Roles.Planner.AutoDiscovery = true
	if !autoIntakeProjectEnabled(cfg, "isolated") {
		t.Fatal("planner discovery should enable auto-intake routing")
	}
}

func TestAutoIntakeV2RetiresLegacyProductSpecHoldAndRoutesPlanner(t *testing.T) {
	labels := []string{"await-id"}
	productSpecLookups := 0
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
		joined := strings.Join(options.Args, "\x00")
		switch {
		case strings.Contains(joined, "api\x00label\x00list"):
			return shell.Result{Stdout: `{"results":[{"id":"await-id","name":"looper:awaiting-product-spec"},{"id":"plan-id","name":"looper:plan"}]}`}, nil
		case strings.Contains(joined, "api\x00work-item\x00get"):
			encoded, _ := json.Marshal(map[string]any{"labels": labels})
			return shell.Result{Stdout: string(encoded)}, nil
		case strings.Contains(joined, "api\x00work-item\x00update"):
			for i, arg := range options.Args {
				if arg == "--data" && i+1 < len(options.Args) {
					var payload map[string][]string
					_ = json.Unmarshal([]byte(options.Args[i+1]), &payload)
					labels = payload["labels"]
				}
			}
			return shell.Result{Stdout: `{}`}, nil
		case strings.Contains(joined, "api\x00link\x00list"):
			productSpecLookups++
			return shell.Result{Stdout: `{"results":[]}`}, nil
		default:
			return shell.Result{Stdout: `{}`}, nil
		}
	}})
	runtime := &Runtime{}
	runtime.reconcileAutoIntakeItem(context.Background(), gateway, nil, "plane-project", config.ProjectRefConfig{ID: "project", Repo: "acme/looper"}, forge.Issue{Number: 1595, HTMLURL: "https://plane.example/workspace/projects/plane-project/issues/wi-1595", Labels: []forge.Label{{Name: intakeAwaitingProductLabel}}}, nil, nil, time.Now, nil, true)
	if len(labels) != 1 || labels[0] != "plan-id" {
		t.Fatalf("V2 legacy hold did not converge to planner label: %#v", labels)
	}
	if productSpecLookups != 0 {
		t.Fatalf("V2 legacy hold unexpectedly required product spec: lookups=%d", productSpecLookups)
	}
}

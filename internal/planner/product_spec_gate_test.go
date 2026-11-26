package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func productOwnerConfig(planeID string) *config.Config {
	return &config.Config{Projects: []config.ProjectRefConfig{{ID: "proj-1", ProductOwner: &config.ProductOwnerConfig{PlaneID: planeID}}}}
}

// scriptedGateway builds a planedoc gateway whose plane CLI returns the given
// stdouts in order, and records the invocations.
func scriptedGateway(stdouts ...string) (*planedoc.Gateway, *[][]string) {
	calls := &[][]string{}
	i := 0
	gw := planedoc.New(planedoc.Options{
		APIKey: "k", Workspace: "w", APIBaseURL: "https://plane.x/api/v1",
		Run: func(_ context.Context, o shell.Options) (shell.Result, error) {
			*calls = append(*calls, o.Args)
			out := ""
			if i < len(stdouts) {
				out = stdouts[i]
			}
			i++
			return shell.Result{Stdout: out}, nil
		},
	})
	return gw, calls
}

func gateInput(labels []string) (stepInput, plannerCheckpoint) {
	url := "https://plane.x/w/projects/pp/issues/wi-9"
	cp := plannerCheckpoint{Issue: &checkpointIssue{Repo: "owner/repo", IssueNumber: 9, Title: "登录", URL: url, Labels: labels}}
	in := stepInput{Project: storage.ProjectRecord{ID: "proj-1"}, Checkpoint: cp}
	return in, cp
}

func TestProductSpecGateHoldsFeatureWithoutSpec(t *testing.T) {
	// link list (no product-spec) → exact-ask lookup → comment create
	gw, calls := scriptedGateway(`{"results":[]}`, `{"results":[]}`, `{"id":"c1"}`)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj-uuid", true }}
	in, cp := gateInput([]string{"kind/feature", "looper:plan"})

	gateErr := r.productSpecGate(context.Background(), in, cp)
	if gateErr == nil || gateErr.kind != FailureManualIntervention {
		t.Fatalf("gate = %v, want a manual-intervention hold", gateErr)
	}
	if len(*calls) != 3 {
		t.Fatalf("calls = %d, want link list + ask lookup + comment create", len(*calls))
	}
	// asked product on the work item
	comment := (*calls)[2]
	if !strings.Contains(strings.Join(comment, " "), "api request workspaces/w/projects/plane-proj-uuid/work-items/wi-9/comments/ --method POST") {
		t.Fatalf("third call = %v, want comment create", comment)
	}
}

func TestProductSpecGateProceedsWhenSpecPresent(t *testing.T) {
	gw, calls := scriptedGateway(
		`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://plane.x/w/projects/pp/pages/pg1"}]}`,
		`{"id":"pg1","description_html":"<p>目标：首版导出 React + CSS。验收：产物可独立运行。</p>","created_by":"product-owner"}`,
	)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj-uuid", true }, projectRoleConfig: productOwnerConfig("product-owner")}
	in, cp := gateInput([]string{"kind/feature", "looper:plan"})
	cp.Issue.ProductSpec = "stale content from an earlier checkpoint"
	cp.Issue.ProductSpecURL = "https://plane.x/old"
	if gateErr := r.productSpecGate(context.Background(), in, cp); gateErr != nil {
		t.Fatalf("gate = %v, want nil (has product spec → proceed)", gateErr)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want link list + product page read (no comment)", len(*calls))
	}
	if cp.Issue.ProductSpec == "" || cp.Issue.ProductSpecURL == "" {
		t.Fatalf("issue product spec = %q, %q; want authoritative content + URL", cp.Issue.ProductSpec, cp.Issue.ProductSpecURL)
	}
}

func TestProductSpecGateHoldsEmptyLinkedSpec(t *testing.T) {
	gw, calls := scriptedGateway(
		`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://plane.x/w/projects/pp/pages/pg1"}]}`,
		`{"id":"pg1","description_html":" ","created_by":"product-owner"}`,
		`{"results":[]}`,
		`{"id":"c-empty"}`,
	)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj-uuid", true }, projectRoleConfig: productOwnerConfig("product-owner")}
	in, cp := gateInput([]string{"kind/feature", "looper:plan"})
	cp.Issue.ProductSpec = "stale content from an earlier checkpoint"
	cp.Issue.ProductSpecURL = "https://plane.x/old"

	gateErr := r.productSpecGate(context.Background(), in, cp)
	if gateErr == nil || gateErr.kind != FailureManualIntervention {
		t.Fatalf("gate = %v, want empty linked spec to hold", gateErr)
	}
	if len(*calls) != 4 {
		t.Fatalf("calls = %d, want link read + page read + ask lookup + comment", len(*calls))
	}
	if cp.Issue.ProductSpec != "" || cp.Issue.ProductSpecURL != "" {
		t.Fatalf("stale product spec survived failed revalidation: %q, %q", cp.Issue.ProductSpec, cp.Issue.ProductSpecURL)
	}
}

func TestProductSpecGateRejectsPageAuthoredByLooperOwner(t *testing.T) {
	gw, calls := scriptedGateway(
		`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://plane.x/w/projects/pp/pages/pg1"}]}`,
		`{"id":"pg1","description_html":"<p>E2E draft written by Looper owner</p>","created_by":"looper-owner","updated_by":"looper-owner","owned_by":"looper-owner"}`,
		`{"results":[]}`,
		`{"id":"ask-product"}`,
	)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj-uuid", true }, projectRoleConfig: productOwnerConfig("product-owner")}
	in, cp := gateInput([]string{"kind/feature", "looper:plan"})

	gateErr := r.productSpecGate(context.Background(), in, cp)
	if gateErr == nil || gateErr.kind != FailureManualIntervention {
		t.Fatalf("gate = %v, want untrusted page to hold", gateErr)
	}
	if len(*calls) != 4 {
		t.Fatalf("calls = %d, want link + page provenance + ask lookup + comment", len(*calls))
	}
	if cp.Issue.ProductSpec != "" {
		t.Fatalf("untrusted product spec leaked into planner prompt: %q", cp.Issue.ProductSpec)
	}
}

func TestProductSpecGateSkipsNonFeatureAndNonPlane(t *testing.T) {
	gw, calls := scriptedGateway(`{"results":[]}`)
	// a bug → no gate (bugs don't need a product spec)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "pp", true }}
	inBug, cpBug := gateInput([]string{"kind/bug", "looper:plan"})
	if gateErr := r.productSpecGate(context.Background(), inBug, cpBug); gateErr != nil {
		t.Fatalf("bug gate = %v, want nil", gateErr)
	}
	if len(*calls) != 0 {
		t.Fatalf("bug made %d plane calls, want 0", len(*calls))
	}
	// a github project (planeDoc → false) → no gate
	rGH := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return nil, "", false }}
	inF, cpF := gateInput([]string{"kind/feature"})
	if gateErr := rGH.productSpecGate(context.Background(), inF, cpF); gateErr != nil {
		t.Fatalf("github gate = %v, want nil", gateErr)
	}
	// no planeDoc resolver at all → no gate
	rNil := &Runner{}
	if gateErr := rNil.productSpecGate(context.Background(), inF, cpF); gateErr != nil {
		t.Fatalf("nil resolver gate = %v, want nil", gateErr)
	}
}

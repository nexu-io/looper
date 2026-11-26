package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func scriptedGateway(stdouts ...string) *planedoc.Gateway {
	i := 0
	return planedoc.New(planedoc.Options{
		APIKey: "k", Workspace: "w", APIBaseURL: "https://plane.x/api/v1",
		Run: func(_ context.Context, o shell.Options) (shell.Result, error) {
			out := ""
			if i < len(stdouts) {
				out = stdouts[i]
			}
			i++
			return shell.Result{Stdout: out}, nil
		},
	})
}

const linksJSON = `{"results":[{"id":"lp","title":"looper:product-spec","url":"https://plane.x/w/projects/pp/pages/prod"},{"id":"lt","title":"looper:tech-spec","url":"https://plane.x/w/projects/pp/pages/tech"}]}`

func TestPlaneSpecBlockReadsProductAndTech(t *testing.T) {
	// ReadSpec(product): link list → page get; ReadSpec(tech): link list → page get.
	gw := scriptedGateway(linksJSON, "<h1>Product</h1>", linksJSON, "<h1>Tech</h1>")
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "pp", true }}
	block := r.planeSpecBlock(context.Background(), "proj-1", "https://plane.x/w/projects/pp/issues/wi-9")
	if !strings.Contains(block, "Product spec (from Plane)") || !strings.Contains(block, "<h1>Product</h1>") {
		t.Fatalf("block missing product spec:\n%s", block)
	}
	if !strings.Contains(block, "Tech spec (from Plane)") || !strings.Contains(block, "<h1>Tech</h1>") {
		t.Fatalf("block missing tech spec:\n%s", block)
	}
}

func TestPlaneSpecBlockEmptyWhenNoLinksOrNonPlane(t *testing.T) {
	// no spec links → empty
	gw := scriptedGateway(`{"results":[]}`, `{"results":[]}`)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "pp", true }}
	if b := r.planeSpecBlock(context.Background(), "proj-1", "https://plane.x/w/projects/pp/issues/wi-9"); b != "" {
		t.Fatalf("block = %q, want empty when no spec links", b)
	}
	// github project → empty (no plane read)
	rGH := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return nil, "", false }}
	if b := rGH.planeSpecBlock(context.Background(), "proj-1", "https://plane.x/w/projects/pp/issues/wi-9"); b != "" {
		t.Fatalf("github block = %q, want empty", b)
	}
	// unresolvable work item (non-plane URL) → empty
	if b := r.planeSpecBlock(context.Background(), "proj-1", "not-a-url"); b != "" {
		t.Fatalf("unresolvable block = %q, want empty", b)
	}
	// nil resolver → empty
	rNil := &Runner{}
	if b := rNil.planeSpecBlock(context.Background(), "proj-1", "https://plane.x/w/projects/pp/issues/wi-9"); b != "" {
		t.Fatalf("nil resolver block = %q, want empty", b)
	}
}

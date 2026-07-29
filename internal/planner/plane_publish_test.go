package planner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/storage"
)

func TestReadPlannerSpecFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "specs", "s.md"), []byte("# Tech Spec\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readPlannerSpecFile(dir, "specs/s.md")
	if err != nil || got != "# Tech Spec\nbody" {
		t.Fatalf("readPlannerSpecFile = %q, %v", got, err)
	}
	// missing file → empty, no error
	if got, err := readPlannerSpecFile(dir, "specs/missing.md"); err != nil || got != "" {
		t.Fatalf("missing = %q, %v; want empty", got, err)
	}
	// empty path → empty
	if got, err := readPlannerSpecFile(dir, ""); err != nil || got != "" {
		t.Fatalf("empty path = %q, %v", got, err)
	}
}

func TestPublishTechSpecToPlaneWritesPageAndLinks(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "s.md"), []byte("# Tech Spec"), 0o644)

	// FindSpecLink(tech) empty → CreatePage → UpsertSpecLink list empty → link create
	gw, calls := scriptedGateway(`{"results":[]}`, `{"id":"pg-1","name":"Tech Spec: 登录"}`, `{"results":[]}`, `{"id":"l-new"}`)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj", true }}
	in := stepInput{Project: storage.ProjectRecord{ID: "proj-1"}}
	issue := checkpointIssue{Title: "登录", URL: "https://plane.x/w/projects/pp/issues/wi-9", SpecPath: "specs/s.md"}
	wt := checkpointWorktree{Path: dir, SpecPath: "specs/s.md"}

	if err := r.publishTechSpecToPlane(context.Background(), in, issue, wt); err != nil {
		t.Fatalf("publishTechSpecToPlane error = %v", err)
	}
	if len(*calls) != 4 {
		t.Fatalf("calls = %d, want find + page create + upsert list + link create", len(*calls))
	}
	if (*calls)[1][1] != "page" || (*calls)[1][2] != "create" {
		t.Fatalf("2nd call = %v, want page create", (*calls)[1])
	}
}

func TestPublishTechSpecToPlaneIdempotentAndNonPlane(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "s.md"), []byte("# Tech Spec"), 0o644)
	issue := checkpointIssue{Title: "登录", URL: "https://plane.x/w/projects/pp/issues/wi-9", SpecPath: "specs/s.md"}
	wt := checkpointWorktree{Path: dir, SpecPath: "specs/s.md"}
	in := stepInput{Project: storage.ProjectRecord{ID: "proj-1"}}

	// already has a tech-spec link → no-op (only the find call)
	gw, calls := scriptedGateway(`{"results":[{"id":"l1","title":"looper:tech-spec","url":"https://plane.x/pages/p1"}]}`)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj", true }}
	if err := r.publishTechSpecToPlane(context.Background(), in, issue, wt); err != nil {
		t.Fatalf("idempotent error = %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("idempotent calls = %d, want only the find (no page create)", len(*calls))
	}

	// github project (planeDoc false) → no-op
	rGH := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return nil, "", false }}
	if err := rGH.publishTechSpecToPlane(context.Background(), in, issue, wt); err != nil {
		t.Fatalf("github error = %v", err)
	}
}

// TestRunPlanePublishStepWritesSpecNoPR: the Plane-provider publish path writes the
// tech spec to Plane (node G), verifies it landed, marks PlaneSpecReview, and opens
// NO pull request — the impl PR is the worker's job after review (node H).
func TestRunPlanePublishStepWritesSpecNoPR(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "specs"), 0o755)
	os.WriteFile(filepath.Join(dir, "specs", "s.md"), []byte("# Tech Spec\n验收: e2e"), 0o644)

	// publishTechSpecToPlane: find(empty)→page create→upsert list(empty)→link create;
	// verify find → tech-spec link; then node H review-open: list comments(empty)→create.
	gw, _ := scriptedGateway(
		`{"results":[]}`,
		`{"id":"pg-1","name":"Tech Spec: 登录"}`,
		`{"results":[]}`,
		`{"id":"l-new"}`,
		`{"results":[{"id":"l-new","title":"looper:tech-spec","url":"https://plane.x/pages/pg-1"}]}`,
		`{"results":[]}`,
		`{"id":"c-review","comment_html":"<p>[looper] ...</p>","comment_stripped":"[looper] ..."}`,
	)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj", true }}
	in := stepInput{
		Project: storage.ProjectRecord{ID: "proj-1"},
		Checkpoint: plannerCheckpoint{
			Issue:    &checkpointIssue{Title: "登录", Repo: "o/r", IssueNumber: 9, URL: "https://plane.x/w/projects/pp/issues/wi-9", SpecPath: "specs/s.md"},
			Worktree: &checkpointWorktree{Path: dir, SpecPath: "specs/s.md"},
		},
	}
	cp, err := r.runPlanePublishStep(context.Background(), in, gw, "plane-proj")
	if err != nil {
		t.Fatalf("runPlanePublishStep error = %v", err)
	}
	if cp.Publish == nil || !cp.Publish.PlaneSpecReview {
		t.Fatalf("PlaneSpecReview not set: %+v", cp.Publish)
	}
	if cp.Publish.PullRequest != nil {
		t.Fatalf("Plane path must not open a PR, got %+v", cp.Publish.PullRequest)
	}
}

// TestRunPlanePublishStepHoldsWhenNoSpec: if the agent wrote no spec file there is
// nothing to review and no PR fallback on Plane — hold for a human rather than
// completing empty.
func TestRunPlanePublishStepHoldsWhenNoSpec(t *testing.T) {
	dir := t.TempDir() // no spec file written
	// publishTechSpecToPlane: find(empty) → read spec(empty) → returns nil (one call);
	// then runPlanePublishStep verify find → still empty.
	gw, _ := scriptedGateway(`{"results":[]}`, `{"results":[]}`)
	r := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return gw, "plane-proj", true }}
	in := stepInput{
		Project: storage.ProjectRecord{ID: "proj-1"},
		Checkpoint: plannerCheckpoint{
			Issue:    &checkpointIssue{Title: "登录", Repo: "o/r", IssueNumber: 9, URL: "https://plane.x/w/projects/pp/issues/wi-9", SpecPath: "specs/missing.md"},
			Worktree: &checkpointWorktree{Path: dir, SpecPath: "specs/missing.md"},
		},
	}
	_, err := r.runPlanePublishStep(context.Background(), in, gw, "plane-proj")
	var le *loopError
	if !errors.As(err, &le) || le.kind != FailureManualIntervention {
		t.Fatalf("want manual-intervention hold when no spec file, got %v", err)
	}
}

// TestGrillReviewNoOpForGitHub: node H's grill/review gates are Plane-only — a project
// whose planeDoc doesn't resolve passes straight through (no agent run, no marker).
func TestGrillReviewNoOpForGitHub(t *testing.T) {
	rGH := &Runner{planeDoc: func(string) (*planedoc.Gateway, string, bool) { return nil, "", false }}
	in := stepInput{
		Project: storage.ProjectRecord{ID: "gh-proj"},
		Checkpoint: plannerCheckpoint{
			Issue:    &checkpointIssue{Repo: "o/r", IssueNumber: 9, URL: "https://github.com/o/r/issues/9"},
			Worktree: &checkpointWorktree{Path: "/tmp/x"},
		},
	}
	for _, step := range []func(context.Context, stepInput) (plannerCheckpoint, error){rGH.runGrillStep, rGH.runReviewStep} {
		cp, err := step(context.Background(), in)
		if err != nil {
			t.Fatalf("github no-op step error = %v", err)
		}
		if cp.Publish != nil && (cp.Publish.Grilled || cp.Publish.Reviewed) {
			t.Fatalf("github project must not grill/review: %+v", cp.Publish)
		}
	}
}

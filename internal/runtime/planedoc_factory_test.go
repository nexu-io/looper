package runtime

import (
	"github.com/nexu-io/looper/internal/workerhost"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func strPtr(s string) *string { return &s }

func TestPlaneDocForProject(t *testing.T) {
	t.Setenv("PLANE_TEST_KEY", "secret-123")
	cfg := &config.Config{
		Tools: config.ToolPathsConfig{PlanePath: strPtr("/usr/local/bin/plane")},
		Providers: []config.ProviderConfig{
			{ID: "plane-od", Kind: config.ProviderKindPlane, BaseURL: "https://plane.powerformer.net/api/v1", TokenEnv: strPtr("PLANE_TEST_KEY"), Workspace: strPtr("open-design"), ProjectID: strPtr("plane-uuid-1")},
			{ID: "gh", Kind: config.ProviderKindGitHub},
		},
		Projects: []config.ProjectRefConfig{
			{ID: "plane-proj", Provider: "plane-od", Repo: "owner/code", RepoPath: "/tmp/code"},
			{ID: "gh-proj", Provider: "gh", Repo: "owner/gh", RepoPath: "/tmp/gh"},
			{ID: "default-proj", Repo: "owner/def", RepoPath: "/tmp/def"}, // no provider → github default
		},
	}

	// A plane project → gateway + the Plane project UUID.
	gw, planeProjectID, ok := workerhost.PlaneDocForProject(cfg, "plane-proj")
	if !ok || gw == nil || planeProjectID != "plane-uuid-1" {
		t.Fatalf("workerhost.PlaneDocForProject(plane) = %v, %q, %v; want a gateway + plane-uuid-1", gw, planeProjectID, ok)
	}

	// A github project → no plane gateway (keeps the repo-file spec path).
	if _, _, ok := workerhost.PlaneDocForProject(cfg, "gh-proj"); ok {
		t.Fatal("workerhost.PlaneDocForProject(github) ok = true, want false")
	}
	// A project with no provider (github default) → no plane gateway.
	if _, _, ok := workerhost.PlaneDocForProject(cfg, "default-proj"); ok {
		t.Fatal("workerhost.PlaneDocForProject(default) ok = true, want false")
	}
	// Unknown project → false.
	if _, _, ok := workerhost.PlaneDocForProject(cfg, "nope"); ok {
		t.Fatal("workerhost.PlaneDocForProject(unknown) ok = true, want false")
	}
	// nil cfg → false.
	if _, _, ok := workerhost.PlaneDocForProject(nil, "plane-proj"); ok {
		t.Fatal("workerhost.PlaneDocForProject(nil cfg) ok = true, want false")
	}
}

package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIsReservedReviewerScratchBaseName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{".looper-review-1048.json", true},
		{".looper-review-abc_def-1.json", true},
		{".looper-review-.json", false},
		{".looper-review-é.json", false},
		{"looper-review-1.json", false},
		{".Looper-review-1.json", false},
		{".looper-review-1.JSON", false},
		{"subdir/.looper-review-1.json", false},
		{" .looper-review-1.json", false},
	}
	for _, tc := range cases {
		if got := IsReservedReviewerScratchBaseName(tc.name); got != tc.want {
			t.Errorf("IsReservedReviewerScratchBaseName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestScrubReservedReviewerScratchDeletesUntrackedOnly(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	mustGit(t, root, "init", repo)
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Tracked fixture that matches the reserved grammar must not be deleted.
	tracked := ".looper-review-fixture.json"
	if err := os.WriteFile(filepath.Join(repo, tracked), []byte(`{"tracked":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "README.md", tracked)
	mustGit(t, repo, "commit", "-m", "init")

	scratch := ".looper-review-3503.json"
	if err := os.WriteFile(filepath.Join(repo, scratch), []byte(`{"scratch":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ordinary := "notes.txt"
	if err := os.WriteFile(filepath.Join(repo, ordinary), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink with reserved name must not be followed or removed as a file scrub.
	linkName := ".looper-review-link.json"
	if err := os.Symlink("README.md", filepath.Join(repo, linkName)); err != nil {
		t.Fatal(err)
	}

	if err := ScrubReservedReviewerScratch(context.Background(), "git", repo); err != nil {
		t.Fatalf("ScrubReservedReviewerScratch() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(repo, scratch)); !os.IsNotExist(err) {
		t.Fatalf("untracked scratch still present: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, tracked)); err != nil {
		t.Fatalf("tracked reserved-name fixture missing: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, ordinary)); err != nil || string(got) != "keep\n" {
		t.Fatalf("ordinary untracked = %q err=%v, want preserved", got, err)
	}
	if _, err := os.Lstat(filepath.Join(repo, linkName)); err != nil {
		t.Fatalf("symlink reserved name missing: %v", err)
	}
}

func mustGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

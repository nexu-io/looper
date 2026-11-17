package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
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

// After `git rm --cached`, ls-files no longer lists the path but HEAD still owns
// it. Scrub must preserve the worktree file as ordinary dirt.
func TestScrubReservedReviewerScratchPreservesHeadTrackedAfterCachedRemoval(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	mustGit(t, root, "init", repo)
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cached := ".looper-review-cached.json"
	payload := []byte(`{"local":"content"}` + "\n")
	if err := os.WriteFile(filepath.Join(repo, cached), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "README.md", cached)
	mustGit(t, repo, "commit", "-m", "init with reserved name tracked")
	mustGit(t, repo, "rm", "--cached", cached)

	// True untracked scratch still scrubbed in the same pass.
	scratch := ".looper-review-untracked.json"
	if err := os.WriteFile(filepath.Join(repo, scratch), []byte(`{"scratch":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ScrubReservedReviewerScratch(context.Background(), "git", repo); err != nil {
		t.Fatalf("ScrubReservedReviewerScratch() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(repo, cached))
	if err != nil {
		t.Fatalf("HEAD-tracked file after rm --cached missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("HEAD-tracked content = %q, want %q", got, payload)
	}
	if _, err := os.Stat(filepath.Join(repo, scratch)); !os.IsNotExist(err) {
		t.Fatalf("untracked scratch still present: err=%v", err)
	}
}

func TestScrubReservedReviewerScratchUsesConfiguredGitExecutable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	mustGit(t, root, "init", repo)
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "README.md")
	mustGit(t, repo, "commit", "-m", "init")

	scratch := ".looper-review-custom-git.json"
	if err := os.WriteFile(filepath.Join(repo, scratch), []byte(`{"scratch":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wrapper is the only "git" the scrub may invoke; real git is not on PATH for it.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath git: %v", err)
	}
	wrapper := filepath.Join(root, "custom-git")
	script := "#!/bin/sh\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	gateway := New(Options{GitPath: wrapper})
	if err := gateway.ScrubReservedReviewerScratch(context.Background(), repo); err != nil {
		t.Fatalf("Gateway.ScrubReservedReviewerScratch() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, scratch)); !os.IsNotExist(err) {
		t.Fatalf("scratch still present after gateway scrub via custom git: err=%v", err)
	}

	// Re-create scratch so the missing binary is actually invoked for the probe.
	if err := os.WriteFile(filepath.Join(repo, scratch), []byte(`{"scratch":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Hard-fail when the configured path is unusable so recovery does not silently
	// fall back to PATH "git".
	if err := ScrubReservedReviewerScratch(context.Background(), filepath.Join(root, "missing-git"), repo); err == nil {
		t.Fatal("ScrubReservedReviewerScratch with missing gitPath error = nil, want failure")
	}
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'"'"'`) + "'"
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

// A missing promised HEAD tree must preserve scratch without consulting the
// daemon's personal SSH transport, including in a linked reviewer worktree.
func TestScrubReservedReviewerScratchBotPartialCloneWorktree(t *testing.T) {
	for _, missingTree := range []bool{false, true} {
		t.Run(map[bool]string{false: "available_tree", true: "missing_promised_tree"}[missingTree], func(t *testing.T) {
			root := t.TempDir()
			repo, worktree := filepath.Join(root, "repo"), filepath.Join(root, "review")
			mustGit(t, root, "init", repo)
			mustGit(t, repo, "config", "user.email", "test@example.com")
			mustGit(t, repo, "config", "user.name", "test")
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			mustGit(t, repo, "add", "README.md")
			mustGit(t, repo, "commit", "-m", "base")
			mustGit(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
			scratch := filepath.Join(worktree, ".looper-review-partial.json")
			payload := []byte("scratch\n")
			if err := os.WriteFile(scratch, payload, 0600); err != nil {
				t.Fatal(err)
			}
			mustGit(t, repo, "remote", "add", "origin", "git@example.invalid:org/repo.git")
			mustGit(t, repo, "config", "remote.origin.promisor", "true")
			mustGit(t, repo, "config", "remote.origin.partialclonefilter", "blob:none")
			if missingTree {
				cmd := exec.Command("git", "rev-parse", "HEAD^{tree}")
				cmd.Dir = repo
				output, err := cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
				tree := strings.TrimSpace(string(output))
				if err := os.Remove(filepath.Join(repo, ".git", "objects", tree[:2], tree[2:])); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(root, "personal-ssh-used")
			ssh := filepath.Join(root, "personal-ssh")
			if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf used > "+shellQuote(marker)+"\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_SSH_COMMAND", ssh)
			cfg := config.Config{
				Identities: map[string]config.HostingIdentityConfig{"reviewer": {Kind: config.HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: filepath.Join(root, "unavailable.pem")}},
				Projects:   []config.ProjectRefConfig{{ID: "project", Repo: "org/repo", Identity: "reviewer"}},
			}
			ctx, err := hostingidentity.Bind(context.Background(), cfg, "project", "reviewer")
			if err != nil {
				t.Fatal(err)
			}
			err = New(Options{GitPath: "git"}).ScrubReservedReviewerScratch(ctx, worktree)
			if missingTree {
				if err == nil {
					t.Fatal("missing HEAD tree must fail without deleting scratch")
				}
				if got, err := os.ReadFile(scratch); err != nil || string(got) != string(payload) {
					t.Fatalf("scratch changed: %q, %v", got, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(scratch); !os.IsNotExist(err) {
					t.Fatalf("untracked scratch not removed: %v", err)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("scratch probe invoked personal SSH: %v", err)
			}
		})
	}
}

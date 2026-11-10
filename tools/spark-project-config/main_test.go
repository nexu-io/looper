package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPrepareCandidatePreservesExistingConfigurationAndCanBeRepeated(t *testing.T) {
	configPath, repo := fixture(t)
	before := read(t, configPath)
	output := filepath.Join(t.TempDir(), "candidate.toml")
	if err := prepareCandidate(configPath, repo, output); err != nil {
		t.Fatal(err)
	}
	candidate := read(t, output)
	if !bytes.HasPrefix(candidate, before) || !bytes.Equal(read(t, configPath), before) {
		t.Fatal("the original configuration was not preserved")
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("candidate permissions = %o, want 600", info.Mode().Perm())
	}

	repeated := filepath.Join(t.TempDir(), "repeat.toml")
	if err := prepareCandidate(output, repo, repeated); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read(t, repeated), candidate) {
		t.Fatal("preparing a candidate again changed its content")
	}
	if err := prepareCandidate(configPath, repo, output); err == nil {
		t.Fatal("allowed overwriting an existing candidate")
	}
}

func TestPrepareCandidateRejectsAnExistingProjectOrUnrelatedCheckout(t *testing.T) {
	configPath, repo := fixture(t)
	output := filepath.Join(t.TempDir(), "candidate.toml")
	base := read(t, configPath)
	conflict := bytes.Replace(base, []byte(`id = "existing"`), []byte(`id = "openmotion"`), 1)
	if err := os.WriteFile(configPath, conflict, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareCandidate(configPath, repo, output); err == nil {
		t.Fatal("allowed another project with the OpenMotion ID")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("rejected candidate remains on disk: %v", err)
	}
	if err := os.WriteFile(configPath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "remote", "set-url", "origin", "https://github.com/nexu-io/looper.git")
	if err := prepareCandidate(configPath, repo, output); err == nil {
		t.Fatal("allowed an unrelated checkout")
	}
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "motion")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "Fixture", "--allow-empty")
	git(t, repo, "remote", "add", "origin", "https://github.com/nexu-io/html-anything.git")
	git(t, repo, "update-ref", "refs/remotes/origin/feature/motion", "HEAD")

	path := filepath.Join(root, "config.toml")
	base := fmt.Sprintf(`[storage]
dbPath = %q

[daemon]
logDir = %q

# Existing project must be kept for the next startup import.
[[projects]]
id = "existing"
name = "Existing project"
repoPath = %q
repo = "nexu-io/looper"
`, filepath.Join(root, "looper.sqlite"), root, root)
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, repo
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

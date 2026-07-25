package worktreesafety

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFixerOwnerTokenRoundTripOrdinaryGitDir(t *testing.T) {
	t.Parallel()

	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	const token = "fixer:loop_1:run_1:2026-07-26T00:00:00.000Z"
	if err := WriteFixerOwnerToken(wt, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken() error = %v", err)
	}
	if got := ReadFixerOwnerToken(wt); got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q", got, token)
	}
	ClearFixerOwnerToken(wt)
	if got := ReadFixerOwnerToken(wt); got != "" {
		t.Fatalf("ReadFixerOwnerToken() after clear = %q, want empty", got)
	}
}

func TestFixerOwnerTokenRoundTripLinkedGitdir(t *testing.T) {
	t.Parallel()

	wt := t.TempDir()
	gitdir := filepath.Join(t.TempDir(), "worktrees", "wt-1")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatalf("MkdirAll gitdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .git: %v", err)
	}
	const token = "fixer:loop_linked:run_2:t"
	if err := WriteFixerOwnerToken(wt, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(gitdir, FixerOwnerTokenFile)); err != nil {
		t.Fatalf("token file missing in gitdir: %v", err)
	}
	if got := ReadFixerOwnerToken(wt); got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q", got, token)
	}
	ClearFixerOwnerToken(wt)
	if got := ReadFixerOwnerToken(wt); got != "" {
		t.Fatalf("ReadFixerOwnerToken() after clear = %q, want empty", got)
	}
}

func TestWriteFixerOwnerTokenCreatesMissingGitDir(t *testing.T) {
	t.Parallel()

	wt := t.TempDir()
	const token = "fixer:create-missing"
	if err := WriteFixerOwnerToken(wt, token); err != nil {
		t.Fatalf("WriteFixerOwnerToken() error = %v", err)
	}
	if got := ReadFixerOwnerToken(wt); got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q", got, token)
	}
}

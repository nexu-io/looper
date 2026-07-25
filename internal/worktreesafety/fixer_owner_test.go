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
	if err := ClearFixerOwnerToken(wt); err != nil {
		t.Fatalf("ClearFixerOwnerToken() error = %v", err)
	}
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
	if err := ClearFixerOwnerToken(wt); err != nil {
		t.Fatalf("ClearFixerOwnerToken() error = %v", err)
	}
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

func TestClearFixerOwnerTokenMissingMarkerIsSuccess(t *testing.T) {
	t.Parallel()

	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	if err := ClearFixerOwnerToken(wt); err != nil {
		t.Fatalf("ClearFixerOwnerToken() missing marker error = %v", err)
	}
	if err := ClearFixerOwnerToken(""); err != nil {
		t.Fatalf("ClearFixerOwnerToken() empty path error = %v", err)
	}
	if err := ClearFixerOwnerToken(filepath.Join(t.TempDir(), "missing-wt")); err != nil {
		t.Fatalf("ClearFixerOwnerToken() missing worktree error = %v", err)
	}
}

func TestClearFixerOwnerTokenPropagatesRemoveFailure(t *testing.T) {
	t.Parallel()

	wt := t.TempDir()
	gitDir := filepath.Join(wt, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	marker := filepath.Join(gitDir, FixerOwnerTokenFile)
	if err := os.WriteFile(marker, []byte("token\n"), 0o644); err != nil {
		t.Fatalf("WriteFile marker: %v", err)
	}
	// Make the private git dir non-writable so remove fails on Unix.
	if err := os.Chmod(gitDir, 0o555); err != nil {
		t.Fatalf("Chmod gitDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	err := ClearFixerOwnerToken(wt)
	if err == nil {
		// Some platforms still allow remove despite dir mode; force-check by
		// making the marker a non-empty directory instead is not possible for a
		// file. Skip when the OS does not enforce this permission model.
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Skip("platform allowed remove on read-only directory")
		}
		t.Fatal("ClearFixerOwnerToken() error = nil, want remove failure")
	}
	if got := ReadFixerOwnerToken(wt); got == "" {
		t.Fatal("token was cleared despite clear error; authority must remain revoked only on success")
	}
}

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
	got, err := ReadFixerOwnerToken(wt)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q", got, token)
	}
	if err := ClearFixerOwnerToken(wt); err != nil {
		t.Fatalf("ClearFixerOwnerToken() error = %v", err)
	}
	got, err = ReadFixerOwnerToken(wt)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() after clear error = %v", err)
	}
	if got != "" {
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
	got, err := ReadFixerOwnerToken(wt)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q", got, token)
	}
	if err := ClearFixerOwnerToken(wt); err != nil {
		t.Fatalf("ClearFixerOwnerToken() error = %v", err)
	}
	got, err = ReadFixerOwnerToken(wt)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() after clear error = %v", err)
	}
	if got != "" {
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
	got, err := ReadFixerOwnerToken(wt)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("ReadFixerOwnerToken() = %q, want %q", got, token)
	}
}

func TestReadFixerOwnerTokenMissingIsEmpty(t *testing.T) {
	t.Parallel()

	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	got, err := ReadFixerOwnerToken(wt)
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() missing marker error = %v", err)
	}
	if got != "" {
		t.Fatalf("ReadFixerOwnerToken() missing marker = %q, want empty", got)
	}
	got, err = ReadFixerOwnerToken(filepath.Join(t.TempDir(), "missing-wt"))
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() missing worktree error = %v", err)
	}
	if got != "" {
		t.Fatalf("ReadFixerOwnerToken() missing worktree = %q, want empty", got)
	}
	got, err = ReadFixerOwnerToken("")
	if err != nil {
		t.Fatalf("ReadFixerOwnerToken() empty path error = %v", err)
	}
	if got != "" {
		t.Fatalf("ReadFixerOwnerToken() empty path = %q, want empty", got)
	}
}

func TestReadFixerOwnerTokenPropagatesReadFailure(t *testing.T) {
	t.Parallel()

	wt := t.TempDir()
	gitDir := filepath.Join(wt, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	// Directory-as-marker is a deterministic read failure (not IsNotExist).
	marker := filepath.Join(gitDir, FixerOwnerTokenFile)
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatalf("Mkdir marker: %v", err)
	}

	got, err := ReadFixerOwnerToken(wt)
	if err == nil {
		t.Fatal("ReadFixerOwnerToken() error = nil, want read failure")
	}
	if got != "" {
		t.Fatalf("ReadFixerOwnerToken() = %q on failure, want empty", got)
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
	// Induce a deterministic remove failure without relying on directory
	// permissions: os.Remove refuses a non-empty directory. chmod 0555 is not
	// reliable under root or CAP_DAC_OVERRIDE (common in CI containers).
	marker := filepath.Join(gitDir, FixerOwnerTokenFile)
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatalf("Mkdir marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(marker, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile marker child: %v", err)
	}

	err := ClearFixerOwnerToken(wt)
	if err == nil {
		t.Fatal("ClearFixerOwnerToken() error = nil, want remove failure")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("marker removed despite clear error: %v", statErr)
	}
}

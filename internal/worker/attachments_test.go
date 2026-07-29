package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testWorkItemUUID = "11111111-2222-3333-4444-555555555555"

func testIssueURL(uuid string) string {
	return "https://plane.powerformer.net/open-design/browse/issues/" + uuid
}

// TestAttachTaskScreenshots_CopiesImagesAndPrompts verifies the happy path: images
// dropped at <attachmentsRoot>/<uuid>/ are copied into ./.looper-attachments/ and the
// returned prompt block names each one and tells the agent to Read (not commit) them.
func TestAttachTaskScreenshots_CopiesImagesAndPrompts(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, testWorkItemUUID)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "bug.png"), []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "shot.JPG"), []byte("JPGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-image sibling must be ignored.
	if err := os.WriteFile(filepath.Join(srcDir, "notes.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	worktree := t.TempDir()
	r := &Runner{attachmentsRoot: root}

	block := r.attachTaskScreenshots(workerInput{IssueURL: testIssueURL(testWorkItemUUID)}, worktree)
	if block == "" {
		t.Fatal("expected a non-empty prompt block")
	}
	for _, want := range []string{"./.looper-attachments/bug.png", "./.looper-attachments/shot.JPG", "Read tool", "do NOT git add or commit"} {
		if !strings.Contains(block, want) {
			t.Errorf("prompt block missing %q\n---\n%s", want, block)
		}
	}
	if strings.Contains(block, "notes.txt") {
		t.Errorf("non-image file leaked into prompt block:\n%s", block)
	}

	// Images must actually be copied into the worktree with their bytes intact.
	got, err := os.ReadFile(filepath.Join(worktree, ".looper-attachments", "bug.png"))
	if err != nil {
		t.Fatalf("bug.png not copied into worktree: %v", err)
	}
	if string(got) != "PNGDATA" {
		t.Errorf("copied bytes = %q, want PNGDATA", got)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".looper-attachments", "notes.txt")); !os.IsNotExist(err) {
		t.Errorf("notes.txt should not have been copied")
	}
}

// TestAttachTaskScreenshots_NoopCases verifies every best-effort bail-out returns ""
// (and never errors) so a worker keeps running against text alone.
func TestAttachTaskScreenshots_NoopCases(t *testing.T) {
	worktree := t.TempDir()

	cases := []struct {
		name string
		r    *Runner
		work workerInput
	}{
		{"empty attachmentsRoot", &Runner{attachmentsRoot: ""}, workerInput{IssueURL: testIssueURL(testWorkItemUUID)}},
		{"non-plane issue url", &Runner{attachmentsRoot: t.TempDir()}, workerInput{IssueURL: "https://github.com/o/r/issues/7"}},
		{"empty issue url", &Runner{attachmentsRoot: t.TempDir()}, workerInput{IssueURL: ""}},
		{"missing dir", &Runner{attachmentsRoot: t.TempDir()}, workerInput{IssueURL: testIssueURL("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.attachTaskScreenshots(tc.work, worktree); got != "" {
				t.Errorf("expected empty block, got:\n%s", got)
			}
		})
	}
}

// TestAttachTaskScreenshots_EmptyDirReturnsEmpty verifies a dir with only non-image
// files yields no prompt block (nothing to show the agent).
func TestAttachTaskScreenshots_EmptyDirReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, testWorkItemUUID)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "readme.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{attachmentsRoot: root}
	if got := r.attachTaskScreenshots(workerInput{IssueURL: testIssueURL(testWorkItemUUID)}, t.TempDir()); got != "" {
		t.Errorf("expected empty block for image-less dir, got:\n%s", got)
	}
}

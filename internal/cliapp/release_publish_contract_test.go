package cliapp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Run the publisher against a local bucket and compare its pointer decision to
// the actual updater. This catches drift between the shell/Python and Go paths.
func TestReleasePublisherMatchesUpdaterVersionOrdering(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("release publisher requires python3")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "publish-release-manifest-r2.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ existing, incoming, channel string }{
		{"v1.2.3-beta.01", "v1.2.3-beta.1", "beta"},
		{"v1.2.3-beta.1", "v1.2.3-beta.01", "beta"},
		{"v1.2.3-beta.00", "v1.2.3-beta.0", "beta"},
		{"v1.2.3-beta.0", "v1.2.3-beta.00", "beta"},
		{"v1.2.3-beta.01", "v1.2.3-beta.001", "beta"},
		{"v1.2.3-beta.2", "v1.2.3-beta.10", "beta"},
		{"v1.2.3-beta.10", "v1.2.3-beta.2", "beta"},
		{"v1.2.3-beta.1", "v1.2.3-beta.rc", "beta"},
		{"v1.2.3-beta.rc", "v1.2.3-beta.1", "beta"},
		{"v1.2.3-beta", "v1.2.3-beta.1", "beta"},
		{"v1.2.3-beta.1", "v1.2.3-beta", "beta"},
		{"v1.2.3-beta.1", "v1.2.3-beta.1", "beta"},
		{"v1.2.3", "v1.2.4", "stable"},
		{"v1.2.4", "v1.2.3", "stable"},
	} {
		t.Run(tc.existing+"_to_"+tc.incoming, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			bucket := filepath.Join(root, "bucket")
			current := []byte(fmt.Sprintf(`{"tag":%q,"channel":%q,"source":"existing"}`, tc.existing, tc.channel))
			incoming := []byte(fmt.Sprintf(`{"tag":%q,"channel":%q,"source":"incoming"}`, tc.incoming, tc.channel))
			pointer := filepath.Join(bucket, "test-releases", "channels", tc.channel+".json")
			if err := os.MkdirAll(filepath.Dir(pointer), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pointer, current, 0644); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(bucket, "test-releases", "manifest.json")
			if tc.channel == "stable" {
				if err := os.WriteFile(alias, current, 0644); err != nil {
					t.Fatal(err)
				}
			}
			manifest := filepath.Join(root, "incoming.json")
			if err := os.WriteFile(manifest, incoming, 0644); err != nil {
				t.Fatal(err)
			}
			stub := `#!/bin/sh
set -eu
[ "$1" = r2 ] && [ "$2" = object ]
operation="$3"
key="$4"
shift 4
file=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --file) file="$2"; shift 2 ;;
    *) shift ;;
  esac
done
object="$TEST_R2_ROOT/$key"
case "$operation" in
  get) cp "$object" "$file" ;;
  put) mkdir -p "$(dirname "$object")"; cp "$file" "$object" ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(root, "wrangler"), []byte(stub), 0755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", script)
			// Explicit overrides keep this test entirely inside the temporary bucket.
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_R2_ROOT="+bucket, "R2_RELEASES_BUCKET=test-releases", "RELEASE_MANIFEST="+manifest, "RELEASE_TAG="+tc.incoming, "RELEASE_CHANNEL="+tc.channel)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("publisher failed: %v\n%s", err, output)
			}
			comparison, err := compareSemver(tc.existing, tc.incoming)
			if err != nil {
				t.Fatal(err)
			}
			want := current
			if comparison <= 0 {
				want = incoming
			}
			for path, expected := range map[string][]byte{
				pointer: want,
				filepath.Join(bucket, "test-releases", tc.incoming, "manifest.json"): incoming,
			} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, expected) {
					t.Fatalf("%s = %s, error=%v; want %s (Go comparison=%d)\npublisher: %s", path, got, err, expected, comparison, output)
				}
			}
			if tc.channel == "stable" {
				got, err := os.ReadFile(alias)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("stable alias=%s error=%v want=%s", got, err, want)
				}
			}
		})
	}
}

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
			current := []byte(fmt.Sprintf(`{"tag":%q,"channel":%q,"source":"existing"}`, tc.existing, tc.channel))
			bucket, incoming, output, err := runTestReleasePublisher(t, current, tc.incoming, tc.channel, "")
			if err != nil {
				t.Fatalf("publisher failed: %v\n%s", err, output)
			}
			pointer := filepath.Join(bucket, "test-releases", "channels", tc.channel+".json")
			alias := filepath.Join(bucket, "test-releases", "manifest.json")

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

func runTestReleasePublisher(t *testing.T, current []byte, tag, channel, getError string) (string, []byte, string, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("release publisher requires python3")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "publish-release-manifest-r2.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bucket := filepath.Join(root, "bucket")
	incoming := []byte(fmt.Sprintf(`{"tag":%q,"channel":%q,"source":"incoming"}`, tag, channel))
	pointer := filepath.Join(bucket, "test-releases", "channels", channel+".json")
	if err := os.MkdirAll(filepath.Dir(pointer), 0755); err != nil {
		t.Fatal(err)
	}
	if current != nil {
		for _, path := range []string{pointer, filepath.Join(bucket, "test-releases", "manifest.json"), filepath.Join(bucket, "test-releases", tag, "manifest.json")} {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, current, 0644); err != nil {
				t.Fatal(err)
			}
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
cache=""
while [ "$#" -gt 0 ]; do
 case "$1" in
  --file) file="$2"; shift 2 ;;
  --cache-control) cache="$2"; shift 2 ;;
  *) shift ;;
 esac
done
object="$TEST_R2_ROOT/$key"
case "$operation" in
 get)
  if [ -n "$TEST_GET_ERROR" ]; then echo "$TEST_GET_ERROR" >&2; exit 1; fi
  if [ ! -f "$object" ]; then echo "404 Not Found" >&2; exit 1; fi
  cp "$object" "$file" ;;
 put)
  mkdir -p "$(dirname "$object")"
  cp "$file" "$object"
  printf '%s' "$cache" > "$object.cache-control" ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "wrangler"), []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_R2_ROOT="+bucket, "TEST_GET_ERROR="+getError, "R2_RELEASES_BUCKET=test-releases", "RELEASE_MANIFEST="+manifest, "RELEASE_TAG="+tag, "RELEASE_CHANNEL="+channel)
	output, err := cmd.CombinedOutput()
	return bucket, incoming, string(output), err
}

func TestReleasePublisherRepairsMalformedPointers(t *testing.T) {
	t.Parallel()
	for name, current := range map[string][]byte{
		"missing": nil, "truncated": []byte(`{"tag":`), "null": []byte(`null`), "array": []byte(`[]`), "unversioned": []byte(`{}`), "invalid tag": []byte(`{"tag":"bad"}`), "invalid encoding": {0xff},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bucket, incoming, output, err := runTestReleasePublisher(t, current, "v1.2.4", "stable", "")
			if err != nil {
				t.Fatalf("publisher failed: %v\n%s", err, output)
			}
			for _, key := range []string{"channels/stable.json", "manifest.json", "v1.2.4/manifest.json"} {
				got, err := os.ReadFile(filepath.Join(bucket, "test-releases", key))
				if err != nil || !bytes.Equal(got, incoming) {
					t.Fatalf("%s=%s err=%v want=%s", key, got, err, incoming)
				}
			}
		})
	}
}

func TestReleasePublisherDoesNotRepairTransportFailures(t *testing.T) {
	t.Parallel()
	current := []byte(`{"tag":"v1.2.3","channel":"stable"}`)
	bucket, _, output, err := runTestReleasePublisher(t, current, "v1.2.4", "stable", "403 Forbidden")
	if err == nil {
		t.Fatalf("read failure ignored: %s", output)
	}
	got, err := os.ReadFile(filepath.Join(bucket, "test-releases", "channels", "stable.json"))
	if err != nil || !bytes.Equal(got, current) {
		t.Fatalf("pointer=%s err=%v; read failure must preserve old pointer", got, err)
	}
}

func TestReleasePublisherRerunsUseRefreshableManifests(t *testing.T) {
	t.Parallel()
	current := []byte(`{"tag":"v1.2.4","channel":"stable","source":"old build"}`)
	bucket, incoming, output, err := runTestReleasePublisher(t, current, "v1.2.4", "stable", "")
	if err != nil {
		t.Fatalf("publisher failed: %v\n%s", err, output)
	}
	for _, key := range []string{"channels/stable.json", "manifest.json", "v1.2.4/manifest.json"} {
		object := filepath.Join(bucket, "test-releases", key)
		got, err := os.ReadFile(object)
		if err != nil || !bytes.Equal(got, incoming) {
			t.Fatalf("%s=%s err=%v want rebuilt manifest", key, got, err)
		}
		cache, err := os.ReadFile(object + ".cache-control")
		if err != nil || string(cache) != "public, max-age=60" {
			t.Fatalf("%s cache=%s err=%v; want short refreshable cache", key, cache, err)
		}
	}
}

func TestReleasePublisherRejectsInvalidIncomingVersionDuringRepair(t *testing.T) {
	t.Parallel()
	current := []byte(`{}`)
	bucket, _, output, err := runTestReleasePublisher(t, current, "bad-version", "stable", "")
	if err == nil {
		t.Fatalf("invalid incoming version was accepted: %s", output)
	}
	got, err := os.ReadFile(filepath.Join(bucket, "test-releases", "channels", "stable.json"))
	if err != nil || !bytes.Equal(got, current) {
		t.Fatalf("pointer=%s error=%v; invalid incoming version must not replace it", got, err)
	}
}

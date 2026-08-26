package runtime

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func writeHumanAttentionOsascript(t *testing.T, path, capturePath string, fail bool) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + capturePath + "\"\n"
	if fail {
		script += "exit 1\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func assertHumanAttentionInAppCount(t *testing.T, repos *storage.Repositories, loopID string, want int) {
	t.Helper()
	notifications, err := repos.Notifications.List(context.Background(), 100)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	got := 0
	for _, n := range notifications {
		if n.LoopID == nil || *n.LoopID != loopID {
			continue
		}
		if n.Channel != "in_app" || n.Level != "action_required" {
			continue
		}
		if n.DedupeKey == nil || !strings.HasPrefix(*n.DedupeKey, "human_attention:") {
			continue
		}
		got++
	}
	if got != want {
		t.Fatalf("human_attention in_app count for %s = %d, want %d", loopID, got, want)
	}
}

func assertHumanAttentionInAppBodyContains(t *testing.T, repos *storage.Repositories, loopID, want string) {
	t.Helper()
	notifications, err := repos.Notifications.List(context.Background(), 100)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	for _, n := range notifications {
		if n.LoopID == nil || *n.LoopID != loopID {
			continue
		}
		if n.Channel != "in_app" || n.Level != "action_required" {
			continue
		}
		if n.DedupeKey == nil || !strings.HasPrefix(*n.DedupeKey, "human_attention:") {
			continue
		}
		if strings.Contains(n.Body, want) {
			return
		}
		t.Fatalf("human_attention in_app body for %s = %q, want substring %q", loopID, n.Body, want)
	}
	t.Fatalf("human_attention in_app row for %s not found", loopID)
}

func assertOsascriptContains(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if !strings.Contains(string(body), want) {
		t.Fatalf("osascript log %q does not contain %q\nlog:\n%s", path, want, body)
	}
}

func assertOsascriptNotContains(t *testing.T, path, unwanted string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if strings.Contains(string(body), unwanted) {
		t.Fatalf("osascript log %q unexpectedly contains %q\nlog:\n%s", path, unwanted, body)
	}
}

func assertOsascriptLacksSensitive(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	lower := strings.ToLower(string(body))
	for _, banned := range []string{"token=", "authorization", "answer=", "password", "secret"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("osascript log contains sensitive fragment %q:\n%s", banned, body)
		}
	}
}

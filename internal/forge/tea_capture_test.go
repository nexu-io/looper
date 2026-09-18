package forge

import (
	"context"
	"encoding/json"
	"github.com/nexu-io/looper/internal/config"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func teaOutputScript(t *testing.T, stdout, stderr string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"stdout": stdout, "stderr": stderr,
		"tea": "#!/bin/sh\ncd -- \"$(dirname -- \"$0\")\"\ncat stdout\ncat stderr >&2\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "tea")
}

func TestDefaultTeaRunnerBoundsBothStreams(t *testing.T) {
	path := teaOutputScript(t, strings.Repeat("b", 2<<20), strings.Repeat("h", 2<<20))
	runner := defaultTeaRunner{maxCapturedBytes: 1 << 20}
	result, err := runner.Run(context.Background(), path, nil, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stdout) != 1<<20 || len(result.Stderr) != 1<<20 || !result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("capture lengths = %d/%d, truncated = %v/%v", len(result.Stdout), len(result.Stderr), result.StdoutTruncated, result.StderrTruncated)
	}
}

func TestTeaTransportCaptureContract(t *testing.T) {
	for _, tc := range []struct {
		name            string
		limit, bodySize int
		headers         string
		wantError       bool
	}{
		{"unlimited large diff", 0, 2 << 20, "", false},
		{"exact limit", 16, 16, "", false},
		{"one byte over", 16, 17, "", true},
		{"health probe limit", 1 << 20, 2 << 20, "", true},
		{"bounded headers", 16, 16, strings.Repeat("X-Padding: value\n", 8192), true},
		{"maximum int does not overflow", math.MaxInt, 16, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat("d", tc.bodySize)
			path := teaOutputScript(t, body, "HTTP/1.1 200 OK\n"+tc.headers+"\n")
			transport := newTeaTransport(path, "test", nil, 10*time.Second, nil, tc.limit)
			response, err := transport.doRaw(context.Background(), http.MethodGet, "repos/acme/repo/pulls/1.diff", nil, nil)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "response exceeds") {
					t.Fatalf("error = %v, want response exceeds", err)
				}
			} else if err != nil || string(response.body) != body {
				t.Fatalf("body length = %d, error = %v", len(response.body), err)
			}
		})
	}
}

func TestReadBoundedResponseMaximumLimit(t *testing.T) {
	body, err := readBoundedResponse(strings.NewReader("diff"), math.MaxInt)
	if err != nil || string(body) != "diff" {
		t.Fatalf("body = %q, error = %v", body, err)
	}
}

// Exercise the production constructors and real shell capture, not an injected
// TeaCommandRunner: preselecting an unbounded runner bypasses transport limits.
func TestTeaProviderAndHealthCaptureContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"test"}`))
	}))
	defer server.Close()
	path := teaOutputScript(t, `{"id":7,"login":"reviewer"}`, "HTTP/1.1 200 OK\n"+strings.Repeat("X-Padding: value\n", 131072)+"\n")
	logins, err := json.Marshal([]TeaLogin{{Name: "test", URL: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "logins"), logins, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncd -- \"$(dirname -- \"$0\")\"\nif [ \"$1\" = logins ]; then cat logins; exit 0; fi\ncat stdout\ncat stderr >&2\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	provider := config.ProviderConfig{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, Auth: config.ProviderAuthTea, TeaPath: &path, TeaLogin: stringPtr("test"), MaxResponseBytes: 64}
	client, err := NewForgejoClientFromConfig(provider, "acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CurrentUser(context.Background()); err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("configured provider must reject truncated headers: %v", err)
	}
	// A provider's unlimited default must not override the health probe's 1 MiB cap.
	provider.MaxResponseBytes = 0
	health := ProbeForgejoProvider(context.Background(), provider, nil)
	if health.Authentication != AuthenticationUnknown || health.Identity != nil {
		t.Fatalf("health probe accepted oversized capture: %+v", health)
	}
}

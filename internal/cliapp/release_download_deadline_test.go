package cliapp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The transport models a header/body stall by returning a deadline error, but
// only after asserting that the production caller supplied a bounded context.
// No wall-clock wait for the full production download timeout is necessary.
func TestCandidateDownloadDeadlineAllowsReleaseFallback(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"binary headers", "binary body", "checksum headers", "checksum body"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			binary := []byte("verified release")
			checksum := fmt.Sprintf("%x", sha256.Sum256(binary))
			var binaryDeadline time.Time
			var fallbackDeadline time.Time
			stalled := false
			app := New(Deps{HomeDir: t.TempDir(), Platform: "darwin", Arch: "arm64", HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				u := req.URL.String()
				if isCDNReleaseMetadataURL(u) {
					return jsonResponse(t, 200, withPublishedArtifacts(t, `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3"}`)), nil
				}
				if u == buildGitHubReleaseAPIURL(defaultReleaseOwner, defaultReleaseRepo, "") {
					if !stalled {
						t.Error("fallback happened before exercising a candidate download")
					}
					if err := req.Context().Err(); err != nil {
						t.Fatalf("fallback inherited failed candidate context: %v", err)
					}
					return jsonResponse(t, 200, `{"tag_name":"v1.2.4","assets":[{"name":"looperd-darwin-arm64","browser_download_url":"https://example.invalid/binary"},{"name":"looperd-darwin-arm64.sha256","browser_download_url":"https://example.invalid/checksum"}]}`), nil
				}
				deadline, ok := req.Context().Deadline()
				if !ok || time.Until(deadline) > 2*time.Minute {
					t.Errorf("artifact request has no deadline within 2 minutes: %s deadline=%v", u, deadline)
				}
				if strings.HasPrefix(u, "https://github.com/") {
					isChecksum := strings.HasSuffix(u, ".sha256")
					if !isChecksum {
						binaryDeadline = deadline
					} else if !deadline.Equal(binaryDeadline) {
						t.Errorf("binary and checksum did not share one candidate deadline")
					}
					if isChecksum == strings.HasPrefix(stage, "checksum") {
						stalled = true
						if strings.HasSuffix(stage, "headers") {
							return nil, context.DeadlineExceeded
						}
						return &http.Response{StatusCode: http.StatusOK, Body: deadlineErrorBody{}}, nil
					}
					return binaryResponse(t, 200, binary), nil
				}
				if u == "https://example.invalid/binary" {
					fallbackDeadline = deadline
					if !deadline.After(binaryDeadline) {
						t.Errorf("fallback did not receive a fresh deadline")
					}
					return binaryResponse(t, 200, binary), nil
				}
				if u == "https://example.invalid/checksum" {
					if !deadline.Equal(fallbackDeadline) {
						t.Error("fallback checksum received a separate deadline")
					}
					return textResponse(t, 200, checksum), nil
				}
				t.Fatalf("unexpected request %s", u)
				return nil, nil
			})}})
			parent := context.Background()
			prepared, err := newCommandRuntime(app, nil).prepareManagedDaemonInstall(parent, true, "", nil)
			if err != nil || string(prepared.binaryBytes) != string(binary) || prepared.result.DownloadedFrom == nil || *prepared.result.DownloadedFrom != "https://example.invalid/binary" {
				t.Fatalf("prepared=%+v error=%v", prepared, err)
			}
			if parent.Err() != nil || !stalled {
				t.Fatalf("parent error=%v stalled=%v", parent.Err(), stalled)
			}
		})
	}
}

type deadlineErrorBody struct{}

func (deadlineErrorBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
func (deadlineErrorBody) Close() error             { return nil }

// Exercise actual HTTP body cancellation with a shorter caller budget, rather
// than waiting two minutes for the production candidate deadline.
func TestCandidateDownloadPreservesEarlierCallerDeadline(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
	defer server.Close()
	app := New(Deps{HomeDir: t.TempDir(), Platform: "darwin", Arch: "arm64", HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if isCDNReleaseMetadataURL(req.URL.String()) {
			return jsonResponse(t, 200, withPublishedArtifacts(t, `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3"}`)), nil
		}
		if req.URL.Host != "github.com" {
			t.Fatalf("caller timeout must stop fallback: %s", req.URL)
		}
		local := req.Clone(req.Context())
		local.URL.Scheme = "http"
		local.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return server.Client().Transport.RoundTrip(local)
	})}})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := newCommandRuntime(app, nil).prepareManagedDaemonInstall(ctx, true, "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v; want caller deadline", err)
	}
}

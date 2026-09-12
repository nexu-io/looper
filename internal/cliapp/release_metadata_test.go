package cliapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLatestVersionsFallBackFromMalformedCDNManifest(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"schema only":       `{"manifestVersion":1}`,
		"empty version":     `{"manifestVersion":1,"channel":"stable","tag":" ","version":" "}`,
		"invalid tag":       `{"manifestVersion":1,"channel":"stable","tag":"not-a-version","artifacts":{"looperd-darwin-arm64":{}}}`,
		"unsafe tag":        `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3-beta/../../other","artifacts":{"looperd-darwin-arm64":{}}}`,
		"missing artifacts": `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3"}`,
		"empty artifacts":   `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3","artifacts":{}}`,
		"invalid artifacts": `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3","artifacts":{"../escape":{}}}`,
		"future schema":     `{"manifestVersion":2,"tag":"v1.2.3","artifacts":{"looperd-darwin-arm64":{}}}`,
		"GitHub shape":      `{"tag_name":"v1.2.3","assets":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var seen []string
			app := New(Deps{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				seen = append(seen, req.URL.Host)
				if isCDNReleaseMetadataURL(req.URL.String()) {
					return jsonResponse(t, http.StatusOK, body), nil
				}
				return jsonResponse(t, http.StatusOK, `{"tag_name":"v1.2.4","assets":[]}`), nil
			})}})
			runtime := newCommandRuntime(app, nil)
			cli, err := runtime.fetchLatestCLIVersion(context.Background())
			if err != nil || cli != "1.2.4" {
				t.Fatalf("latest CLI = %q, %v; want GitHub version 1.2.4", cli, err)
			}
			daemon, err := runtime.fetchLatestDaemonRelease(context.Background())
			if err != nil || daemon.Version != "1.2.4" || daemon.Tag != "v1.2.4" {
				t.Fatalf("latest daemon = %#v, %v; want GitHub version 1.2.4", daemon, err)
			}
			if strings.Join(seen, ",") != "releases.looper.powerformer.com,api.github.com,releases.looper.powerformer.com,api.github.com" {
				t.Fatalf("requests = %v", seen)
			}
		})
	}
}

func TestReleaseMetadataFallsBackFromStalledCDN(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"headers", "body"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if stage == "body" {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"manifestVersion":1,"channel":"stable",`)
					w.(http.Flusher).Flush()
				}
				<-req.Context().Done()
			}))
			defer server.Close()
			var seen []string
			app := New(Deps{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				seen = append(seen, req.URL.Host)
				if !isCDNReleaseMetadataURL(req.URL.String()) {
					if err := req.Context().Err(); err != nil {
						return nil, err
					}
					return jsonResponse(t, http.StatusOK, `{"tag_name":"v1.2.4","assets":[]}`), nil
				}
				local := req.Clone(req.Context())
				local.URL.Scheme = "http"
				local.URL.Host = strings.TrimPrefix(server.URL, "http://")
				return server.Client().Transport.RoundTrip(local)
			})}})
			// The caller deadline is only a test safety net. Each source needs its
			// own earlier deadline so GitHub still has a usable context.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			payload, err := newCommandRuntime(app, nil).fetchReleaseMetadata(ctx, "")
			if err != nil || payload.TagName != "v1.2.4" {
				t.Fatalf("metadata = %#v, %v; want GitHub fallback", payload, err)
			}
			if ctx.Err() != nil || len(seen) != 2 {
				t.Fatalf("caller context = %v, requests = %v", ctx.Err(), seen)
			}
		})
	}
}

func TestReleaseMetadataHonorsCallerCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app := New(Deps{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("canceled caller must not start a metadata request")
		return nil, nil
	})}})
	_, err := newCommandRuntime(app, nil).fetchReleaseMetadata(ctx, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestReleaseMetadataResponseSizeLimit(t *testing.T) {
	t.Parallel()
	const limit = 1 << 20
	for _, source := range releaseMetadataURLs("") {
		for _, size := range []int{limit, limit + 1, limit * 2} {
			t.Run(source+"/"+strconv.Itoa(size), func(t *testing.T) {
				t.Parallel()
				body := `{"tag_name":"v1.2.3","assets":[]}`
				if isCDNReleaseMetadataURL(source) {
					body = `{"manifestVersion":1,"channel":"stable","tag":"v1.2.3","artifacts":{"looperd-darwin-arm64":{}}}`
				}
				reader := &metadataCountingBody{Reader: strings.NewReader(body + strings.Repeat(" ", size-len(body)))}
				app := New(Deps{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: reader, ContentLength: -1}, nil
				})}})
				_, err := newCommandRuntime(app, nil).fetchReleaseMetadataFromURL(context.Background(), source)
				if size == limit && err != nil {
					t.Fatalf("exact-limit metadata rejected: %v", err)
				}
				if size > limit && (err == nil || !strings.Contains(err.Error(), "exceeds")) {
					t.Fatalf("oversized metadata error = %v", err)
				}
				if reader.read > limit+1 || !reader.closed {
					t.Fatalf("read = %d, closed = %v", reader.read, reader.closed)
				}
			})
		}
	}
}

func TestReleaseMetadataFallsBackFromOversizedCDN(t *testing.T) {
	t.Parallel()
	var seen []string
	app := New(Deps{HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req.URL.Host)
		if isCDNReleaseMetadataURL(req.URL.String()) {
			return jsonResponse(t, http.StatusOK, strings.Repeat(" ", 1<<20)+`{"manifestVersion":1,"channel":"stable","tag":"v1.2.3","artifacts":{"looperd-darwin-arm64":{}}}`), nil
		}
		return jsonResponse(t, http.StatusOK, `{"tag_name":"v1.2.4","assets":[]}`), nil
	})}})
	payload, err := newCommandRuntime(app, nil).fetchReleaseMetadata(context.Background(), "")
	if err != nil || payload.TagName != "v1.2.4" || len(seen) != 2 {
		t.Fatalf("metadata = %#v, %v; requests = %v; want GitHub fallback", payload, err, seen)
	}
}

type metadataCountingBody struct {
	*strings.Reader
	read   int
	closed bool
}

func (b *metadataCountingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *metadataCountingBody) Close() error {
	b.closed = true
	return nil
}

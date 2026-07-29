package notify

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestDashboardDeepLinkUsable_OriginAndAuthPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		baseURL  string
		authMode config.AuthMode
		want     bool
	}{
		{name: "loopback none", baseURL: "http://127.0.0.1:17310", authMode: config.AuthModeNone, want: true},
		{name: "localhost none", baseURL: "http://localhost:17310", authMode: config.AuthModeNone, want: true},
		{name: "loopback local-token", baseURL: "http://127.0.0.1:17310", authMode: config.AuthModeLocalToken, want: false},
		{name: "non-loopback http none", baseURL: "http://dash.example:8080", authMode: config.AuthModeNone, want: false},
		{name: "non-loopback https none", baseURL: "https://dash.example", authMode: config.AuthModeNone, want: false},
		{name: "non-loopback https local-token", baseURL: "https://dash.example", authMode: config.AuthModeLocalToken, want: false},
		{name: "empty base", baseURL: "", authMode: config.AuthModeNone, want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewGateway(Options{
				DashboardBaseURL:  tc.baseURL,
				DashboardAuthMode: tc.authMode,
			})
			if got := g.dashboardDeepLinkUsable(); got != tc.want {
				t.Fatalf("dashboardDeepLinkUsable() = %v, want %v (base=%q auth=%q)", got, tc.want, tc.baseURL, tc.authMode)
			}
		})
	}
}

func TestResolveDashboardBaseURL_RejectsNonOriginBaseURL(t *testing.T) {
	t.Parallel()

	fallback := "http://127.0.0.1:17310"
	cases := []struct {
		name    string
		baseURL string
		host    string
		port    int
		want    string
	}{
		{name: "clean http origin", baseURL: "http://dash.example:8080", want: "http://dash.example:8080"},
		{name: "clean https origin trailing slash", baseURL: "https://dash.example/", want: "https://dash.example"},
		{name: "userinfo rejected", baseURL: "https://user:token@dash.example", host: "127.0.0.1", port: 17310, want: fallback},
		{name: "query rejected", baseURL: "https://dash.example/?x=y", host: "127.0.0.1", port: 17310, want: fallback},
		{name: "fragment rejected", baseURL: "https://dash.example/#frag", host: "127.0.0.1", port: 17310, want: fallback},
		{name: "path rejected", baseURL: "https://dash.example/prefix", host: "127.0.0.1", port: 17310, want: fallback},
		{name: "non-http scheme rejected", baseURL: "ftp://dash.example", host: "127.0.0.1", port: 17310, want: fallback},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := tc.baseURL
			cfg := config.ServerConfig{Host: tc.host, Port: tc.port, BaseURL: &base}
			if got := ResolveDashboardBaseURL(cfg); got != tc.want {
				t.Fatalf("ResolveDashboardBaseURL(%q) = %q, want %q", tc.baseURL, got, tc.want)
			}
		})
	}
}

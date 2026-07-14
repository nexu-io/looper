package forge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestTeaTransportAuthFailedAndRedaction(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://code.example.com"}}),
		apiHandlers: map[string]teaAPIResponse{
			"GET /user": {
				Stdout:   "",
				Stderr:   "Error: unauthorized token abcdefghijklmnopqrstuvwxyz0123456789secret rejected",
				ExitCode: 1,
			},
		},
	}
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	client, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner), WithLookPath(fakeTeaLookPath))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = client.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwxyz0123456789secret") {
		t.Fatalf("error leaked token-like material: %v", err)
	}
	var teaErr *TeaAuthError
	if !errors.As(err, &teaErr) || teaErr.Code != TeaErrorAuthFailed {
		t.Fatalf("error = %v, want tea_auth_failed", err)
	}
}

func TestTeaTransportCancellation(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://code.example.com"}}),
		delay:      200 * time.Millisecond,
		defaultAPI: &teaAPIResponse{Stdout: `{}`, Stderr: "HTTP/1.1 200 OK\n\n", ExitCode: 0},
	}
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	client, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner), WithLookPath(fakeTeaLookPath))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.CurrentUser(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		// tea runner returns ctx.Err on cancel during delay; if cancel already done before call, may still race.
		if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "cancel")) {
			t.Fatalf("CurrentUser() error = %v, want canceled", err)
		}
	}
}

func TestTeaTransportHTTPStatusError(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://code.example.com"}}),
		apiHandlers: map[string]teaAPIResponse{
			"GET /repos/acme/looper/issues/1": {
				Stdout:   `{"message":"not found"}`,
				Stderr:   "HTTP/1.1 404 Not Found\nContent-Type: application/json\n\n",
				ExitCode: 0,
			},
		},
	}
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	client, err := NewForgejoClientFromConfig(provider, "acme/looper", WithTeaRunner(runner), WithLookPath(fakeTeaLookPath))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = client.ViewIssue(context.Background(), 1)
	var httpErr *ForgejoHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusNotFound {
		t.Fatalf("error = %v, want HTTP 404", err)
	}
}

func TestProbeForgejoProviderTeaStates(t *testing.T) {
	runner := &recordingTeaRunner{
		loginsJSON: mustJSON(t, []TeaLogin{{Name: "selected-login", URL: "https://code.example.com"}}),
		apiHandlers: map[string]teaAPIResponse{
			"GET /user": {
				Stdout:   `{"id":1,"login":"alice"}`,
				Stderr:   "HTTP/1.1 200 OK\n\n",
				ExitCode: 0,
			},
			"GET /repos/acme/looper": {
				Stdout:   `{"permissions":{"admin":true,"push":true,"pull":true}}`,
				Stderr:   "HTTP/1.1 200 OK\n\n",
				ExitCode: 0,
			},
		},
	}
	// Use a non-routable base so unauthenticated HTTP version probe fails closed without hanging long.
	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	// Force tea path via options; version probe may be unreachable but auth should still validate via tea.
	health := ProbeForgejoProvider(context.Background(), provider, []ForgejoProbeProject{{ID: "p", Repo: "acme/looper"}}, WithTeaRunner(runner), WithLookPath(fakeTeaLookPath), WithTimeout(50*time.Millisecond))
	if health.Authentication != AuthenticationValid {
		t.Fatalf("authentication = %s, want valid (identity=%v)", health.Authentication, health.Identity)
	}
	if health.Identity == nil || health.Identity.Login != "alice" {
		t.Fatalf("identity = %#v", health.Identity)
	}
	if len(health.Projects) != 1 || health.Projects[0].Access != AccessWritable {
		t.Fatalf("projects = %#v", health.Projects)
	}
}

func TestListTeaLoginsAcceptsBooleanDefault(t *testing.T) {
	// Real tea CLI emits default as a JSON boolean; decoding into string used to
	// fail the whole list before login selection could run.
	runner := &recordingTeaRunner{
		loginsJSON: `[
			{"name":"default-login","url":"https://code.example.com","user":"alice","default":true},
			{"name":"selected-login","url":"https://code.example.com","user":"alice","default":false}
		]`,
	}
	logins, err := ListTeaLogins(context.Background(), "/usr/bin/fake-tea", runner)
	if err != nil {
		t.Fatalf("ListTeaLogins() error = %v", err)
	}
	if len(logins) != 2 {
		t.Fatalf("ListTeaLogins() len = %d, want 2", len(logins))
	}
	if !logins[0].Default || logins[1].Default {
		t.Fatalf("Default flags = [%v, %v], want [true, false]", logins[0].Default, logins[1].Default)
	}

	provider := config.ProviderConfig{
		ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.com",
		Auth: config.ProviderAuthTea, TeaLogin: stringPtr("selected-login"),
	}
	_, login, err := ValidateTeaLoginForProvider(context.Background(), provider, runner, fakeTeaLookPath)
	if err != nil {
		t.Fatalf("ValidateTeaLoginForProvider() error = %v", err)
	}
	if login.Name != "selected-login" {
		t.Fatalf("selected login = %q, want selected-login", login.Name)
	}
}

func TestMatchTeaLoginHostNormalizes(t *testing.T) {
	if !MatchTeaLoginHost("https://Code.Example.com/", "https://code.example.com") {
		t.Fatal("expected host match with case/path normalization")
	}
	if MatchTeaLoginHost("https://code.example.com", "https://other.example.com") {
		t.Fatal("expected host mismatch")
	}
}

func TestBuildTeaAPIEndpoint(t *testing.T) {
	got := buildTeaAPIEndpoint("repos/acme/looper/issues", map[string][]string{"state": {"open"}, "page": {"1"}})
	if !strings.HasPrefix(got, "/repos/acme/looper/issues?") || !strings.Contains(got, "state=open") || !strings.Contains(got, "page=1") {
		t.Fatalf("endpoint = %q", got)
	}
	abs := buildTeaAPIEndpoint("https://code.example.com/swagger.v1.json", nil)
	if abs != "https://code.example.com/swagger.v1.json" {
		t.Fatalf("absolute endpoint = %q", abs)
	}
}

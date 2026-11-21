package cliapp

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestBuildFeishuAuthorizeURL(t *testing.T) {
	redirectURI := "http://127.0.0.1:54321/callback"
	got := buildFeishuAuthorizeURL("cli_app_123", redirectURI, "statexyz")

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", got, err)
	}
	if want := "/open-apis/authen/v1/authorize"; parsed.Path != want {
		t.Fatalf("path = %q, want %q", parsed.Path, want)
	}
	query := parsed.Query()
	checks := map[string]string{
		"app_id":        "cli_app_123",
		"redirect_uri":  redirectURI,
		"response_type": "code",
		"state":         "statexyz",
	}
	for key, want := range checks {
		if got := query.Get(key); got != want {
			t.Fatalf("query[%q] = %q, want %q", key, got, want)
		}
	}
	// The redirect_uri must be percent-encoded in the raw query so Feishu receives
	// it intact (the ":" and "/" are escaped).
	if raw := parsed.RawQuery; !containsEncodedRedirect(raw) {
		t.Fatalf("raw query %q does not contain an encoded redirect_uri", raw)
	}
}

func containsEncodedRedirect(rawQuery string) bool {
	// url.Values.Encode escapes ":" as %3A and "/" as %2F.
	return len(rawQuery) > 0 &&
		(indexOf(rawQuery, "redirect_uri=http%3A%2F%2F127.0.0.1%3A54321%2Fcallback") >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestParseFeishuAccessTokenResponse(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		body := []byte(`{"code":0,"msg":"success","data":{"open_id":"ou_abc123","name":"Elian","access_token":"u-tok"}}`)
		openID, name, err := parseFeishuAccessTokenResponse(body)
		if err != nil {
			t.Fatalf("parseFeishuAccessTokenResponse error = %v", err)
		}
		if openID != "ou_abc123" {
			t.Fatalf("openID = %q, want ou_abc123", openID)
		}
		if name != "Elian" {
			t.Fatalf("name = %q, want Elian", name)
		}
	})

	t.Run("api error code", func(t *testing.T) {
		body := []byte(`{"code":20037,"msg":"invalid code","data":{}}`)
		if _, _, err := parseFeishuAccessTokenResponse(body); err == nil {
			t.Fatal("expected error for non-zero code, got nil")
		}
	})

	t.Run("missing open_id", func(t *testing.T) {
		body := []byte(`{"code":0,"msg":"success","data":{"name":"Elian"}}`)
		if _, _, err := parseFeishuAccessTokenResponse(body); err == nil {
			t.Fatal("expected error for missing open_id, got nil")
		}
	})
}

func TestResolveFeishuLoginProjectID(t *testing.T) {
	single := mustParsePartial(t, `{"projects":[{"id":"only","repoPath":"/tmp/only"}]}`)
	multi := mustParsePartial(t, `{"projects":[{"id":"a","repoPath":"/tmp/a"},{"id":"b","repoPath":"/tmp/b"}]}`)
	empty := config.PartialConfig{}

	if got, err := resolveFeishuLoginProjectID(single, ""); err != nil || got != "only" {
		t.Fatalf("single project resolve = %q, err = %v; want only", got, err)
	}
	if got, err := resolveFeishuLoginProjectID(multi, "b"); err != nil || got != "b" {
		t.Fatalf("explicit project resolve = %q, err = %v; want b", got, err)
	}
	if _, err := resolveFeishuLoginProjectID(multi, ""); err == nil {
		t.Fatal("expected ambiguity error for multiple projects without --project")
	}
	if _, err := resolveFeishuLoginProjectID(multi, "missing"); err == nil {
		t.Fatal("expected error for unknown project id")
	}
	if _, err := resolveFeishuLoginProjectID(empty, ""); err == nil {
		t.Fatal("expected error when no projects configured")
	}
}

func TestSetProjectOwnerInPartialWriteAndReadBack(t *testing.T) {
	partial := mustParsePartial(t, `{"projects":[{"id":"a","repoPath":"/tmp/a"},{"id":"b","repoPath":"/tmp/b"}]}`)

	index, err := setProjectOwnerInPartial(&partial, "b", "ou_owner999")
	if err != nil {
		t.Fatalf("setProjectOwnerInPartial error = %v", err)
	}
	if index != 1 {
		t.Fatalf("index = %d, want 1", index)
	}

	// The owner must survive a JSON round-trip at the exact projects[N].owner path.
	raw, err := json.Marshal(partial)
	if err != nil {
		t.Fatalf("json.Marshal(partial) error = %v", err)
	}
	var reparsed config.PartialConfig
	if err := json.Unmarshal(raw, &reparsed); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if reparsed.Projects == nil {
		t.Fatal("reparsed projects nil")
	}
	got := (*reparsed.Projects)[1].Owner
	if got == nil || got.FeishuOpenID != "ou_owner999" {
		t.Fatalf("projects[1].owner = %+v, want feishuOpenId ou_owner999", got)
	}
	// The other project must be untouched.
	if (*reparsed.Projects)[0].Owner != nil {
		t.Fatalf("projects[0].owner = %+v, want nil", (*reparsed.Projects)[0].Owner)
	}

	// And the config resolver must surface it through the normalized config.
	normalized, err := config.Normalize(t.TempDir(), partial)
	if err != nil {
		t.Fatalf("config.Normalize error = %v", err)
	}
	if got := config.ProjectOwner(normalized, "b"); got != "ou_owner999" {
		t.Fatalf("config.ProjectOwner = %q, want ou_owner999", got)
	}
}

func TestSetProjectOwnerInPartialUnknownID(t *testing.T) {
	partial := mustParsePartial(t, `{"projects":[{"id":"a","repoPath":"/tmp/a"}]}`)
	if _, err := setProjectOwnerInPartial(&partial, "nope", "ou_x"); err == nil {
		t.Fatal("expected error for unknown project id")
	}
}

func mustParsePartial(t *testing.T, raw string) config.PartialConfig {
	t.Helper()
	var partial config.PartialConfig
	if err := json.Unmarshal([]byte(raw), &partial); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", raw, err)
	}
	return partial
}

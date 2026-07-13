package git

import "testing"

func TestParseRemoteRepoFromURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		url      string
		wantHost string
		wantRepo string
	}{
		{url: "git@github.com:nexu-io/looper.git", wantHost: "github.com", wantRepo: "nexu-io/looper"},
		{url: "ssh://git@github.com/nexu-io/looper.git", wantHost: "github.com", wantRepo: "nexu-io/looper"},
		{url: "https://github.com/nexu-io/looper.git", wantHost: "github.com", wantRepo: "nexu-io/looper"},
		{url: "ssh://git@ssh.code.powerformer.net/core/odcrew.git", wantHost: "ssh.code.powerformer.net", wantRepo: "core/odcrew"},
		{url: "ssh://git@ssh.code.powerformer.net:2222/core/odcrew.git", wantHost: "ssh.code.powerformer.net", wantRepo: "core/odcrew"},
		{url: "https://code.powerformer.net/core/odcrew.git", wantHost: "code.powerformer.net", wantRepo: "core/odcrew"},
		{url: "git@code.powerformer.net:core/odcrew.git", wantHost: "code.powerformer.net", wantRepo: "core/odcrew"},
		{url: "forgejo@code.powerformer.net:core/odcrew.git", wantHost: "code.powerformer.net", wantRepo: "core/odcrew"},
		{url: "forgejo@[2001:db8::1]:core/odcrew.git", wantHost: "2001:db8::1", wantRepo: "core/odcrew"},
		{url: "", wantHost: "", wantRepo: ""},
		{url: "not-a-remote", wantHost: "", wantRepo: ""},
	}

	for _, tc := range cases {
		host, repo := parseRemoteRepoFromURL(tc.url)
		if host != tc.wantHost || repo != tc.wantRepo {
			t.Fatalf("parseRemoteRepoFromURL(%q) = (%q, %q), want (%q, %q)", tc.url, host, repo, tc.wantHost, tc.wantRepo)
		}
	}
}

func TestParseGitHubRepoFromRemoteURLIgnoresForgejo(t *testing.T) {
	t.Parallel()
	if got := parseGitHubRepoFromRemoteURL("ssh://git@ssh.code.powerformer.net/core/odcrew.git"); got != "" {
		t.Fatalf("parseGitHubRepoFromRemoteURL(forgejo) = %q, want empty", got)
	}
	if got := parseGitHubRepoFromRemoteURL("git@github.com:nexu-io/looper.git"); got != "nexu-io/looper" {
		t.Fatalf("parseGitHubRepoFromRemoteURL(github) = %q, want nexu-io/looper", got)
	}
}

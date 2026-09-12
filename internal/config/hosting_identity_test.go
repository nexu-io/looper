package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func hostingIdentityFixture(t *testing.T) Config {
	t.Helper()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Notifications.Osascript.Enabled = false
	cfg.Identities = map[string]HostingIdentityConfig{}
	for _, name := range []string{"project-bot", "global-role-bot", "project-role-bot"} {
		cfg.Identities[name] = HostingIdentityConfig{
			Kind: HostingIdentityGitHubApp, AppID: 1, InstallationID: 2,
			PrivateKeyFile: "/missing/app.pem", BaseURL: "https://github.com",
		}
	}
	cfg.Projects = []ProjectRefConfig{{ID: "project", Name: "Project", Repo: "owner/repo", RepoPath: t.TempDir()}}
	return cfg
}

func roleIdentityPartial(t *testing.T, role, name string) *PartialRoleConfigs {
	t.Helper()
	raw, err := json.Marshal(map[string]any{role: map[string]string{"identity": name}})
	if err != nil {
		t.Fatal(err)
	}
	var roles PartialRoleConfigs
	if err := json.Unmarshal(raw, &roles); err != nil {
		t.Fatal(err)
	}
	return &roles
}

func TestHostingIdentityInheritanceEveryRole(t *testing.T) {
	for _, role := range hostingIdentityRoles {
		t.Run(role, func(t *testing.T) {
			cfg := hostingIdentityFixture(t)
			check := func(want string) ResolvedHostingIdentity {
				t.Helper()
				got, selected, err := ResolveHostingIdentity(cfg, "project", role)
				if err != nil || selected != (want != "") || got.Name != want {
					t.Fatalf("ResolveHostingIdentity() = (%#v, %v, %v), want %q", got, selected, err, want)
				}
				return got
			}
			check("")
			cfg.Projects[0].Identity = "project-bot"
			check("project-bot")
			mergeRoleConfigs(&cfg.Roles, *roleIdentityPartial(t, role, "global-role-bot"))
			check("global-role-bot")
			cfg.Projects[0].Roles = roleIdentityPartial(t, role, "project-role-bot")
			captured := check("project-role-bot")
			cfg.Projects[0].Roles = roleIdentityPartial(t, role, "")
			check("project-bot")
			cfg.Identities["project-role-bot"] = HostingIdentityConfig{Kind: HostingIdentityForgejoToken, TokenEnv: "CHANGED"}
			cfg.Projects[0].Repo = "different/repo"
			if captured.Definition.Kind != HostingIdentityGitHubApp || captured.Target.Repo != "owner/repo" || captured.ProjectID != "project" || captured.Role != role {
				t.Fatalf("captured binding changed: %#v", captured)
			}
			if name, _ := roleHostingIdentity(cfg.Roles, role); name != "global-role-bot" {
				t.Fatalf("project resolution mutated global role: %q", name)
			}
		})
	}
}

func TestHostingIdentitySelectionFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"missing definition", func(cfg *Config) { delete(cfg.Identities, "project-bot") }},
		{"incomplete definition", func(cfg *Config) {
			cfg.Identities["project-bot"] = HostingIdentityConfig{Kind: HostingIdentityGitHubApp}
		}},
		{"provider mismatch", func(cfg *Config) {
			cfg.Identities["project-bot"] = HostingIdentityConfig{Kind: HostingIdentityForgejoToken, BaseURL: "https://github.com", TokenEnv: "BOT_TOKEN"}
		}},
		{"instance mismatch", func(cfg *Config) {
			value := cfg.Identities["project-bot"]
			value.BaseURL = "https://github.other.example"
			cfg.Identities["project-bot"] = value
		}},
		{"unknown provider", func(cfg *Config) { cfg.Projects[0].Provider = "missing" }},
		{"unknown repository", func(cfg *Config) { cfg.Projects[0].Repo = "" }},
		{"malformed repository", func(cfg *Config) { cfg.Projects[0].Repo = "owner/../escape" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := hostingIdentityFixture(t)
			cfg.Projects[0].Identity = "project-bot"
			tt.edit(&cfg)
			if _, selected, err := ResolveHostingIdentity(cfg, "project", "reviewer"); err == nil || !selected {
				t.Fatalf("selected invalid identity returned selected=%v, err=%v", selected, err)
			}
			if err := ValidateHostingIdentities(cfg); err == nil {
				t.Fatal("static validation accepted selected invalid identity")
			}
		})
	}
}

func TestHostingIdentityLegacyProjectsDoNotResolveCredentials(t *testing.T) {
	cfg := hostingIdentityFixture(t)
	cfg.Providers = []ProviderConfig{{ID: "forgejo", Kind: ProviderKindForgejo, BaseURL: "https://code.example", Auth: ProviderAuthTea, TeaLogin: stringPtr("personal")}}
	cfg.Projects[0].Provider = "forgejo"
	for _, projectID := range []string{"project", "not-configured"} {
		for _, role := range hostingIdentityRoles {
			if _, selected, err := ResolveHostingIdentity(cfg, projectID, role); selected || err != nil {
				t.Fatalf("legacy resolution = (%v, %v)", selected, err)
			}
		}
	}
}

func TestHostingIdentitiesLoadAcrossFormatsAndDetach(t *testing.T) {
	for _, format := range []string{"json", "toml", "yaml"} {
		t.Run(format, func(t *testing.T) {
			cwd := t.TempDir()
			identities := map[string]HostingIdentityConfig{
				"bot":     {Kind: HostingIdentityGitHubApp, AppID: 123, InstallationID: 456, PrivateKeyFile: " keys/bot.pem ", Commit: HostingCommitIdentity{Name: " Bot ", Email: " bot@example.test "}},
				"forgejo": {Kind: HostingIdentityForgejoToken, BaseURL: " https://CODE.example.test/team/ ", TokenEnv: " FORGEJO_TOKEN "},
			}
			projects := []PartialProjectRefConfig{{ID: "project", Name: "Project", RepoPath: cwd, Repo: stringPtr("Owner/Repo"), Identity: stringPtr(" bot "), Roles: roleIdentityPartial(t, "reviewer", " bot ")}}
			partial := PartialConfig{Identities: &identities, Projects: &projects, Roles: roleIdentityPartial(t, "coordinator", " bot ")}
			raw, err := MarshalConfigFile("config."+format, partial)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cwd, "config."+format)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: path, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/detected/git", "gh": "/detected/gh", "osascript": "/detected/osascript"})})
			if err != nil {
				t.Fatalf("LoadFile() error = %v", err)
			}
			cfg := loaded.Config
			bot := cfg.Identities["bot"]
			if bot.BaseURL != "https://github.com" || bot.PrivateKeyFile != filepath.Join(cwd, "keys/bot.pem") || bot.AppID != 123 || bot.InstallationID != 456 || bot.Commit.Name != "Bot" || bot.Commit.Email != "bot@example.test" {
				t.Fatalf("normalized bot = %#v", bot)
			}
			if forgejo := cfg.Identities["forgejo"]; forgejo.BaseURL != "https://code.example.test/team" || forgejo.TokenEnv != "FORGEJO_TOKEN" {
				t.Fatalf("normalized Forgejo identity = %#v", forgejo)
			}
			for _, role := range hostingIdentityRoles {
				if got, selected, err := ResolveHostingIdentity(cfg, "project", role); err != nil || !selected || got.Name != "bot" {
					t.Fatalf("%s binding = (%#v, %v, %v)", role, got, selected, err)
				}
			}
			clone := CloneConfig(cfg)
			clone.Identities["bot"] = HostingIdentityConfig{}
			*clone.Projects[0].Roles.Reviewer.Identity = "changed"
			clone.Roles.Coordinator.Identity = "changed"
			if cfg.Identities["bot"] != bot || *cfg.Projects[0].Roles.Reviewer.Identity != " bot " || cfg.Roles.Coordinator.Identity != "bot" {
				t.Fatal("CloneConfig leaked a mutable identity reference")
			}
		})
	}
}

func TestNormalizeHostingIdentitiesDoesNotAliasCallerOrMixDefinitionLayers(t *testing.T) {
	first := map[string]HostingIdentityConfig{"bot": {Kind: HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: "key.pem"}}
	second := map[string]HostingIdentityConfig{"bot": {Kind: HostingIdentityForgejoToken, BaseURL: "https://code.example", TokenEnv: "BOT_TOKEN"}}
	role := "bot"
	projects := []PartialProjectRefConfig{{ID: "project", Roles: &PartialRoleConfigs{Worker: &PartialWorkerRoleConfig{Identity: &role}}}}
	cfg, err := Normalize(t.TempDir(), PartialConfig{Identities: &first}, PartialConfig{Identities: &second, Projects: &projects})
	if err != nil {
		t.Fatal(err)
	}
	second["bot"] = HostingIdentityConfig{}
	role = "changed"
	if definition := cfg.Identities["bot"]; definition.Kind != HostingIdentityForgejoToken || definition.AppID != 0 || definition.PrivateKeyFile != "" || definition.TokenEnv != "BOT_TOKEN" {
		t.Fatalf("definition mixed layers or aliased caller: %#v", definition)
	}
	if got := *cfg.Projects[0].Roles.Worker.Identity; got != "bot" {
		t.Fatalf("project identity aliased caller: %q", got)
	}
}

func TestValidateHostingIdentitiesReportsEveryStaticIssueWithoutReadingCredentials(t *testing.T) {
	cfg := hostingIdentityFixture(t)
	cfg.Identities = map[string]HostingIdentityConfig{
		"bad name": {Kind: HostingIdentityGitHubApp, BaseURL: "https://user:password@example.test?q=secret", TokenEnv: "TOKEN"},
		"forgejo":  {Kind: HostingIdentityForgejoToken, BaseURL: "https://code.example", TokenEnv: "not a reference", AppID: 2, InstallationID: 3, PrivateKeyFile: "/key", Commit: HostingCommitIdentity{Email: "bad\nmail"}},
	}
	cfg.Projects[0].Identity = "missing"
	for _, role := range hostingIdentityRoles {
		mergeRoleConfigs(&cfg.Roles, *roleIdentityPartial(t, role, "unknown-"+role))
	}
	err := ValidateHostingIdentities(cfg)
	var validation *ConfigValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("got %v, want ConfigValidationError", err)
	}
	want := []string{"identities.bad name", "identities.bad name.baseUrl", "identities.bad name.appId", "identities.bad name.installationId", "identities.bad name.privateKeyFile", "identities.bad name.tokenEnv", "identities.forgejo.tokenEnv", "identities.forgejo.appId", "identities.forgejo.installationId", "identities.forgejo.privateKeyFile", "identities.forgejo.commit.email", "projects[0].identity"}
	for _, role := range hostingIdentityRoles {
		want = append(want, "roles."+role+".identity", "projects[0].roles."+role+".identity")
	}
	assertValidationIssueForPaths(t, validation.Issues, want)
	for _, secret := range []string{"password", "q=secret", "not a reference", "bad\nmail"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("validation error leaked a value: %v", err)
		}
	}
	valid := hostingIdentityFixture(t)
	valid.Projects[0].Identity = "project-bot"
	if err := Validate(valid); err != nil {
		t.Fatalf("missing key file is an external auth failure, not schema error: %v", err)
	}
}

func TestHostingIdentityInstanceBindingAndPlaneCodeRepository(t *testing.T) {
	cfg := hostingIdentityFixture(t)
	cfg.Identities["forgejo"] = HostingIdentityConfig{Kind: HostingIdentityForgejoToken, BaseURL: "https://code.example/team", TokenEnv: "UNSET_FORGEJO_IDENTITY_TOKEN"}
	cfg.Providers = []ProviderConfig{{ID: "forgejo", Kind: ProviderKindForgejo, BaseURL: "https://CODE.example/team/"}, {ID: "plane", Kind: ProviderKindPlane}}
	cfg.Projects[0].Provider = "forgejo"
	cfg.Projects[0].Identity = "forgejo"
	got, selected, err := ResolveHostingIdentity(cfg, "project", "worker")
	if err != nil || !selected || got.Target.Kind != ProviderKindForgejo || got.Target.BaseURL != "https://code.example/team" {
		t.Fatalf("Forgejo binding = (%#v, %v, %v)", got, selected, err)
	}
	cfg.Projects[0].Provider = "plane"
	cfg.Projects[0].Identity = "project-bot"
	got, selected, err = ResolveHostingIdentity(cfg, "project", "worker")
	if err != nil || !selected || got.Target.Kind != ProviderKindGitHub || got.Target.Repo != "owner/repo" {
		t.Fatalf("Plane code repository binding = (%#v, %v, %v)", got, selected, err)
	}
}

func TestBotOnlyForgejoDoesNotRequireLegacyCredentials(t *testing.T) {
	cfg := hostingIdentityFixture(t)
	cfg.Identities = map[string]HostingIdentityConfig{"bot": {Kind: HostingIdentityForgejoToken, BaseURL: "https://code.example", TokenEnv: "UNSET_BOT_TOKEN"}}
	cfg.Providers = []ProviderConfig{{ID: "forgejo", Kind: ProviderKindForgejo, BaseURL: "https://code.example"}}
	cfg.Projects[0].Provider = "forgejo"
	cfg.Projects[0].Identity = "bot"
	ApplyForgejoProjectProfile(&cfg.Projects[0])
	if err := Validate(cfg); err != nil {
		t.Fatalf("bot-only config required unused provider credentials: %v", err)
	}
	cfg.Providers[0].Auth = ProviderAuthTea
	cfg.Providers[0].TeaLogin = stringPtr("missing-login")
	cfg.Providers[0].TeaPath = stringPtr("/missing/tea")
	if err := Validate(cfg); err != nil {
		t.Fatalf("static validation probed unused legacy tea: %v", err)
	}
	cfg.Providers[0].Auth = ""
	cfg.Providers[0].TeaLogin = nil
	cfg.Projects[0].Identity = ""
	for _, role := range []string{"planner", "reviewer", "worker", "fixer"} {
		mergeRoleConfigs(&cfg.Roles, *roleIdentityPartial(t, role, "bot"))
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("all coding roles selecting bots still required legacy auth: %v", err)
	}
	cfg.Roles.Worker.Identity = ""
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "providers[0].auth") {
		t.Fatalf("uncovered legacy role must require provider credentials: %v", err)
	}
}

func TestHostingIdentityHotPolicyAndProjectImportBoundary(t *testing.T) {
	old := hostingIdentityFixture(t)
	next := CloneConfig(old)
	next.Identities["new-bot"] = HostingIdentityConfig{Kind: HostingIdentityGitHubApp, AppID: 3, InstallationID: 4, PrivateKeyFile: "/new.pem"}
	next.Roles.Reviewer.Identity = "new-bot"
	next.Roles.Coordinator.Identity = "new-bot"
	if got := RestartRequiredChanges(old, next); len(got) != 0 {
		t.Fatalf("identity policy must apply to new runs without restart: %v", got)
	}
	for _, path := range []string{"identities.new-bot", "identities.new-bot.appId", "identities.new-bot.commit.email", "roles.reviewer.identity", "roles.coordinator.identity"} {
		if !IsHotEditablePath(path) || !IsFieldLevelConfigPath(path) {
			t.Errorf("identity field %q is not hot editable", path)
		}
	}
	for _, path := range []string{"identities", "identities.new-bot.token", "identities.new-bot.commit.secret", "roles.unknown.identity", "roles.auditor.identity", "projects.0.identity"} {
		if IsHotEditablePath(path) {
			t.Errorf("unexpected hot path: %q", path)
		}
	}
	next = CloneConfig(old)
	next.Projects[0].Identity = "project-bot"
	if got := RestartRequiredChanges(old, next); !reflect.DeepEqual(got, []string{"projects"}) {
		t.Fatalf("file projects remain import input: %v", got)
	}
}

func TestHostingIdentityPrivateKeyHomeReferenceAndGitHubBotEmail(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	identities := map[string]HostingIdentityConfig{"bot": {
		Kind: HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: "~/.looper/keys/bot.pem",
		Commit: HostingCommitIdentity{Email: "9917+example[bot]@users.noreply.github.com"},
	}}
	cfg, err := Normalize(t.TempDir(), PartialConfig{Identities: &identities})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Identities["bot"].PrivateKeyFile != filepath.Join(homeDir, ".looper/keys/bot.pem") {
		t.Fatalf("home-relative key reference = %q", cfg.Identities["bot"].PrivateKeyFile)
	}
	if err := ValidateHostingIdentities(cfg); err != nil {
		t.Fatalf("official GitHub bot noreply email was rejected: %v", err)
	}
}

func TestHostingIdentityForgejoFileWithoutProjectsRequiresMatchingValidDefinition(t *testing.T) {
	for _, test := range []struct {
		name       string
		definition *HostingIdentityConfig
		wantValid  bool
	}{
		{name: "matching", definition: &HostingIdentityConfig{Kind: HostingIdentityForgejoToken, BaseURL: "https://code.example/team", TokenEnv: "BOT_TOKEN"}, wantValid: true},
		{name: "wrong instance", definition: &HostingIdentityConfig{Kind: HostingIdentityForgejoToken, BaseURL: "https://other.example/team", TokenEnv: "BOT_TOKEN"}},
		{name: "invalid matching definition", definition: &HostingIdentityConfig{Kind: HostingIdentityForgejoToken, BaseURL: "https://code.example/team"}},
		{name: "legacy provider without auth"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := hostingIdentityFixture(t)
			cfg.Projects = nil
			cfg.Providers = []ProviderConfig{{ID: "forgejo", Kind: ProviderKindForgejo, BaseURL: "https://CODE.example/team/"}}
			if test.definition != nil {
				cfg.Identities["forgejo-bot"] = *test.definition
			}
			err := Validate(cfg)
			if test.wantValid && err != nil {
				t.Fatalf("provider prepared for SQLite bot projects did not validate: %v", err)
			}
			if !test.wantValid && (err == nil || !strings.Contains(err.Error(), "providers[0].auth")) {
				t.Fatalf("unrelated or invalid identities bypassed legacy auth validation: %v", err)
			}
		})
	}
}

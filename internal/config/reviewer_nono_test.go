package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReviewerNonoConfigRoundTripAndAuthority(t *testing.T) {
	original := canonicalRoundTripFixtureConfig()
	original.Roles.Reviewer.Nono = &ReviewerNonoConfig{Command: "/opt/nono", Profile: "/etc/looper/reviewer.json"}
	for _, format := range []string{"json", "yaml", "toml"} {
		got := roundTripConfigViaStructuredFormat(t, original, format)
		if !reflect.DeepEqual(got.Roles.Reviewer.Nono, original.Roles.Reviewer.Nono) {
			t.Fatalf("%s lost nono policy: %+v", format, got.Roles.Reviewer.Nono)
		}
	}
	before := original
	before.Roles.Reviewer.Nono = nil
	if len(RestartRequiredChanges(before, original)) == 0 || IsHotEditablePath("roles.reviewer.nono.profile") {
		t.Fatal("nono policy must require restart")
	}
	projectPolicy := &ReviewerNonoConfig{Profile: "/untrusted/profile.json"}
	original.Projects[0].Roles = &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Nono: projectPolicy}}
	if got := ProjectRoleConfigs(original, original.Projects[0].ID).Reviewer.Nono; !reflect.DeepEqual(got, original.Roles.Reviewer.Nono) {
		t.Fatal("project overrode operator policy")
	}
	if err := Validate(original); err == nil || !strings.Contains(err.Error(), "nono is global-only") {
		t.Fatalf("project policy validation: %v", err)
	}
	original.Projects[0].Roles = nil
	original.Roles.Reviewer.Nono = &ReviewerNonoConfig{Profile: "relative.json"}
	if err := Validate(original); err == nil || !strings.Contains(err.Error(), "roles.reviewer.nono.profile") {
		t.Fatalf("relative policy validation: %v", err)
	}
}

func TestReviewerNonoNormalizeCopiesPolicy(t *testing.T) {
	policy := &ReviewerNonoConfig{Profile: filepath.Join(t.TempDir(), "profile.json")}
	partial := PartialConfig{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Nono: policy}}}
	got, err := Normalize(t.TempDir(), partial)
	if err != nil {
		t.Fatal(err)
	}
	policy.Profile = "changed"
	if got.Roles.Reviewer.Nono.Profile == policy.Profile {
		t.Fatal("normalized policy aliases input")
	}
}

package config

import (
	"strings"
	"testing"
)

func TestAgentSnapshotMarshalRoundTrip(t *testing.T) {
	t.Parallel()

	model := "gpt-5"
	original := AgentSnapshot{
		Vendor:    string(AgentVendorCodex),
		Model:     &model,
		ProfileID: "fast",
	}
	encoded, err := MarshalAgentSnapshot(original)
	if err != nil {
		t.Fatalf("MarshalAgentSnapshot() error = %v", err)
	}
	got, err := ParseAgentSnapshot(encoded)
	if err != nil {
		t.Fatalf("ParseAgentSnapshot() error = %v", err)
	}
	if got.Vendor != original.Vendor {
		t.Fatalf("Vendor = %q, want %q", got.Vendor, original.Vendor)
	}
	if got.Model == nil || *got.Model != model {
		t.Fatalf("Model = %v, want %q", got.Model, model)
	}
	if got.ProfileID != original.ProfileID {
		t.Fatalf("ProfileID = %q, want %q", got.ProfileID, original.ProfileID)
	}
}

func TestAgentSnapshotFromResolvedAndIdentity(t *testing.T) {
	t.Parallel()

	model := "claude-sonnet"
	resolved := ResolvedAgent{
		Vendor:    AgentVendorClaudeCode,
		Model:     &model,
		ProfileID: "review",
	}
	fromResolved := AgentSnapshotFromResolved(resolved)
	if fromResolved.Vendor != string(AgentVendorClaudeCode) || fromResolved.ProfileID != "review" {
		t.Fatalf("AgentSnapshotFromResolved() = %#v", fromResolved)
	}
	if fromResolved.Model == nil || *fromResolved.Model != model {
		t.Fatalf("AgentSnapshotFromResolved().Model = %v, want %q", fromResolved.Model, model)
	}

	opencodeModel := "opencode-model"
	fromIdentity := AgentSnapshotFromIdentity(string(AgentVendorOpenCode), &opencodeModel, "worker-profile")
	if fromIdentity.Vendor != string(AgentVendorOpenCode) || fromIdentity.ProfileID != "worker-profile" {
		t.Fatalf("AgentSnapshotFromIdentity() = %#v", fromIdentity)
	}
	if fromIdentity.Model == nil || *fromIdentity.Model != "opencode-model" {
		t.Fatalf("AgentSnapshotFromIdentity().Model = %v", fromIdentity.Model)
	}

	empty := ""
	fromEmpty := AgentSnapshotFromIdentity(string(AgentVendorClaudeCode), &empty, "review")
	if fromEmpty.Model == nil || *fromEmpty.Model != "" {
		t.Fatalf("AgentSnapshotFromIdentity(empty model) = %#v, want non-nil empty model", fromEmpty)
	}
	if AgentSnapshotFromIdentity(string(AgentVendorClaudeCode), nil, "review").Model != nil {
		t.Fatal("AgentSnapshotFromIdentity(nil model) should leave Model unset")
	}
}

func TestResolveRunAgentSnapshotJSON_CopiesPredecessorOnResume(t *testing.T) {
	t.Parallel()

	predecessor := `{"vendor":"codex","model":"old-model","profileId":"sticky"}`
	got, legacy, err := ResolveRunAgentSnapshotJSON(&predecessor, true, string(AgentVendorClaudeCode), strPtr("new-model"), "new-profile")
	if err != nil {
		t.Fatalf("ResolveRunAgentSnapshotJSON() error = %v", err)
	}
	if legacy {
		t.Fatal("legacyResume = true, want false when predecessor has snapshot")
	}
	if got == nil || *got != predecessor {
		t.Fatalf("snapshot = %v, want predecessor copy %q", got, predecessor)
	}
}

func TestResolveRunAgentSnapshotJSON_LegacyResumeUsesCurrentIdentity(t *testing.T) {
	t.Parallel()

	got, legacy, err := ResolveRunAgentSnapshotJSON(nil, true, string(AgentVendorCodex), strPtr("gpt-5"), "fast")
	if err != nil {
		t.Fatalf("ResolveRunAgentSnapshotJSON() error = %v", err)
	}
	if !legacy {
		t.Fatal("legacyResume = false, want true when predecessor snapshot missing")
	}
	if got == nil {
		t.Fatal("snapshot = nil, want current identity")
	}
	parsed, err := ParseAgentSnapshot(*got)
	if err != nil {
		t.Fatalf("ParseAgentSnapshot() error = %v", err)
	}
	if parsed.Vendor != string(AgentVendorCodex) || parsed.ProfileID != "fast" {
		t.Fatalf("parsed = %#v", parsed)
	}
	if parsed.Model == nil || *parsed.Model != "gpt-5" {
		t.Fatalf("parsed.Model = %v, want gpt-5", parsed.Model)
	}
}

func TestIdentityFromRunSnapshot_UsesSnapshotWhenPresent(t *testing.T) {
	t.Parallel()

	raw := `{"vendor":"codex","model":"sticky-model","profileId":"sticky-profile"}`
	vendor, model, profile, fromSnapshot, err := IdentityFromRunSnapshot(&raw, string(AgentVendorClaudeCode), strPtr("fallback-model"), "fallback-profile")
	if err != nil {
		t.Fatalf("IdentityFromRunSnapshot() error = %v", err)
	}
	if !fromSnapshot {
		t.Fatal("fromSnapshot = false, want true")
	}
	if vendor != string(AgentVendorCodex) || model == nil || *model != "sticky-model" || profile != "sticky-profile" {
		t.Fatalf("identity = (%q, %v, %q)", vendor, model, profile)
	}
}

func TestIdentityFromRunSnapshot_FallsBackWhenEmpty(t *testing.T) {
	t.Parallel()

	vendor, model, profile, fromSnapshot, err := IdentityFromRunSnapshot(nil, string(AgentVendorOpenCode), strPtr("opencode-model"), "worker")
	if err != nil {
		t.Fatalf("IdentityFromRunSnapshot() error = %v", err)
	}
	if fromSnapshot {
		t.Fatal("fromSnapshot = true, want false")
	}
	if vendor != string(AgentVendorOpenCode) || model == nil || *model != "opencode-model" || profile != "worker" {
		t.Fatalf("identity = (%q, %v, %q)", vendor, model, profile)
	}
}

func TestIdentityFromRunSnapshot_MalformedReturnsError(t *testing.T) {
	t.Parallel()

	raw := `{not-json`
	_, _, _, _, err := IdentityFromRunSnapshot(&raw, string(AgentVendorCodex), strPtr("m"), "p")
	if err == nil {
		t.Fatal("IdentityFromRunSnapshot() error = nil, want parse error")
	}
}

func TestIdentityFromRunSnapshot_EmptyVendorReturnsError(t *testing.T) {
	t.Parallel()

	raw := `{"vendor":"","model":"m","profileId":"p"}`
	_, _, _, _, err := IdentityFromRunSnapshot(&raw, string(AgentVendorCodex), strPtr("fallback-model"), "fallback-profile")
	if err == nil {
		t.Fatal("IdentityFromRunSnapshot() error = nil, want missing vendor")
	}
	if !strings.Contains(err.Error(), "agent snapshot missing vendor") {
		t.Fatalf("IdentityFromRunSnapshot() error = %v, want missing vendor", err)
	}
}

func TestIdentityFromRunSnapshot_WhitespaceVendorReturnsError(t *testing.T) {
	t.Parallel()

	raw := `{"vendor":"   ","model":"m"}`
	_, _, _, _, err := IdentityFromRunSnapshot(&raw, string(AgentVendorOpenCode), strPtr("m"), "p")
	if err == nil {
		t.Fatal("IdentityFromRunSnapshot() error = nil, want missing vendor")
	}
}

func TestResolveRunAgentSnapshotJSON_RejectsInvalidPredecessor(t *testing.T) {
	t.Parallel()

	malformed := `{not-json`
	_, _, err := ResolveRunAgentSnapshotJSON(&malformed, true, string(AgentVendorCodex), strPtr("gpt-5"), "fast")
	if err == nil {
		t.Fatal("ResolveRunAgentSnapshotJSON() error = nil, want parse error for malformed predecessor")
	}

	emptyVendor := `{"vendor":"","model":"old"}`
	_, _, err = ResolveRunAgentSnapshotJSON(&emptyVendor, true, string(AgentVendorClaudeCode), strPtr("new-model"), "new-profile")
	if err == nil {
		t.Fatal("ResolveRunAgentSnapshotJSON() error = nil, want missing vendor for predecessor")
	}
	if !strings.Contains(err.Error(), "agent snapshot missing vendor") {
		t.Fatalf("ResolveRunAgentSnapshotJSON() error = %v, want missing vendor", err)
	}
}

func TestResolveRunAgentSnapshotJSON_EmptyPredecessorIsLegacy(t *testing.T) {
	t.Parallel()

	empty := "   "
	got, legacy, err := ResolveRunAgentSnapshotJSON(&empty, true, string(AgentVendorCodex), strPtr("gpt-5"), "fast")
	if err != nil {
		t.Fatalf("ResolveRunAgentSnapshotJSON() error = %v", err)
	}
	if !legacy {
		t.Fatal("legacyResume = false, want true for empty predecessor snapshot")
	}
	if got == nil {
		t.Fatal("snapshot = nil, want current identity")
	}
	parsed, err := ParseAgentSnapshot(*got)
	if err != nil {
		t.Fatalf("ParseAgentSnapshot() error = %v", err)
	}
	if parsed.Vendor != string(AgentVendorCodex) {
		t.Fatalf("parsed.Vendor = %q, want codex", parsed.Vendor)
	}
}

func TestResolveRunAgentSnapshotJSON_EmptyVendorLeavesNil(t *testing.T) {
	t.Parallel()

	got, legacy, err := ResolveRunAgentSnapshotJSON(nil, false, "", strPtr("model"), "profile")
	if err != nil {
		t.Fatalf("ResolveRunAgentSnapshotJSON() error = %v", err)
	}
	if legacy {
		t.Fatal("legacyResume = true, want false for non-sticky create")
	}
	if got != nil {
		t.Fatalf("snapshot = %v, want nil when vendor is empty", got)
	}
}

func strPtr(value string) *string { return &value }

func TestResolveRunAgentSnapshotJSON_PreservesEmptyModel(t *testing.T) {
	t.Parallel()

	empty := ""
	got, legacy, err := ResolveRunAgentSnapshotJSON(nil, false, string(AgentVendorClaudeCode), &empty, "review")
	if err != nil {
		t.Fatalf("ResolveRunAgentSnapshotJSON() error = %v", err)
	}
	if legacy {
		t.Fatal("legacyResume = true, want false")
	}
	if got == nil {
		t.Fatal("snapshot = nil, want encoded identity")
	}
	parsed, err := ParseAgentSnapshot(*got)
	if err != nil {
		t.Fatalf("ParseAgentSnapshot() error = %v", err)
	}
	if parsed.Model == nil || *parsed.Model != "" {
		t.Fatalf("parsed.Model = %v, want non-nil empty", parsed.Model)
	}

	vendor, model, profile, fromSnapshot, err := IdentityFromRunSnapshot(got, string(AgentVendorCodex), strPtr("fallback"), "fb")
	if err != nil {
		t.Fatalf("IdentityFromRunSnapshot() error = %v", err)
	}
	if !fromSnapshot {
		t.Fatal("fromSnapshot = false, want true")
	}
	if vendor != string(AgentVendorClaudeCode) || profile != "review" {
		t.Fatalf("identity = (%q, %q)", vendor, profile)
	}
	if model == nil || *model != "" {
		t.Fatalf("model = %v, want non-nil empty suppress", model)
	}
}

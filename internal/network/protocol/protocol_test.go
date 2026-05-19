package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJoinRoundTripAndVersionValidation(t *testing.T) {
	joinedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	payload := JoinRequest{
		ProtocolVersion: CurrentVersion,
		DaemonVersion:   "1.2.3",
		JoinKey:         "join-123",
		NodeName:        "worker-1",
		GitHub:          GitHubIdentity{NumericID: 42, Login: "octo"},
		TargetLabels:    []string{"linux", "arm64"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded JoinRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.NodeName != payload.NodeName || decoded.GitHub.NumericID != payload.GitHub.NumericID {
		t.Fatalf("round trip mismatch: %#v", decoded)
	}
	if err := ValidateCompatibility(CurrentVersion, "1.2.3", "1.2.0"); err != nil {
		t.Fatalf("ValidateCompatibility() error = %v", err)
	}
	audit := AuditEnvelope{Event: "joined", Actor: "admin", OccurredAt: joinedAt, LeaseToken: 5}
	if _, err := json.Marshal(audit); err != nil {
		t.Fatalf("Marshal(audit) error = %v", err)
	}
	if err := ValidateCompatibility("loopernet/v2", "1.2.3", "1.2.0"); err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("ValidateCompatibility(protocol mismatch) error = %v", err)
	}
	if err := ValidateCompatibility(CurrentVersion, "1.1.9", "1.2.0"); err == nil || !strings.Contains(err.Error(), "minimum supported version") {
		t.Fatalf("ValidateCompatibility(daemon mismatch) error = %v", err)
	}
}

func TestValidateNodeName(t *testing.T) {
	valid := []string{"node1", "worker-1", "a1", "abc-123"}
	for _, name := range valid {
		if err := ValidateNodeName(name); err != nil {
			t.Fatalf("ValidateNodeName(%q) error = %v", name, err)
		}
	}
	invalid := []string{"", " worker-1", "worker-1 ", "Worker", "-node", "node-", "node_name", strings.Repeat("a", 33)}
	for _, name := range invalid {
		if err := ValidateNodeName(name); err == nil {
			t.Fatalf("ValidateNodeName(%q) error = nil, want error", name)
		}
	}
}

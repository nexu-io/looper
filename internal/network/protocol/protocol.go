package protocol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	CurrentVersion     = "loopernet/v1"
	DefaultLeaseName   = "coordinator"
	DefaultLeaseTTL    = 30 * time.Second
	MinimumDaemonField = "daemonVersion"
)

var nodeNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

type GitHubIdentity struct {
	NumericID int64  `json:"numericId"`
	Login     string `json:"login,omitempty"`
}

type NodeCapabilities struct {
	Roles               []string `json:"roles,omitempty"`
	CoordinatorEligible bool     `json:"coordinatorEligible"`
	RoutedProjects      int      `json:"routedProjects"`
	LocalProjects       int      `json:"localProjects"`
	DynamicLoad         int      `json:"dynamicLoad"`
	IdentityDrift       bool     `json:"identityDrift"`
	DriftReason         string   `json:"driftReason,omitempty"`
}

type AuditEnvelope struct {
	Event       string          `json:"event"`
	Actor       string          `json:"actor"`
	OccurredAt  time.Time       `json:"occurredAt"`
	NetworkID   string          `json:"networkId,omitempty"`
	NodeID      string          `json:"nodeId,omitempty"`
	LeaseName   string          `json:"leaseName,omitempty"`
	LeaseToken  int64           `json:"leaseToken,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	WarningText []string        `json:"warnings,omitempty"`
}

type JoinRequest struct {
	ProtocolVersion string         `json:"protocolVersion"`
	DaemonVersion   string         `json:"daemonVersion"`
	JoinKey         string         `json:"joinKey"`
	NodeName        string         `json:"nodeName"`
	GitHub          GitHubIdentity `json:"github"`
	TargetLabels    []string       `json:"targetLabels,omitempty"`
}

type JoinResponse struct {
	NetworkID string   `json:"networkId"`
	NodeID    string   `json:"nodeId"`
	NodeToken string   `json:"nodeToken"`
	Warnings  []string `json:"warnings,omitempty"`
}

type Membership struct {
	NodeID           string           `json:"nodeId"`
	NodeName         string           `json:"nodeName"`
	GitHub           GitHubIdentity   `json:"github"`
	Capabilities     NodeCapabilities `json:"capabilities"`
	TargetLabels     []string         `json:"targetLabels,omitempty"`
	JoinedAt         time.Time        `json:"joinedAt"`
	LastHeartbeatAt  *time.Time       `json:"lastHeartbeatAt,omitempty"`
	DuplicateWarning bool             `json:"duplicateGithubIdentityWarning,omitempty"`
}

type HeartbeatRequest struct {
	ProtocolVersion string           `json:"protocolVersion"`
	DaemonVersion   string           `json:"daemonVersion"`
	NodeName        string           `json:"nodeName"`
	GitHub          GitHubIdentity   `json:"github"`
	Capabilities    NodeCapabilities `json:"capabilities"`
}

type HeartbeatResponse struct {
	RecordedAt time.Time `json:"recordedAt"`
	Warnings   []string  `json:"warnings,omitempty"`
}

type CoordinatorLease struct {
	Name         string     `json:"name"`
	HolderNodeID string     `json:"holderNodeId,omitempty"`
	FencingToken int64      `json:"fencingToken"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
}

type CoordinatorLeaseAcquireRequest struct {
	TTLSeconds int `json:"ttlSeconds,omitempty"`
}

type CoordinatorLeaseRenewRequest struct {
	FencingToken int64 `json:"fencingToken"`
	TTLSeconds   int   `json:"ttlSeconds,omitempty"`
}

type CoordinatorLeaseHandoffRequest struct {
	FencingToken   int64  `json:"fencingToken"`
	TargetNodeName string `json:"targetNodeName"`
	TTLSeconds     int    `json:"ttlSeconds,omitempty"`
}

type CoordinatorLeaseRevalidateRequest struct {
	FencingToken int64  `json:"fencingToken"`
	URL          string `json:"url"`
	Method       string `json:"method,omitempty"`
}

type StatusResponse struct {
	NetworkID   string           `json:"networkId"`
	Lease       CoordinatorLease `json:"lease"`
	Memberships []Membership     `json:"memberships"`
	Warnings    []string         `json:"warnings,omitempty"`
}

type NodeStatusResponse struct {
	NetworkID           string           `json:"networkId"`
	Membership          Membership       `json:"membership"`
	Memberships         []Membership     `json:"memberships,omitempty"`
	Lease               CoordinatorLease `json:"lease"`
	Warnings            []string         `json:"warnings,omitempty"`
	CloudReachable      bool             `json:"cloudReachable"`
	CurrentGitHub       GitHubIdentity   `json:"currentGithub"`
	IdentityDrift       bool             `json:"identityDrift"`
	IdentityDriftReason string           `json:"identityDriftReason,omitempty"`
}

func TargetLabelForNode(nodeName string) string {
	return "looper:target:" + strings.TrimSpace(nodeName)
}

func ValidateNodeName(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("node name is required")
	}
	if trimmed != value {
		return fmt.Errorf("node name %q must not include leading or trailing whitespace", value)
	}
	if !nodeNamePattern.MatchString(trimmed) {
		return fmt.Errorf("node name %q must match %s", value, nodeNamePattern.String())
	}
	return nil
}

func ValidateCompatibility(protocolVersion, daemonVersion, minDaemonVersion string) error {
	if strings.TrimSpace(protocolVersion) != CurrentVersion {
		return fmt.Errorf("unsupported protocol version %q; expected %q", protocolVersion, CurrentVersion)
	}
	if strings.TrimSpace(daemonVersion) == "" {
		return fmt.Errorf("%s is required", MinimumDaemonField)
	}
	if strings.TrimSpace(minDaemonVersion) == "" {
		return nil
	}
	cmp, err := compareSemver(daemonVersion, minDaemonVersion)
	if err != nil {
		return fmt.Errorf("invalid daemon version %q: %w", daemonVersion, err)
	}
	if cmp < 0 {
		return fmt.Errorf("unsupported daemon version %q; minimum supported version is %q", daemonVersion, minDaemonVersion)
	}
	return nil
}

func compareSemver(current string, minimum string) (int, error) {
	c, err := parseSemver(current)
	if err != nil {
		return 0, err
	}
	m, err := parseSemver(minimum)
	if err != nil {
		return 0, err
	}
	if c[0] != m[0] {
		if c[0] < m[0] {
			return -1, nil
		}
		return 1, nil
	}
	if c[1] != m[1] {
		if c[1] < m[1] {
			return -1, nil
		}
		return 1, nil
	}
	if c[2] != m[2] {
		if c[2] < m[2] {
			return -1, nil
		}
		return 1, nil
	}
	return 0, nil
}

func parseSemver(value string) ([3]int, error) {
	var out [3]int
	trimmed := strings.TrimSpace(strings.TrimPrefix(value, "v"))
	if trimmed == "" {
		return out, fmt.Errorf("empty version")
	}
	if idx := strings.Index(trimmed, "+"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if idx := strings.Index(trimmed, "-"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("invalid semver %q", value)
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, fmt.Errorf("invalid semver %q", value)
		}
		out[i] = n
	}
	return out, nil
}

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
	TargetLabelPrefix  = "looper:target:"
)

var nodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

type GitHubIdentity struct {
	NumericID int64  `json:"numericId"`
	Login     string `json:"login,omitempty"`
}

type ReviewerProjectCapability struct {
	ProjectID            string   `json:"projectId"`
	IncludeDrafts        bool     `json:"includeDrafts"`
	RequireReviewRequest *bool    `json:"requireReviewRequest,omitempty"`
	EnableSelfReview     bool     `json:"enableSelfReview"`
	Labels               []string `json:"labels,omitempty"`
	LabelMode            string   `json:"labelMode,omitempty"`
}

type NodeCapabilities struct {
	Roles               []string                    `json:"roles,omitempty"`
	CoordinatorEligible bool                        `json:"coordinatorEligible"`
	RoutedProjects      int                         `json:"routedProjects"`
	RoutedProjectIDs    []string                    `json:"routedProjectIds,omitempty"`
	ReviewerProjects    []ReviewerProjectCapability `json:"reviewerProjects,omitempty"`
	LocalProjects       int                         `json:"localProjects"`
	DynamicLoad         int                         `json:"dynamicLoad"`
	IdentityDrift       bool                        `json:"identityDrift"`
	DriftReason         string                      `json:"driftReason,omitempty"`
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

type WebhookHealth struct {
	DeliveriesReceived int        `json:"deliveriesReceived"`
	LastDeliveryAt     *time.Time `json:"lastDeliveryAt,omitempty"`
	LastDeliveryID     string     `json:"lastDeliveryId,omitempty"`
	LastEvent          string     `json:"lastEvent,omitempty"`
	LastRepo           string     `json:"lastRepo,omitempty"`
	EventSubscribers   int        `json:"eventSubscribers"`
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
	Webhook     WebhookHealth    `json:"webhook"`
	Warnings    []string         `json:"warnings,omitempty"`
}

type NodeStatusResponse struct {
	NetworkID           string           `json:"networkId"`
	Membership          Membership       `json:"membership"`
	Memberships         []Membership     `json:"memberships,omitempty"`
	Lease               CoordinatorLease `json:"lease"`
	Webhook             WebhookHealth    `json:"webhook"`
	Warnings            []string         `json:"warnings,omitempty"`
	CloudReachable      bool             `json:"cloudReachable"`
	CurrentGitHub       GitHubIdentity   `json:"currentGithub"`
	IdentityDrift       bool             `json:"identityDrift"`
	IdentityDriftReason string           `json:"identityDriftReason,omitempty"`
}

func TargetLabelForNode(nodeName string) string {
	return TargetLabelPrefix + strings.TrimSpace(nodeName)
}

type ExactTargetPlan struct {
	DesiredLabel string   `json:"desiredLabel,omitempty"`
	Current      []string `json:"current,omitempty"`
	Add          []string `json:"add,omitempty"`
	Remove       []string `json:"remove,omitempty"`
}

func ParseTargetLabel(label string) (string, bool) {
	trimmed := strings.TrimSpace(label)
	if !strings.HasPrefix(trimmed, TargetLabelPrefix) {
		return "", false
	}
	nodeName := strings.TrimSpace(strings.TrimPrefix(trimmed, TargetLabelPrefix))
	if err := ValidateNodeName(nodeName); err != nil {
		return "", false
	}
	return nodeName, true
}

func CollectTargetLabels(labels []string) []string {
	result := make([]string, 0, 1)
	for _, label := range labels {
		if _, ok := ParseTargetLabel(label); ok {
			result = append(result, strings.TrimSpace(label))
		}
	}
	return result
}

func CollectTargetLikeLabels(labels []string) []string {
	result := make([]string, 0, 1)
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if strings.HasPrefix(strings.ToLower(trimmed), strings.ToLower(TargetLabelPrefix)) {
			result = append(result, trimmed)
		}
	}
	return result
}

func PlanExactTarget(labels []string, nodeName string) (ExactTargetPlan, error) {
	if err := ValidateNodeName(nodeName); err != nil {
		return ExactTargetPlan{}, err
	}
	desired := TargetLabelForNode(nodeName)
	plan := ExactTargetPlan{DesiredLabel: desired, Current: CollectTargetLikeLabels(labels)}
	for _, label := range plan.Current {
		if label != desired {
			plan.Remove = append(plan.Remove, label)
		}
	}
	if !containsExact(plan.Current, desired) {
		plan.Add = append(plan.Add, desired)
	}
	return plan, nil
}

func HasExactTarget(labels []string, nodeName string) bool {
	if err := ValidateNodeName(nodeName); err != nil {
		return false
	}
	return containsExact(CollectTargetLabels(labels), TargetLabelForNode(nodeName))
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func ValidateNodeName(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("node name is required")
	}
	if trimmed != value {
		return fmt.Errorf("node name %q must not include leading or trailing whitespace", value)
	}
	if strings.Contains(trimmed, ":") {
		return fmt.Errorf("node name %q must not contain ':'", value)
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

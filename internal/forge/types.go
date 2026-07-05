package forge

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

type ProviderKind = config.ProviderKind

const (
	ProviderKindGitHub  = config.ProviderKindGitHub
	ProviderKindForgejo = config.ProviderKindForgejo
	ProviderKindPlane   = config.ProviderKindPlane
)

type RepositoryRef struct {
	ProviderID string
	Kind       ProviderKind
	BaseURL    string
	Repo       string
}

type ReviewDiscoveryStrategy string
type ReviewPublishStrategy string
type ThreadResolutionStrategy string
type WorkerClaimStrategy string
type WebhookStrategy string

const (
	ReviewDiscoveryReviewRequest ReviewDiscoveryStrategy = "review_request"
	ReviewDiscoveryLabel         ReviewDiscoveryStrategy = "label"

	ReviewPublishNative      ReviewPublishStrategy = "native_review"
	ReviewPublishCommentOnly ReviewPublishStrategy = "comment_only"

	ThreadResolutionNative   ThreadResolutionStrategy = "native"
	ThreadResolutionDisabled ThreadResolutionStrategy = "disabled"

	WorkerClaimAssignSelf  WorkerClaimStrategy = "assign_self"
	WorkerClaimPreAssigned WorkerClaimStrategy = "pre_assigned"

	WebhookNative  WebhookStrategy = "native"
	WebhookPolling WebhookStrategy = "polling"
)

type Capabilities struct {
	Issues         bool
	PullRequests   bool
	Labels         bool
	Assignees      bool
	Comments       bool
	Identity       bool
	Diffs          bool
	NativeReviews  bool
	ReviewRequests bool
	AutoMerge      bool
	Webhooks       bool

	ReviewDiscovery  ReviewDiscoveryStrategy
	ReviewPublish    ReviewPublishStrategy
	ThreadResolution ThreadResolutionStrategy
	WorkerClaim      WorkerClaimStrategy
	Webhook          WebhookStrategy
}

func StaticCapabilities(kind ProviderKind) (Capabilities, bool) {
	switch kind {
	case ProviderKindGitHub:
		return Capabilities{Issues: true, PullRequests: true, Labels: true, Assignees: true, Comments: true, Identity: true, Diffs: true, NativeReviews: true, ReviewRequests: true, AutoMerge: true, Webhooks: true, ReviewDiscovery: ReviewDiscoveryReviewRequest, ReviewPublish: ReviewPublishNative, ThreadResolution: ThreadResolutionNative, WorkerClaim: WorkerClaimAssignSelf, Webhook: WebhookNative}, true
	case ProviderKindForgejo:
		return Capabilities{Issues: true, PullRequests: true, Labels: true, Assignees: true, Comments: true, Identity: true, Diffs: true, NativeReviews: false, ReviewRequests: false, AutoMerge: false, Webhooks: false, ReviewDiscovery: ReviewDiscoveryLabel, ReviewPublish: ReviewPublishCommentOnly, ThreadResolution: ThreadResolutionDisabled, WorkerClaim: WorkerClaimPreAssigned, Webhook: WebhookPolling}, true
	case ProviderKindPlane:
		// Plane is a task-source: it owns issues/labels/comments/assignees but
		// has no pull requests, diffs, or native reviews (those are delegated to
		// the GitHub code repo). Issue discovery is polling by trigger label.
		return Capabilities{Issues: true, PullRequests: false, Labels: true, Assignees: true, Comments: true, Identity: true, Diffs: false, NativeReviews: false, ReviewRequests: false, AutoMerge: false, Webhooks: false, ReviewDiscovery: ReviewDiscoveryLabel, ReviewPublish: ReviewPublishCommentOnly, ThreadResolution: ThreadResolutionDisabled, WorkerClaim: WorkerClaimPreAssigned, Webhook: WebhookPolling}, true
	default:
		return Capabilities{}, false
	}
}

type Provider interface {
	Kind() ProviderKind
	Repository() RepositoryRef
	Capabilities() Capabilities
	CurrentUser(ctx context.Context) (Identity, error)
}

type Identity struct {
	Login string
	ID    int64
}

type Registry struct {
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) (*Registry, error) {
	registry := &Registry{providers: map[string]Provider{}}
	for _, provider := range providers {
		if provider == nil {
			return nil, fmt.Errorf("register forge provider: provider is nil")
		}
		id := strings.TrimSpace(provider.Repository().ProviderID)
		if id == "" {
			return nil, fmt.Errorf("register forge provider: provider id is empty")
		}
		if _, exists := registry.providers[id]; exists {
			return nil, fmt.Errorf("register forge provider %q: duplicate provider id", id)
		}
		registry.providers[id] = provider
	}
	return registry, nil
}

func (registry *Registry) Get(id string) (Provider, bool) {
	if registry == nil {
		return nil, false
	}
	provider, ok := registry.providers[strings.TrimSpace(id)]
	return provider, ok
}

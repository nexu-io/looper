package webhookforward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	defaultQueueCapacity     = 128
	defaultMaxConcurrent     = 4
	defaultDeliveryTTL       = time.Hour
	defaultRetryDelay        = 2 * time.Second
	defaultRecentOutcomeSize = 64
	maxRetries               = 2
)

type Lane string

const (
	LaneReviewer Lane = "reviewer"
	LaneFixer    Lane = "fixer"
)

type DeliveryRequest struct {
	DeliveryID string
	EventType  string
	Payload    []byte
}

type ForwardResult struct {
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	WorkItems int    `json:"workItems"`
}

type Outcome struct {
	At         string   `json:"at"`
	ProjectID  string   `json:"projectId"`
	Repo       string   `json:"repo"`
	ObjectType string   `json:"objectType"`
	Number     int64    `json:"number"`
	Lanes      []string `json:"lanes"`
	Status     string   `json:"status"`
	Attempts   int      `json:"attempts"`
	Error      string   `json:"error,omitempty"`
	EventType  string   `json:"eventType,omitempty"`
	Action     string   `json:"action,omitempty"`
	DeliveryID string   `json:"deliveryId,omitempty"`
}

type Stats struct {
	DeliveriesReceived  int64     `json:"deliveriesReceived"`
	DeliveriesDeduped   int64     `json:"deliveriesDeduped"`
	DeliveriesIgnored   int64     `json:"deliveriesIgnored"`
	DeliveriesAccepted  int64     `json:"deliveriesAccepted"`
	QueueCapacity       int       `json:"queueCapacity"`
	QueueEnqueued       int64     `json:"queueEnqueued"`
	QueueCoalesced      int64     `json:"queueCoalesced"`
	QueueRejected       int64     `json:"queueRejected"`
	ExecutionsStarted   int64     `json:"executionsStarted"`
	ExecutionsRetried   int64     `json:"executionsRetried"`
	ExecutionsSucceeded int64     `json:"executionsSucceeded"`
	ExecutionsFailed    int64     `json:"executionsFailed"`
	InFlight            int       `json:"inFlight"`
	Queued              int       `json:"queued"`
	KnownCoalescedKeys  int       `json:"knownCoalescedKeys"`
	RecentOutcomes      []Outcome `json:"recentOutcomes"`
}

type Forwarder interface {
	Forward(context.Context, DeliveryRequest) (ForwardResult, error)
	Stats() Stats
	Close()
}

type TargetedReviewer interface {
	DiscoverPullRequest(context.Context, reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error)
}

type TargetedFixer interface {
	DiscoverPullRequest(context.Context, fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error)
	DiscoverPullRequestsForBaseBranchUpdate(context.Context, fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error)
}

type Options struct {
	Repos              *storage.Repositories
	Config             config.Config
	Reviewer           TargetedReviewer
	Fixer              TargetedFixer
	Logger             bootstrap.Logger
	Now                func() time.Time
	QueueCapacity      int
	MaxConcurrent      int
	DeliveryTTL        time.Duration
	RetryDelay         time.Duration
	RecentOutcomeLimit int
}

type deliveryRecord struct {
	expiresAt time.Time
}

type workKey struct {
	ProjectID  string
	Repo       string
	ObjectType string
	Number     int64
	Branch     string
}

type workMetadata struct {
	EventType  string
	Action     string
	DeliveryID string
}

type workItem struct {
	key      workKey
	lanes    map[Lane]struct{}
	metadata workMetadata
	running  bool
	enqueued bool
}

type routedDelivery struct {
	repo       string
	objectType string
	branch     string
	numbers    []int64
	action     string
	lanes      map[Lane]struct{}
}

type pushEnvelope struct {
	Ref        string `json:"ref"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type pullRequestEnvelope struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Number int64 `json:"number"`
	} `json:"pull_request"`
}

type issueCommentEnvelope struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue struct {
		Number      int64 `json:"number"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	} `json:"issue"`
}

type pullRequestRef struct {
	Number int64 `json:"number"`
}

type checkRunEnvelope struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	CheckRun struct {
		Conclusion   string           `json:"conclusion"`
		PullRequests []pullRequestRef `json:"pull_requests"`
		CheckSuite   struct {
			PullRequests []pullRequestRef `json:"pull_requests"`
		} `json:"check_suite"`
	} `json:"check_run"`
}

type forwarder struct {
	repos              *storage.Repositories
	cfg                config.Config
	reviewer           TargetedReviewer
	fixer              TargetedFixer
	logger             bootstrap.Logger
	now                func() time.Time
	queueCapacity      int
	maxConcurrent      int
	deliveryTTL        time.Duration
	retryDelay         time.Duration
	recentOutcomeLimit int

	mu              sync.Mutex
	cond            *sync.Cond
	closed          bool
	workersDone     sync.WaitGroup
	queue           []workKey
	works           map[string]*workItem
	deliveries      map[string]deliveryRecord
	stats           Stats
	recentOutcomes  []Outcome
	currentInFlight int
}

func New(options Options) Forwarder {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	queueCapacity := options.QueueCapacity
	if queueCapacity <= 0 {
		queueCapacity = defaultQueueCapacity
	}
	maxConcurrent := options.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	deliveryTTL := options.DeliveryTTL
	if deliveryTTL <= 0 {
		deliveryTTL = defaultDeliveryTTL
	}
	retryDelay := options.RetryDelay
	if retryDelay <= 0 {
		retryDelay = defaultRetryDelay
	}
	recentOutcomeLimit := options.RecentOutcomeLimit
	if recentOutcomeLimit <= 0 {
		recentOutcomeLimit = defaultRecentOutcomeSize
	}
	f := &forwarder{
		repos:              options.Repos,
		cfg:                options.Config,
		reviewer:           options.Reviewer,
		fixer:              options.Fixer,
		logger:             options.Logger,
		now:                now,
		queueCapacity:      queueCapacity,
		maxConcurrent:      maxConcurrent,
		deliveryTTL:        deliveryTTL,
		retryDelay:         retryDelay,
		recentOutcomeLimit: recentOutcomeLimit,
		works:              map[string]*workItem{},
		deliveries:         map[string]deliveryRecord{},
	}
	f.cond = sync.NewCond(&f.mu)
	for i := 0; i < f.maxConcurrent; i++ {
		f.workersDone.Add(1)
		go f.worker()
	}
	return f
}

func (f *forwarder) Forward(ctx context.Context, request DeliveryRequest) (ForwardResult, error) {
	if f.repos == nil || f.repos.Projects == nil {
		return ForwardResult{}, fmt.Errorf("webhook forwarder repositories are not configured")
	}
	now := f.now().UTC()
	deliveryID := strings.TrimSpace(request.DeliveryID)
	eventType := strings.TrimSpace(request.EventType)
	if deliveryID == "" {
		return ForwardResult{}, fmt.Errorf("x-github-delivery header is required")
	}
	if eventType == "" {
		return ForwardResult{}, fmt.Errorf("x-github-event header is required")
	}
	routed, ok, err := routeDelivery(eventType, request.Payload)
	if err != nil {
		return ForwardResult{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ForwardResult{}, fmt.Errorf("webhook forwarder is closed")
	}
	f.stats.DeliveriesReceived++
	f.pruneExpiredDeliveriesLocked(now)
	if _, exists := f.deliveries[deliveryID]; exists {
		f.stats.DeliveriesDeduped++
		return ForwardResult{Status: "duplicate", Reason: "delivery_deduped", WorkItems: 0}, nil
	}
	if !ok {
		f.deliveries[deliveryID] = deliveryRecord{expiresAt: now.Add(f.deliveryTTL)}
		f.stats.DeliveriesIgnored++
		return ForwardResult{Status: "ignored", Reason: "unsupported_event", WorkItems: 0}, nil
	}
	projects, projectErr := f.repos.Projects.List(ctx)
	if projectErr != nil {
		return ForwardResult{}, projectErr
	}
	workItems, enqueueErr := f.enqueueLocked(projects, routed, workMetadata{EventType: eventType, Action: routed.action, DeliveryID: deliveryID})
	if enqueueErr != nil {
		return ForwardResult{}, enqueueErr
	}
	f.deliveries[deliveryID] = deliveryRecord{expiresAt: now.Add(f.deliveryTTL)}
	if workItems == 0 {
		f.stats.DeliveriesIgnored++
		return ForwardResult{Status: "ignored", Reason: "no_matching_projects", WorkItems: 0}, nil
	}
	f.stats.DeliveriesAccepted++
	return ForwardResult{Status: "accepted", WorkItems: workItems}, nil
}

func (f *forwarder) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	stats := f.stats
	stats.QueueCapacity = f.queueCapacity
	stats.InFlight = f.currentInFlight
	stats.Queued = len(f.queue)
	stats.KnownCoalescedKeys = len(f.works)
	stats.RecentOutcomes = append([]Outcome(nil), f.recentOutcomes...)
	return stats
}

func (f *forwarder) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.cond.Broadcast()
	f.mu.Unlock()
	f.workersDone.Wait()
}

func (f *forwarder) enqueueLocked(projects []storage.ProjectRecord, routed routedDelivery, metadata workMetadata) (int, error) {
	type candidate struct {
		key   workKey
		lanes map[Lane]struct{}
	}
	candidates := make([]candidate, 0, len(projects))
	newQueueEntries := 0
	matched := 0
	for _, project := range projects {
		if project.Archived {
			continue
		}
		repo := repoFromProjectMetadata(project.MetadataJSON)
		if !strings.EqualFold(repo, routed.repo) {
			continue
		}
		lanes := enabledLanesForProject(f.cfg, project.ID, routed.lanes)
		if len(lanes) == 0 {
			continue
		}
		if routed.objectType == "base_branch" {
			matched++
			key := workKey{ProjectID: project.ID, Repo: repo, ObjectType: routed.objectType, Branch: routed.branch}
			candidates = append(candidates, candidate{key: key, lanes: lanes})
			itemKey := workKeyString(key)
			item, exists := f.works[itemKey]
			if exists && (item.running || item.enqueued) {
				continue
			}
			newQueueEntries++
			continue
		}
		for _, number := range routed.numbers {
			if number <= 0 {
				continue
			}
			matched++
			key := workKey{ProjectID: project.ID, Repo: repo, ObjectType: routed.objectType, Number: number}
			candidates = append(candidates, candidate{key: key, lanes: lanes})
			itemKey := workKeyString(key)
			item, exists := f.works[itemKey]
			if exists && (item.running || item.enqueued) {
				continue
			}
			newQueueEntries++
		}
	}
	if newQueueEntries > f.queueCapacity-len(f.queue) {
		f.stats.QueueRejected++
		return 0, fmt.Errorf("webhook forward queue is full")
	}
	for _, candidate := range candidates {
		itemKey := workKeyString(candidate.key)
		item, exists := f.works[itemKey]
		if !exists {
			item = &workItem{key: candidate.key, lanes: map[Lane]struct{}{}, metadata: metadata}
			f.works[itemKey] = item
		}
		for lane := range candidate.lanes {
			item.lanes[lane] = struct{}{}
		}
		item.metadata = metadata
		if item.running || item.enqueued {
			f.stats.QueueCoalesced++
			continue
		}
		item.enqueued = true
		f.queue = append(f.queue, candidate.key)
		f.stats.QueueEnqueued++
		f.cond.Signal()
	}
	return matched, nil
}

func (f *forwarder) worker() {
	defer f.workersDone.Done()
	for {
		key, item, ok := f.nextWork()
		if !ok {
			return
		}
		outcome := f.executeWithRetry(context.Background(), key, item)
		f.finishWork(key, outcome)
	}
}

func (f *forwarder) nextWork() (workKey, workItem, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueuePendingLocked()
	for len(f.queue) == 0 && !f.closed {
		f.cond.Wait()
		f.enqueuePendingLocked()
	}
	if f.closed && len(f.queue) == 0 {
		return workKey{}, workItem{}, false
	}
	key := f.queue[0]
	f.queue = f.queue[1:]
	f.enqueuePendingLocked()
	itemKey := workKeyString(key)
	item := f.works[itemKey]
	if item == nil {
		return workKey{}, workItem{}, true
	}
	lanes := copyLanes(item.lanes)
	metadata := item.metadata
	item.lanes = map[Lane]struct{}{}
	item.running = true
	item.enqueued = false
	f.currentInFlight++
	f.stats.ExecutionsStarted++
	return key, workItem{key: key, lanes: lanes, metadata: metadata}, true
}

func (f *forwarder) finishWork(key workKey, outcome Outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.currentInFlight > 0 {
		f.currentInFlight--
	}
	itemKey := workKeyString(key)
	item := f.works[itemKey]
	if item != nil {
		item.running = false
		if len(item.lanes) == 0 {
			delete(f.works, itemKey)
		} else if !item.enqueued {
			if len(f.queue) < f.queueCapacity {
				item.enqueued = true
				f.queue = append(f.queue, key)
				f.stats.QueueEnqueued++
				f.cond.Signal()
			}
		}
	}
	switch outcome.Status {
	case "succeeded":
		f.stats.ExecutionsSucceeded++
	case "failed":
		f.stats.ExecutionsFailed++
	}
	f.appendOutcomeLocked(outcome)
	if f.logger != nil {
		fields := map[string]any{"projectId": outcome.ProjectID, "repo": outcome.Repo, "objectType": outcome.ObjectType, "number": outcome.Number, "lanes": outcome.Lanes, "status": outcome.Status, "attempts": outcome.Attempts}
		if outcome.Error != "" {
			fields["error"] = outcome.Error
		}
		f.logger.Info("webhook targeted discovery completed", fields)
	}
}

func (f *forwarder) executeWithRetry(ctx context.Context, key workKey, item workItem) Outcome {
	lanes := sortedLaneStrings(item.lanes)
	outcome := Outcome{At: f.now().UTC().Format(time.RFC3339Nano), ProjectID: key.ProjectID, Repo: key.Repo, ObjectType: key.ObjectType, Number: key.Number, Lanes: lanes, EventType: item.metadata.EventType, Action: item.metadata.Action, DeliveryID: item.metadata.DeliveryID}
	for attempt := 1; attempt <= maxRetries; attempt++ {
		outcome.Attempts = attempt
		err := f.executeOnce(ctx, key, item)
		if err == nil {
			outcome.Status = "succeeded"
			return outcome
		}
		if attempt < maxRetries && isTransient(err) {
			f.mu.Lock()
			f.stats.ExecutionsRetried++
			f.mu.Unlock()
			select {
			case <-ctx.Done():
				outcome.Status = "failed"
				outcome.Error = ctx.Err().Error()
				return outcome
			case <-time.After(f.retryDelay):
			}
			continue
		}
		outcome.Status = "failed"
		outcome.Error = err.Error()
		return outcome
	}
	outcome.Status = "failed"
	outcome.Error = "targeted discovery exhausted retries"
	return outcome
}

func (f *forwarder) executeOnce(ctx context.Context, key workKey, item workItem) error {
	if _, ok := item.lanes[LaneReviewer]; ok {
		if f.reviewer == nil {
			return fmt.Errorf("reviewer targeted discovery is not configured")
		}
		if _, err := f.reviewer.DiscoverPullRequest(ctx, reviewer.TargetedDiscoveryInput{ProjectID: key.ProjectID, Repo: key.Repo, PRNumber: key.Number}); err != nil {
			return err
		}
	}
	if _, ok := item.lanes[LaneFixer]; ok {
		if f.fixer == nil {
			return fmt.Errorf("fixer targeted discovery is not configured")
		}
		if key.ObjectType == "base_branch" {
			_, err := f.fixer.DiscoverPullRequestsForBaseBranchUpdate(ctx, fixer.BaseBranchDiscoveryInput{ProjectID: key.ProjectID, Repo: key.Repo, BaseRefName: key.Branch})
			return err
		}
		if _, err := f.fixer.DiscoverPullRequest(ctx, fixer.TargetedDiscoveryInput{ProjectID: key.ProjectID, Repo: key.Repo, PRNumber: key.Number}); err != nil {
			return err
		}
	}
	return nil
}

func (f *forwarder) pruneExpiredDeliveriesLocked(now time.Time) {
	for key, record := range f.deliveries {
		if !record.expiresAt.After(now) {
			delete(f.deliveries, key)
		}
	}
}

func (f *forwarder) appendOutcomeLocked(outcome Outcome) {
	f.recentOutcomes = append(f.recentOutcomes, outcome)
	if len(f.recentOutcomes) > f.recentOutcomeLimit {
		f.recentOutcomes = append([]Outcome(nil), f.recentOutcomes[len(f.recentOutcomes)-f.recentOutcomeLimit:]...)
	}
}

func (f *forwarder) enqueuePendingLocked() {
	if len(f.queue) >= f.queueCapacity {
		return
	}
	added := 0
	for _, item := range f.works {
		if item == nil || item.running || item.enqueued || len(item.lanes) == 0 {
			continue
		}
		item.enqueued = true
		f.queue = append(f.queue, item.key)
		f.stats.QueueEnqueued++
		added++
		if len(f.queue) >= f.queueCapacity {
			break
		}
	}
	if added > 0 {
		f.cond.Signal()
	}
}

func routeDelivery(eventType string, payload []byte) (routedDelivery, bool, error) {
	switch strings.TrimSpace(eventType) {
	case "pull_request":
		var envelope pullRequestEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return routedDelivery{}, false, fmt.Errorf("decode pull_request webhook: %w", err)
		}
		lanes := map[Lane]struct{}{}
		switch strings.TrimSpace(envelope.Action) {
		case "review_requested":
			lanes[LaneReviewer] = struct{}{}
		case "opened", "reopened", "ready_for_review", "synchronize":
			lanes[LaneReviewer] = struct{}{}
			lanes[LaneFixer] = struct{}{}
		default:
			return routedDelivery{}, false, nil
		}
		if strings.TrimSpace(envelope.Repository.FullName) == "" || envelope.PullRequest.Number <= 0 {
			return routedDelivery{}, false, errors.New("pull_request webhook missing repository or number")
		}
		return routedDelivery{repo: strings.TrimSpace(envelope.Repository.FullName), objectType: "pull_request", numbers: []int64{envelope.PullRequest.Number}, action: strings.TrimSpace(envelope.Action), lanes: lanes}, true, nil
	case "issue_comment":
		var envelope issueCommentEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return routedDelivery{}, false, fmt.Errorf("decode issue_comment webhook: %w", err)
		}
		if envelope.Issue.PullRequest == nil {
			return routedDelivery{}, false, nil
		}
		if strings.TrimSpace(envelope.Repository.FullName) == "" || envelope.Issue.Number <= 0 {
			return routedDelivery{}, false, errors.New("issue_comment webhook missing repository or number")
		}
		return routedDelivery{repo: strings.TrimSpace(envelope.Repository.FullName), objectType: "pull_request", numbers: []int64{envelope.Issue.Number}, action: strings.TrimSpace(envelope.Action), lanes: map[Lane]struct{}{LaneFixer: {}}}, true, nil
	case "pull_request_review", "pull_request_review_comment":
		var envelope pullRequestEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return routedDelivery{}, false, fmt.Errorf("decode %s webhook: %w", eventType, err)
		}
		if strings.TrimSpace(envelope.Repository.FullName) == "" || envelope.PullRequest.Number <= 0 {
			return routedDelivery{}, false, fmt.Errorf("%s webhook missing repository or number", eventType)
		}
		return routedDelivery{repo: strings.TrimSpace(envelope.Repository.FullName), objectType: "pull_request", numbers: []int64{envelope.PullRequest.Number}, action: strings.TrimSpace(envelope.Action), lanes: map[Lane]struct{}{LaneFixer: {}}}, true, nil
	case "push":
		var envelope pushEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return routedDelivery{}, false, fmt.Errorf("decode push webhook: %w", err)
		}
		if envelope.Deleted {
			return routedDelivery{}, false, nil
		}
		if strings.TrimSpace(envelope.Repository.FullName) == "" {
			return routedDelivery{}, false, errors.New("push webhook missing repository")
		}
		branch := strings.TrimPrefix(strings.TrimSpace(envelope.Ref), "refs/heads/")
		if branch == "" || branch == strings.TrimSpace(envelope.Ref) {
			return routedDelivery{}, false, nil
		}
		return routedDelivery{repo: strings.TrimSpace(envelope.Repository.FullName), objectType: "base_branch", branch: branch, action: "push", lanes: map[Lane]struct{}{LaneFixer: {}}}, true, nil
	case "check_run":
		var envelope checkRunEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return routedDelivery{}, false, fmt.Errorf("decode check_run webhook: %w", err)
		}
		if strings.TrimSpace(envelope.Action) != "completed" || !isFailingCheckConclusion(envelope.CheckRun.Conclusion) {
			return routedDelivery{}, false, nil
		}
		numbers := pullRequestNumbers(envelope.CheckRun.PullRequests)
		if len(numbers) == 0 {
			numbers = pullRequestNumbers(envelope.CheckRun.CheckSuite.PullRequests)
		}
		if strings.TrimSpace(envelope.Repository.FullName) == "" {
			return routedDelivery{}, false, errors.New("check_run webhook missing repository")
		}
		if len(numbers) == 0 {
			return routedDelivery{}, false, nil
		}
		return routedDelivery{repo: strings.TrimSpace(envelope.Repository.FullName), objectType: "pull_request", numbers: numbers, action: strings.TrimSpace(envelope.Action), lanes: map[Lane]struct{}{LaneFixer: {}}}, true, nil
	default:
		return routedDelivery{}, false, nil
	}
}

func pullRequestNumbers(items []pullRequestRef) []int64 {
	seen := make(map[int64]struct{}, len(items))
	numbers := make([]int64, 0, len(items))
	for _, item := range items {
		if item.Number <= 0 {
			continue
		}
		if _, ok := seen[item.Number]; ok {
			continue
		}
		seen[item.Number] = struct{}{}
		numbers = append(numbers, item.Number)
	}
	return numbers
}

func isFailingCheckConclusion(conclusion string) bool {
	switch strings.ToUpper(strings.TrimSpace(conclusion)) {
	case "FAILURE", "FAILED", "ERROR", "TIMED_OUT", "ACTION_REQUIRED":
		return true
	default:
		return false
	}
}

func enabledLanesForProject(cfg config.Config, projectID string, lanes map[Lane]struct{}) map[Lane]struct{} {
	roles := config.ProjectRoleConfigs(cfg, projectID)
	result := map[Lane]struct{}{}
	if _, ok := lanes[LaneReviewer]; ok && roles.Reviewer.Discovery.AutoDiscovery {
		result[LaneReviewer] = struct{}{}
	}
	if _, ok := lanes[LaneFixer]; ok && roles.Fixer.AutoDiscovery {
		result[LaneFixer] = struct{}{}
	}
	return result
}

func repoFromProjectMetadata(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &payload); err != nil {
		return ""
	}
	value, _ := payload["repo"].(string)
	return strings.TrimSpace(value)
}

func workKeyString(key workKey) string {
	return fmt.Sprintf("%s|%s|%s|%d|%s", key.ProjectID, strings.ToLower(key.Repo), key.ObjectType, key.Number, key.Branch)
}

func copyLanes(lanes map[Lane]struct{}) map[Lane]struct{} {
	result := make(map[Lane]struct{}, len(lanes))
	for lane := range lanes {
		result[lane] = struct{}{}
	}
	return result
}

func sortedLaneStrings(lanes map[Lane]struct{}) []string {
	items := make([]string, 0, len(lanes))
	for lane := range lanes {
		items = append(items, string(lane))
	}
	sort.Strings(items)
	return items
}

func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type temporary interface{ Temporary() bool }
	var temp temporary
	if errors.As(err, &temp) && temp.Temporary() {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timeout") || strings.Contains(message, "tempor") || strings.Contains(message, "rate limit") || strings.Contains(message, "retry after")
}

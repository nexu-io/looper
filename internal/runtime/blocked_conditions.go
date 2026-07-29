package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/loops"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	loopengine "github.com/nexu-io/looper/internal/loops/engine"
	"github.com/nexu-io/looper/internal/storage"
)

type blockedConditionReconciler func(context.Context, storage.LoopRecord, loopcondition.Record) (bool, error)

func (r *Runtime) reconcileBlockedConditions(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	gateway := r.githubGateway
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil || repositories.Queue == nil || repositories.Locks == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	registry := r.blockedConditionRegistry(&cfg, repositories, gateway)
	blocked, err := repositories.Loops.ListByStatuses(ctx, []string{"paused", "awaiting_human", "shepherding"})
	if err != nil {
		return
	}
	for _, loop := range blocked {
		lease := loopengine.StorageLease{Locks: repositories.Locks, Key: "lifecycle:" + loop.ID, Owner: "blocked-condition-reconciler", TTL: time.Minute, Now: now}
		acquired, leaseErr := lease.Acquire(ctx)
		if leaseErr != nil || !acquired {
			continue
		}
		func() {
			defer func() { _ = lease.Release(context.Background()) }()
			record, inferred := effectiveBlockedCondition(loop)
			if !record.Valid() {
				return
			}
			check := registry[record.Kind]
			if check == nil {
				return
			}
			ready, checkErr := check(ctx, loop, record)
			if checkErr != nil {
				if logger != nil {
					logger.Warn("blocked condition reconcile failed", map[string]any{"loopId": loop.ID, "condition": record.Kind, "error": checkErr.Error()})
				}
				return
			}
			if !ready {
				if inferred {
					r.persistInferredBlockedCondition(ctx, repositories, loop, record, now)
				}
				return
			}
			if err := resumeBlockedLoop(ctx, repositories, loop, record, now); err != nil {
				if logger != nil {
					logger.Warn("blocked condition cleared but loop requeue failed", map[string]any{"loopId": loop.ID, "condition": record.Kind, "error": err.Error()})
				}
				return
			}
			if logger != nil {
				logger.Info("resumed loop after blocked condition cleared", map[string]any{"loopId": loop.ID, "condition": record.Kind})
			}
		}()
	}
}

func (r *Runtime) blockedConditionRegistry(cfg *config.Config, repositories *storage.Repositories, gateway *githubinfra.Gateway) map[loopcondition.Kind]blockedConditionReconciler {
	return map[loopcondition.Kind]blockedConditionReconciler{
		loopcondition.ProductSpec: func(ctx context.Context, loop storage.LoopRecord, condition loopcondition.Record) (bool, error) {
			planeGateway, planeProjectID, ok := planeDocForProject(cfg, loop.ProjectID)
			if !ok || planeGateway == nil {
				return false, nil
			}
			issueURL, hasSpecPR := loopIssueURLAndSpecPR(loop.MetadataJSON)
			if issueURL == "" {
				var err error
				issueURL, err = latestRunIssueURL(ctx, repositories, loop.ID)
				if err != nil {
					return false, err
				}
			}
			workItemID := planedoc.WorkItemIDFromURL(issueURL)
			if workItemID == "" {
				return false, nil
			}
			productOwnerPlaneID := ""
			if cfg != nil {
				productOwnerPlaneID = strings.TrimSpace(config.ProjectProductOwner(*cfg, loop.ProjectID).PlaneID)
			}
			verified, err := verifiedProductSpec(ctx, planeGateway, planeProjectID, workItemID, productOwnerPlaneID, loop.MetadataJSON)
			if err != nil || verified {
				return shouldResumeForProductSpec(loop.Type, loop.Status, hasSpecPR, verified), err
			}
			associated, confirmation, err := associateProductSpecReply(ctx, planeGateway, planeProjectID, workItemID, loop, condition, productOwnerPlaneID)
			if err == nil && associated {
				err = recordProductSpecConfirmation(ctx, repositories, loop.ID, confirmation)
			}
			return shouldResumeForProductSpec(loop.Type, loop.Status, hasSpecPR, associated), err
		},
		loopcondition.DiskRecovered: func(_ context.Context, _ storage.LoopRecord, _ loopcondition.Record) (bool, error) {
			return diskConditionCleared(cfg)
		},
		loopcondition.CISettled: func(ctx context.Context, loop storage.LoopRecord, _ loopcondition.Record) (bool, error) {
			pr, err := pullRequestForBlockedCondition(ctx, repositories, gateway, loop)
			return err == nil && shepherdCIPhase(pr) != "pending", err
		},
		loopcondition.ReviewUpdated: func(ctx context.Context, loop storage.LoopRecord, condition loopcondition.Record) (bool, error) {
			pr, err := pullRequestForBlockedCondition(ctx, repositories, gateway, loop)
			if err != nil {
				return false, err
			}
			return condition.Fingerprint != "" && foldShepherdSignal(pr) != condition.Fingerprint, nil
		},
		loopcondition.HumanAnswered: func(ctx context.Context, loop storage.LoopRecord, _ loopcondition.Record) (bool, error) {
			ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
			if !ok {
				return false, nil
			}
			if strings.EqualFold(strings.TrimSpace(ask.Status), "answered") && strings.TrimSpace(ask.Answer) != "" {
				return true, nil
			}
			if !strings.EqualFold(ask.Transport, "plane") || !strings.EqualFold(ask.Status, "awaiting") {
				return false, nil
			}
			planeGateway, planeProjectID, configured := planeDocForProject(cfg, loop.ProjectID)
			if !configured || planeGateway == nil {
				return false, nil
			}
			askedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(ask.AskedAt))
			if err != nil {
				return false, fmt.Errorf("invalid Plane HITL askedAt %q: %w", ask.AskedAt, err)
			}
			sourceURL := strings.SplitN(strings.TrimSpace(ask.ActionURL), "#", 2)[0]
			if workItemID := planedoc.WorkItemIDFromURL(sourceURL); workItemID != "" {
				comments, listErr := planeGateway.ListWorkItemComments(ctx, planeProjectID, workItemID)
				if listErr != nil {
					return false, listErr
				}
				productOwnerPlaneID := ""
				if cfg != nil {
					productOwnerPlaneID = strings.TrimSpace(config.ProjectProductOwner(*cfg, loop.ProjectID).PlaneID)
				}
				answer, answeredAt, found := collectWorkItemDecisionAnswer(comments, askedAt, productOwnerPlaneID)
				if !found {
					return false, nil
				}
				if err := recordPlaneHITLAnswer(ctx, repositories, loop.ID, answer, answeredAt); err != nil {
					return false, err
				}
				return true, nil
			}
			pageURL := sourceURL
			comments, err := planeGateway.ListHumanSpecComments(ctx, planeProjectID, pageURL)
			if err != nil {
				return false, err
			}
			var answer *planedoc.PageComment
			for i := range comments {
				createdAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(comments[i].CreatedAt))
				if parseErr != nil || !createdAt.After(askedAt) {
					continue
				}
				if answer == nil {
					answer = &comments[i]
					continue
				}
				currentAt, _ := time.Parse(time.RFC3339Nano, answer.CreatedAt)
				if createdAt.Before(currentAt) {
					answer = &comments[i]
				}
			}
			if answer == nil {
				return false, nil
			}
			text := strings.TrimSpace(answer.CommentStripped)
			if text == "" {
				text = strings.TrimSpace(answer.CommentHTML)
			}
			if err := recordPlaneHITLAnswer(ctx, repositories, loop.ID, text, answer.CreatedAt); err != nil {
				return false, err
			}
			return true, nil
		},
		loopcondition.InfraRecovered: func(ctx context.Context, loop storage.LoopRecord, condition loopcondition.Record) (bool, error) {
			projectID := loop.ProjectID
			return recoverableInfraConditionCleared(ctx, cfg, repositories, &projectID, condition.Fingerprint)
		},
	}
}

func verifiedProductSpec(ctx context.Context, gateway *planedoc.Gateway, planeProjectID, workItemID, productOwnerPlaneID string, metadataJSON *string) (bool, error) {
	productOwnerPlaneID = strings.TrimSpace(productOwnerPlaneID)
	if productOwnerPlaneID == "" {
		return false, nil
	}
	url, found, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.ProductSpecLinkTitle)
	if err != nil || !found {
		return false, err
	}
	if loops.ProductSpecConfirmedBy(metadataJSON, url, productOwnerPlaneID) {
		return true, nil
	}
	pageID := planedoc.PageIDFromURL(url)
	if pageID == "" {
		return false, nil
	}
	page, err := gateway.PageDocument(ctx, planeProjectID, pageID)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(page.ContentHTML) != "" && page.AuthoredBy(productOwnerPlaneID), nil
}

func collectWorkItemDecisionAnswer(comments []planedoc.WorkItemComment, askedAt time.Time, productOwnerPlaneID string) (string, string, bool) {
	productOwnerPlaneID = strings.TrimSpace(productOwnerPlaneID)
	type reply struct {
		text      string
		createdAt time.Time
		rawTime   string
	}
	replies := make([]reply, 0, len(comments))
	for i := range comments {
		text := firstNonEmpty(strings.TrimSpace(comments[i].CommentStripped), strings.TrimSpace(comments[i].CommentHTML))
		createdAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(comments[i].CreatedAt))
		if err != nil || !createdAt.After(askedAt) || strings.Contains(text, planedoc.LooperSignatureMark) || strings.Contains(text, planedoc.LooperCommentMarker) {
			continue
		}
		if productOwnerPlaneID != "" && strings.TrimSpace(comments[i].Actor) != productOwnerPlaneID {
			continue
		}
		replies = append(replies, reply{text: text, createdAt: createdAt, rawTime: comments[i].CreatedAt})
	}
	if len(replies) == 0 {
		return "", "", false
	}
	sort.Slice(replies, func(i, j int) bool { return replies[i].createdAt.Before(replies[j].createdAt) })
	parts := make([]string, 0, len(replies))
	for _, item := range replies {
		parts = append(parts, item.text)
	}
	latest := replies[len(replies)-1]
	return strings.Join(parts, "\n\n"), strings.TrimSpace(latest.rawTime), true
}

func latestRunIssueURL(ctx context.Context, repositories *storage.Repositories, loopID string) (string, error) {
	if repositories == nil || repositories.Runs == nil {
		return "", nil
	}
	run, err := repositories.Runs.GetLatestByLoopID(ctx, loopID)
	if err != nil || run == nil || run.CheckpointJSON == nil {
		return "", err
	}
	var checkpoint struct {
		Issue *struct {
			URL string `json:"url"`
		} `json:"issue"`
	}
	if err := json.Unmarshal([]byte(*run.CheckpointJSON), &checkpoint); err != nil {
		return "", fmt.Errorf("decode latest product-spec checkpoint: %w", err)
	}
	if checkpoint.Issue == nil {
		return "", nil
	}
	return strings.TrimSpace(checkpoint.Issue.URL), nil
}

func associateProductSpecReply(ctx context.Context, gateway *planedoc.Gateway, planeProjectID, workItemID string, loop storage.LoopRecord, condition loopcondition.Record, productOwnerPlaneID string) (bool, loops.ProductSpecConfirmation, error) {
	productOwnerPlaneID = strings.TrimSpace(productOwnerPlaneID)
	if productOwnerPlaneID == "" {
		return false, loops.ProductSpecConfirmation{}, nil
	}
	since, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(condition.Since))
	if err != nil {
		return false, loops.ProductSpecConfirmation{}, nil
	}
	comments, err := gateway.ListWorkItemComments(ctx, planeProjectID, workItemID)
	if err != nil {
		return false, loops.ProductSpecConfirmation{}, err
	}
	var reply *planedoc.WorkItemComment
	for index := range comments {
		comment := &comments[index]
		if comment.ID == condition.Fingerprint {
			continue
		}
		if strings.TrimSpace(comment.Actor) != productOwnerPlaneID {
			continue
		}
		createdAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(comment.CreatedAt))
		if parseErr != nil || !createdAt.After(since) {
			continue
		}
		url, inlineText := planedoc.DroppedSpecContent(*comment)
		if url == "" && inlineText == "" {
			continue
		}
		if reply == nil {
			reply = comment
			continue
		}
		currentAt, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(reply.CreatedAt))
		if createdAt.Before(currentAt) {
			reply = comment
		}
	}
	if reply == nil {
		return false, loops.ProductSpecConfirmation{}, nil
	}
	url, inlineText := planedoc.DroppedSpecContent(*reply)
	issueNumber, _ := parseIssueNumberFromTargetID(derefString(loop.TargetID))
	pageName := "Product spec"
	if issueNumber > 0 {
		pageName = fmt.Sprintf("Product spec #%d", issueNumber)
	}
	associatedURL, err := gateway.AssociateDroppedSpec(ctx, planeProjectID, workItemID, planedoc.SpecKindProduct, url, inlineText, pageName)
	if err != nil {
		return false, loops.ProductSpecConfirmation{}, err
	}
	return true, loops.ProductSpecConfirmation{URL: associatedURL, PlaneActorID: productOwnerPlaneID, ConfirmedAt: reply.CreatedAt}, nil
}

func recordProductSpecConfirmation(ctx context.Context, repositories *storage.Repositories, loopID string, confirmation loops.ProductSpecConfirmation) error {
	if repositories == nil || repositories.Loops == nil {
		return fmt.Errorf("record product spec confirmation: loop repository unavailable")
	}
	current, err := repositories.Loops.GetByID(ctx, loopID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("record product spec confirmation: loop disappeared: %s", loopID)
	}
	metadata, err := loops.WriteProductSpecConfirmation(current.MetadataJSON, confirmation)
	if err != nil {
		return err
	}
	current.MetadataJSON = stringPtr(metadata)
	current.UpdatedAt = firstNonEmpty(strings.TrimSpace(confirmation.ConfirmedAt), current.UpdatedAt)
	return repositories.Loops.Upsert(ctx, *current)
}

func recordPlaneHITLAnswer(ctx context.Context, repositories *storage.Repositories, loopID, answer, answeredAt string) error {
	current, err := repositories.Loops.GetByID(ctx, loopID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("Plane HITL loop disappeared: %s", loopID)
	}
	ask, ok := loops.ReadHITLAsk(current.MetadataJSON)
	if !ok || ask.Transport != "plane" || ask.Status != "awaiting" {
		return nil
	}
	ask.Answer = strings.TrimSpace(answer)
	ask.AnsweredAt = strings.TrimSpace(answeredAt)
	ask.Status = "answered"
	metadata, err := loops.WriteHITLAsk(current.MetadataJSON, ask)
	if err != nil {
		return err
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(metadata), &object); err != nil {
		return err
	}
	object["awaitingProductAnswer"] = false
	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	current.MetadataJSON = stringPtr(string(encoded))
	current.UpdatedAt = strings.TrimSpace(answeredAt)
	return repositories.Loops.Upsert(ctx, *current)
}

func effectiveBlockedCondition(loop storage.LoopRecord) (loopcondition.Record, bool) {
	if record, ok := loopcondition.Read(loop.MetadataJSON); ok {
		return record, false
	}
	if loop.Status == "paused" && loop.Type == "planner" && metadataBool(loop.MetadataJSON, "awaitingProductSpec") {
		return loopcondition.Record{Kind: loopcondition.ProductSpec}, true
	}
	if loop.Status == "awaiting_human" {
		if _, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok {
			return loopcondition.Record{Kind: loopcondition.HumanAnswered}, true
		}
	}
	return loopcondition.Record{}, false
}

func metadataBool(metadataJSON *string, key string) bool {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return false
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(*metadataJSON), &metadata) != nil {
		return false
	}
	value, _ := metadata[key].(bool)
	return value
}

func (r *Runtime) persistInferredBlockedCondition(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, record loopcondition.Record, now func() time.Time) {
	record.Since = formatJavaScriptISOString(now().UTC())
	metadata, err := loopcondition.Set(loop.MetadataJSON, record)
	if err != nil {
		return
	}
	loop.MetadataJSON = &metadata
	loop.UpdatedAt = record.Since
	_ = repositories.Loops.Upsert(ctx, loop)
}

func resumeBlockedLoop(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, record loopcondition.Record, now func() time.Time) error {
	nowTime := now().UTC()
	nowISO := formatJavaScriptISOString(nowTime)
	// Give the loop-state write a short lead over the queue claim so the scheduler
	// cannot observe a still-parked loop and discard its freshly recovered item.
	availableAt := formatJavaScriptISOString(nowTime.Add(time.Second))
	if err := ensureRecoveryQueueItem(ctx, repositories, loop, availableAt, -1); err != nil {
		return err
	}
	current, err := repositories.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("blocked loop disappeared: %s", loop.ID)
	}
	switch current.Status {
	case "paused", "awaiting_human", "shepherding":
	default:
		return nil
	}
	metadata, err := loopcondition.Clear(current.MetadataJSON)
	if err != nil {
		return err
	}
	if record.Kind == loopcondition.ProductSpec {
		metadata, err = writeMetadataBool(metadata, "awaitingProductSpec", false)
		if err != nil {
			return err
		}
	}
	current.MetadataJSON = &metadata
	current.Status = "queued"
	current.NextRunAt = &availableAt
	current.UpdatedAt = nowISO
	if stateJSON, stateErr := loopengine.Write(current.MetadataJSON, loopengine.FromLegacy(current.Status, "", nowISO)); stateErr == nil {
		current.MetadataJSON = &stateJSON
	}
	if err := repositories.Loops.Upsert(ctx, *current); err != nil {
		return err
	}
	payload := mustMarshalJSON(map[string]any{"condition": record.Kind, "clearedAt": nowISO})
	return appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   "loop.condition.cleared",
		ProjectID:   &current.ProjectID,
		LoopID:      &current.ID,
		EntityType:  stringPtr("loop"),
		EntityID:    &current.ID,
		PayloadJSON: payload,
		CreatedAt:   nowISO,
	})
}

func writeMetadataBool(metadataJSON, key string, value bool) (string, error) {
	var metadata map[string]any
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		return "", err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[key] = value
	payload, err := json.Marshal(metadata)
	return string(payload), err
}

func diskConditionCleared(cfg *config.Config) (bool, error) {
	if cfg == nil || !cfg.Daemon.DiskBackpressure.Enabled {
		return true, nil
	}
	path := strings.TrimSpace(cfg.Daemon.DiskBackpressure.Path)
	if path == "" {
		var err error
		path, err = config.DefaultWorktreeRoot()
		if err != nil {
			return false, err
		}
	}
	usage, err := diskUsageStat(path)
	if err != nil {
		return false, err
	}
	high := cfg.Daemon.DiskBackpressure.HighWatermarkPercent
	return high <= 0 || usage.UsedPercent < high, nil
}

func pullRequestForBlockedCondition(ctx context.Context, repositories *storage.Repositories, gateway *githubinfra.Gateway, loop storage.LoopRecord) (githubinfra.PullRequestDetail, error) {
	if gateway == nil || loop.Repo == nil || loop.PRNumber == nil || *loop.PRNumber <= 0 {
		return githubinfra.PullRequestDetail{}, fmt.Errorf("condition %s has no GitHub pull request", loop.ID)
	}
	cwd := ""
	if repositories != nil && repositories.Projects != nil {
		project, err := repositories.Projects.GetByID(ctx, loop.ProjectID)
		if err != nil {
			return githubinfra.PullRequestDetail{}, err
		}
		if project != nil {
			cwd = project.RepoPath
		}
	}
	return gateway.ViewPullRequestForReviewer(ctx, githubinfra.ViewPullRequestInput{Repo: *loop.Repo, PRNumber: *loop.PRNumber, CWD: cwd})
}

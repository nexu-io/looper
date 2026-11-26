package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/planner/decisions"
	"github.com/nexu-io/looper/internal/storage"
)

type decisionCheckpointView struct {
	PipelineVersion int              `json:"plannerPipelineVersion"`
	Phase           string           `json:"phase"`
	Decisions       *decisions.State `json:"decisions"`
	Wait            *struct {
		Reason     string `json:"reason"`
		ResumeStep string `json:"resumeStep"`
	} `json:"wait"`
	Issue *struct {
		URL string `json:"url"`
	} `json:"issue"`
}

// reconcileAwaitingRoleDecisions is the only Plane-inbound path for V2 requirement
// decisions. Feishu remains notification-only.
func (r *Runtime) reconcileAwaitingRoleDecisions(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil || repositories.Runs == nil || repositories.Queue == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	paused, err := repositories.Loops.ListByStatuses(ctx, []string{"paused", "queued"})
	if err != nil {
		return
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	resumeAt := formatJavaScriptISOString(now().UTC().Add(time.Second))
	var roleDialogueLLM triage.LLM
	for _, loop := range paused {
		if loop.Type != "planner" {
			continue
		}
		run, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil || run == nil || run.CheckpointJSON == nil {
			continue
		}
		var view decisionCheckpointView
		if json.Unmarshal([]byte(*run.CheckpointJSON), &view) != nil || view.PipelineVersion < 2 || view.Decisions == nil || view.Issue == nil {
			continue
		}
		stage := view.Decisions.Stage
		if stage != "awaiting_product" && stage != "awaiting_product_spec" && stage != "awaiting_downstream" && stage != "product_resolved" && stage != "downstream_resolved" {
			continue
		}
		gateway, planeProjectID, ok := r.resolvePlaneDoc(&cfg, loop.ProjectID)
		if !ok || gateway == nil {
			continue
		}
		workItemID := planedoc.WorkItemIDFromURL(view.Issue.URL)
		if workItemID == "" {
			continue
		}
		strictConversation := strictDispatchID(loop.MetadataJSON) != "" && strictRoleConversationEnabled(view.Decisions)
		if strictConversation {
			if roleDialogueLLM == nil {
				roleDialogueLLM = buildSpecApprovalLLM(cfg, repositories, now)
			}
			processed, processErr := processPendingStrictRoleMessage(ctx, cfg, loop, view.Decisions, roleDialogueLLM)
			if processErr != nil {
				if logger != nil {
					logger.Warn("role dialogue: process pending reply failed", map[string]any{"loopId": loop.ID, "error": processErr.Error()})
				}
				continue
			}
			if processed {
				if err := publishDecisionLog(ctx, gateway, planeProjectID, workItemID, loop.ID, view.Decisions); err != nil {
					continue
				}
				if err := persistDecisionCheckpoint(ctx, repositories, *run, view); err != nil {
					continue
				}
				if err := parkDecisionBarrier(ctx, repositories, loop, nowISO, "Waiting for Looper role dialogue to converge"); err != nil {
					continue
				}
				continue
			}
		}
		// Resolved stages are intentionally recoverable. persistDecisionCheckpoint,
		// queue creation, and loop requeue are separate durable writes; a crash after
		// the first must not leave a paused loop that no reconciler recognizes. They
		// still consume newer authorized Plane comments while queued: "resolved" is
		// a resumable checkpoint, not permission to ignore superseding authority.
		ready := stage == "product_resolved" || stage == "downstream_resolved"
		stateChanged := false
		formalSpecMissing := false
		formalSpecChanged := false
		if view.Decisions.Brief.FormalProductSpec.Required && stage != "awaiting_product_spec" {
			content, found, readErr := gateway.ReadPlanePageSpec(ctx, planeProjectID, workItemID, planedoc.ProductSpecLinkTitle)
			if readErr != nil {
				continue
			}
			if !found {
				view.Decisions.ProductSpec = ""
				clearDownstreamDecisionAuthority(view.Decisions)
				view.Decisions.Stage = "awaiting_product_spec"
				ready = false
				stateChanged = true
				formalSpecMissing = true
				if view.Wait != nil {
					view.Wait.Reason = "正式产品 Spec 链接已缺失，继续等待 looper:product-spec"
				}
			} else if content != view.Decisions.ProductSpec {
				view.Decisions.ProductSpec = content
				clearDownstreamDecisionAuthority(view.Decisions)
				view.Decisions.Stage = "product_resolved"
				ready = true
				stateChanged = true
				formalSpecChanged = true
				if view.Wait != nil {
					view.Wait.Reason = "正式产品 Spec 已变化，重新生成设计/研发决策 revision"
					view.Wait.ResumeStep = "grill-downstream-decisions"
				}
			}
		}
		if formalSpecMissing {
			// Persist and repair the paused barrier below.
		} else if formalSpecChanged {
			// Require a second fresh read of the formal Spec before requeueing.
		} else if stage == "awaiting_product_spec" {
			content, found, readErr := gateway.ReadPlanePageSpec(ctx, planeProjectID, workItemID, planedoc.ProductSpecLinkTitle)
			if readErr != nil || !found {
				continue
			}
			view.Decisions.ProductSpec = content
			view.Decisions.Stage = "product_resolved"
			ready = true
			stateChanged = true
		} else if strictConversation {
			roles := []decisions.Role{decisions.RoleProduct}
			if stage == "awaiting_downstream" || stage == "downstream_resolved" {
				roles = []decisions.Role{decisions.RoleDesign, decisions.RoleEngineering}
			}
			ready = len(decisions.UnansweredBlocking(*view.Decisions, roles...)) == 0
			if ready {
				if stage == "awaiting_product" {
					view.Decisions.Stage = "product_resolved"
				} else if stage == "awaiting_downstream" {
					view.Decisions.Stage = "downstream_resolved"
				}
			} else if stage == "product_resolved" {
				view.Decisions.Stage = "awaiting_product"
			} else if stage == "downstream_resolved" {
				view.Decisions.Stage = "awaiting_downstream"
			}
		} else {
			comments, listErr := gateway.ListWorkItemComments(ctx, planeProjectID, workItemID)
			if listErr != nil {
				continue
			}
			roles := []decisions.Role{decisions.RoleProduct}
			productChanged := false
			beforeRoleAnswers := map[decisions.Role]map[string]string{}
			if stage == "awaiting_downstream" || stage == "downstream_resolved" {
				roles = []decisions.Role{decisions.RoleDesign, decisions.RoleEngineering}
				before := answerValuesForRole(view.Decisions, decisions.RoleProduct)
				consumeDecisionAnswers(view.Decisions, decisions.RoleProduct, decisionPlaneIDForRequest(cfg, loop.ProjectID, view.Decisions, decisions.RoleProduct), comments)
				productChanged = !equalStringMaps(before, answerValuesForRole(view.Decisions, decisions.RoleProduct))
			}
			if productChanged {
				clearDownstreamDecisionAuthority(view.Decisions)
				stateChanged = true
				if view.Wait != nil {
					view.Wait.ResumeStep = "grill-downstream-decisions"
				}
				_ = publishSupersededAudit(ctx, gateway, planeProjectID, workItemID, loop.ID, view.Decisions.Brief.Revision)
				if len(decisions.UnansweredBlocking(*view.Decisions, decisions.RoleProduct)) > 0 {
					// A newer authorized product reply can invalidate an earlier answer
					// without replacing it (for example A+B in one conflicting comment).
					// Re-open the product barrier; never call that "resolved" merely because
					// the answer map changed.
					view.Decisions.Stage = "awaiting_product"
					ready = false
					if view.Wait != nil {
						view.Wait.Reason = "产品新回答冲突或不完整，继续等待当前 revision 的明确产品决策"
					}
				} else {
					view.Decisions.Stage = "product_resolved"
					ready = true
					if view.Wait != nil {
						view.Wait.Reason = "产品答案发生变化，重新生成设计/研发决策 revision"
					}
				}
			} else {
				for _, role := range roles {
					beforeRoleAnswers[role] = answerValuesForRole(view.Decisions, role)
					consumeDecisionAnswers(view.Decisions, role, decisionPlaneIDForRequest(cfg, loop.ProjectID, view.Decisions, role), comments)
					if !equalStringMaps(beforeRoleAnswers[role], answerValuesForRole(view.Decisions, role)) {
						stateChanged = true
					}
				}
				ready = len(decisions.UnansweredBlocking(*view.Decisions, roles...)) == 0
				if ready {
					if stage == "awaiting_product" {
						view.Decisions.Stage = "product_resolved"
					} else if stage == "awaiting_downstream" {
						view.Decisions.Stage = "downstream_resolved"
					}
				} else if stage == "product_resolved" {
					view.Decisions.Stage = "awaiting_product"
					if view.Wait != nil {
						view.Wait.Reason = "产品新回答冲突或不完整，继续等待当前 revision 的明确产品决策"
					}
				} else if stage == "downstream_resolved" {
					view.Decisions.Stage = "awaiting_downstream"
					if view.Wait != nil {
						view.Wait.Reason = "设计或研发新回答冲突或不完整，继续等待当前 revision 的明确决策"
					}
				}
			}
		}
		if !ready {
			// Partial role answers are still authoritative facts. Persist each changed
			// snapshot immediately and append its Decision Log before waiting for the
			// remaining role, so restarts and later comment edits cannot erase history.
			if stateChanged {
				if err := publishDecisionLog(ctx, gateway, planeProjectID, workItemID, loop.ID, view.Decisions); err != nil {
					continue
				}
				if err := persistDecisionCheckpoint(ctx, repositories, *run, view); err != nil {
					continue
				}
			}
			// This convergence is intentionally unconditional. It also repairs either
			// half of a prior partial failure: checkpoint persisted but loop still
			// queued, or loop paused but its active queue was not cancelled.
			if err := parkDecisionBarrier(ctx, repositories, loop, nowISO, "Decision authority unresolved or superseded; barrier paused"); err != nil {
				continue
			}
			continue
		}
		// Do not create a runnable queue in the same poll that first observes a
		// complete answer set. Persist the resolved snapshot and require one fresh
		// Plane poll to see it unchanged. This gives superseding role authority a
		// real reconciliation boundary instead of a one-second queue race.
		transitionedToResolved := (stage == "awaiting_product" || stage == "awaiting_product_spec" || stage == "awaiting_downstream") && (view.Decisions.Stage == "product_resolved" || view.Decisions.Stage == "downstream_resolved")
		authorityChangedAfterResolved := (stage == "product_resolved" || stage == "downstream_resolved") && stateChanged
		if transitionedToResolved || formalSpecChanged || authorityChangedAfterResolved {
			if err := publishDecisionLog(ctx, gateway, planeProjectID, workItemID, loop.ID, view.Decisions); err != nil {
				continue
			}
			// Park/cancel before persisting the changed resolved snapshot. If either
			// write fails, the old checkpoint still differs from Plane on the next tick,
			// so this branch retries instead of mistaking a partial write for stability.
			if err := parkDecisionBarrier(ctx, repositories, loop, nowISO, "Decision authority snapshot changed; waiting for one stable Plane poll"); err != nil {
				continue
			}
			if err := persistDecisionCheckpoint(ctx, repositories, *run, view); err != nil {
				continue
			}
			continue
		}
		if err := publishDecisionLog(ctx, gateway, planeProjectID, workItemID, loop.ID, view.Decisions); err != nil {
			if logger != nil {
				logger.Warn("decision reconcile: publish log failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if err := persistAndQueueResolvedDecision(ctx, repositories, loop, *run, view, nowISO, resumeAt, int64(cfg.Scheduler.RetryMaxAttempts)); err != nil {
			continue
		}
		if logger != nil {
			logger.Info("decision barrier resolved — planner resumed", map[string]any{"loopId": loop.ID, "stage": stage})
		}
	}
}

func parkDecisionBarrier(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO, reason string) error {
	if loop.Status != "paused" || loop.NextRunAt != nil {
		updated := loop
		updated.Status = "paused"
		updated.NextRunAt = nil
		updated.UpdatedAt = nowISO
		if err := repositories.Loops.Upsert(ctx, updated); err != nil {
			return err
		}
	}
	_, err := repositories.Queue.CancelByLoop(ctx, loop.ID, nowISO, &reason)
	return err
}

func clearDownstreamDecisionAuthority(state *decisions.State) {
	if state == nil {
		return
	}
	for id := range state.Answers {
		if strings.HasPrefix(id, "DESIGN-") || strings.HasPrefix(id, "ENG-") {
			delete(state.Answers, id)
		}
	}
	delete(state.Requests, decisions.RoleDesign)
	delete(state.Requests, decisions.RoleEngineering)
	delete(state.RequestedQuestions, decisions.RoleDesign)
	delete(state.RequestedQuestions, decisions.RoleEngineering)
	state.ImageMessages = map[string]string{}
}

func persistAndQueueResolvedDecision(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, run storage.RunRecord, view decisionCheckpointView, nowISO, resumeAt string, maxAttempts int64) error {
	if err := persistDecisionCheckpoint(ctx, repositories, run, view); err != nil {
		return err
	}
	updated := loop
	updated.Status = "queued"
	updated.NextRunAt = &resumeAt
	updated.UpdatedAt = nowISO
	meta := map[string]any{}
	if updated.MetadataJSON != nil {
		_ = json.Unmarshal([]byte(*updated.MetadataJSON), &meta)
	}
	meta["decisionPhase"] = view.Decisions.Stage
	meta["nodeHPhase"] = view.Decisions.Stage
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	value := string(encoded)
	updated.MetadataJSON = &value
	// Queue only after the loop becomes recoverably queued. If we crash here, the
	// next lightweight reconcile scans queued+resolved loops and repairs the missing
	// item; the scheduler can never consume an item while the loop is paused.
	if err := repositories.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	return ensureRecoveryQueueItem(ctx, repositories, updated, resumeAt, maxAttempts)
}

func decisionPlaneID(cfg config.Config, projectID string, role decisions.Role) string {
	switch role {
	case decisions.RoleProduct:
		return strings.TrimSpace(config.ProjectProductOwner(cfg, projectID).PlaneID)
	case decisions.RoleDesign:
		return strings.TrimSpace(config.ProjectDesignOwner(cfg, projectID).PlaneID)
	case decisions.RoleEngineering:
		return strings.TrimSpace(config.ProjectOwnerActor(cfg, projectID).PlaneID)
	default:
		return ""
	}
}

func decisionPlaneIDForRequest(cfg config.Config, projectID string, state *decisions.State, role decisions.Role) string {
	if state != nil {
		if request, ok := state.Requests[role]; ok && strings.TrimSpace(request.EligibleMemberID) != "" {
			return strings.TrimSpace(request.EligibleMemberID)
		}
	}
	return decisionPlaneID(cfg, projectID, role)
}

func consumeDecisionAnswers(state *decisions.State, role decisions.Role, actorID string, comments []planedoc.WorkItemComment) {
	actorID = strings.TrimSpace(actorID)
	request, ok := state.Requests[role]
	boundary, err := time.Parse(time.RFC3339Nano, request.CreatedAt)
	if !ok || actorID == "" || err != nil {
		return
	}
	questions := state.RequestedQuestions[role]
	if len(questions) == 0 {
		questions = decisions.QuestionsForRole(state.Brief, role)
	}
	if state.Answers == nil {
		state.Answers = map[string]decisions.Answer{}
	}
	filteredQuestions := make([]decisions.Question, 0, len(questions))
	for _, question := range questions {
		if question.ID != "PROD-000" {
			filteredQuestions = append(filteredQuestions, question)
		}
	}
	questions = filteredQuestions
	if len(questions) == 0 {
		return
	}
	ordered := append([]planedoc.WorkItemComment(nil), comments...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt < ordered[j].CreatedAt })
	for _, comment := range ordered {
		if strings.TrimSpace(comment.Actor) != actorID || comment.ID == request.CommentID || strings.Contains(comment.CommentHTML, planedoc.LooperCommentMarker) {
			continue
		}
		createdAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(comment.CreatedAt))
		if parseErr != nil || !createdAt.After(boundary) {
			continue
		}
		parsed, conflicted := decisions.ParseAnswerSet(workItemCommentText(comment), questions)
		for id := range conflicted {
			delete(state.Answers, id)
		}
		for id, value := range parsed {
			questionHash := ""
			for _, question := range questions {
				if question.ID == id {
					questionHash = decisions.QuestionHash(question)
					break
				}
			}
			state.Answers[id] = decisions.Answer{QuestionID: id, Value: value, Revision: request.Revision, QuestionHash: questionHash, CommentID: comment.ID, Actor: comment.Actor, CreatedAt: comment.CreatedAt}
		}
	}
}

var commentTagPattern = regexp.MustCompile(`<[^>]+>`)

func workItemCommentText(comment planedoc.WorkItemComment) string {
	// Plane's current work-item comment response uses `description` as an
	// internal content-object UUID, not the visible comment body. Prefer the
	// actual HTML; keep Description only as a compatibility fallback for older
	// Plane shapes and test doubles.
	text := strings.TrimSpace(comment.CommentHTML)
	if text == "" {
		text = strings.TrimSpace(comment.Description)
	}
	text = strings.ReplaceAll(text, "<br>", "\n")
	text = strings.ReplaceAll(text, "<br/>", "\n")
	text = strings.ReplaceAll(text, "</p>", "\n")
	return strings.TrimSpace(html.UnescapeString(commentTagPattern.ReplaceAllString(text, "")))
}

func publishDecisionLog(ctx context.Context, gateway *planedoc.Gateway, projectID, workItemID, loopID string, state *decisions.State) error {
	snapshot := struct {
		Stage       string                      `json:"stage"`
		ProductSpec string                      `json:"productSpec,omitempty"`
		Answers     map[string]decisions.Answer `json:"answers,omitempty"`
	}{Stage: state.Stage, ProductSpec: state.ProductSpec, Answers: state.Answers}
	encoded, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(encoded)
	marker := fmt.Sprintf("<!-- looper:decision-log v=1 loop=%s revision=%d snapshot=%x -->", loopID, state.Brief.Revision, sum[:8])
	comments, err := gateway.ListWorkItemComments(ctx, projectID, workItemID)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if strings.Contains(comment.CommentHTML, marker) {
			state.DecisionLog = comment.ID
			return nil
		}
	}
	body := marker + "<p><b>Decision Log · revision " + fmt.Sprint(state.Brief.Revision) + " · " + html.EscapeString(state.Stage) + "</b></p>"
	for _, line := range strings.Split(decisions.DecisionLogMarkdown(*state), "\n") {
		if strings.HasPrefix(line, "- ") {
			body += "<p>" + html.EscapeString(strings.TrimPrefix(line, "- ")) + "</p>"
		}
	}
	created, err := gateway.CreateWorkItemComment(ctx, projectID, workItemID, planedoc.SignComment(body, "decision-log", ""))
	if err != nil {
		return err
	}
	state.DecisionLog = created.ID
	return nil
}

func persistDecisionCheckpoint(ctx context.Context, repos *storage.Repositories, run storage.RunRecord, view decisionCheckpointView) error {
	var checkpoint map[string]any
	if run.CheckpointJSON == nil || json.Unmarshal([]byte(*run.CheckpointJSON), &checkpoint) != nil {
		return fmt.Errorf("decision checkpoint is unavailable")
	}
	checkpoint["decisions"] = view.Decisions
	checkpoint["phase"] = view.Decisions.Stage
	if view.Wait != nil {
		checkpoint["wait"] = view.Wait
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	updated := run
	value := string(encoded)
	updated.CheckpointJSON = &value
	return repos.Runs.Upsert(ctx, updated)
}

func answerValuesForRole(state *decisions.State, role decisions.Role) map[string]string {
	out := map[string]string{}
	for _, question := range state.RequestedQuestions[role] {
		if answer, ok := state.Answers[question.ID]; ok {
			out[question.ID] = answer.Value
		}
	}
	return out
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func publishSupersededAudit(ctx context.Context, gateway *planedoc.Gateway, projectID, workItemID, loopID string, revision int) error {
	marker := fmt.Sprintf("<!-- looper:decision-superseded v=1 loop=%s revision=%d -->", loopID, revision)
	comments, err := gateway.ListWorkItemComments(ctx, projectID, workItemID)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if strings.Contains(comment.CommentHTML, marker) {
			return nil
		}
	}
	_, err = gateway.CreateWorkItemComment(ctx, projectID, workItemID, planedoc.SignComment(marker+"<p>♻️ 产品答案已变化，旧设计/研发问题、答案与截图已 superseded；Looper 将发布新 revision。</p>", "decision-router", ""))
	return err
}

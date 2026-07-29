package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	coordinatorrole "github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/loops"
)

// Plane auto-intake labels. A colleague applies ONLY looper:auto; this reconciler
// classifies the work item and routes it into the EXISTING planner (looper:plan) /
// worker (looper:worker-ready) pipeline. It is the TOP of the looper:auto flowchart
// (classify → product-spec gate → route) — the segment the GitHub coordinator
// provides for GitHub issues but which the Plane pipeline lacked, so a single
// looper:auto label never has to be maintained by hand.
const (
	autoLabel        = "looper:auto"
	planTriggerLabel = "looper:plan"
	// workerReadyLabel ("looper:worker-ready") is defined in spec_approval.go (same package).

	dispatchImplementLabel = "dispatch/implement"
	kindFeatureLabel       = "kind/feature"

	// Intake hold/terminal markers, so classification runs at most once per item.
	intakeAwaitingProductLabel = "looper:awaiting-product-spec"
	intakeOutOfScopeLabel      = "looper:out-of-scope"
	intakeNeedsHumanLabel      = "looper:needs-human"

	autoIntakeEnvVar = "LOOPER_PLANE_AUTO_INTAKE"
)

// maxAutoIntakeClassificationsPerTick bounds how many FRESH looper:auto items are
// classified (one agent run each) per reconcile tick, so a burst of newly-labelled
// items drains over several ticks instead of running classifications back-to-back and
// blocking the tick loop. Already-routed/held items are cheap and never counted.
const maxAutoIntakeClassificationsPerTick = 3

// intakeRoute is the next action for a looper:auto item after classification.
type intakeRoute int

const (
	intakeSkip intakeRoute = iota
	intakeRouteToPlan
	intakeRouteToImplement
	intakeHoldForProductSpec
	intakeHoldUnclear
	intakeMarkOutOfScope
)

// decideAutoIntakeRoute maps a triage decision + product-spec presence to the next
// intake action (flowchart nodes B–F). Pure — unit-tested without any I/O.
//
//	out-of-scope                        → mark, stop
//	unclear                             → hold, @human       (拿不准 → HITL 问人)
//	valid + feature + no product spec   → V2 planner research; V1 hold @product
//	valid + dispatch/implement          → worker directly    (简单 bug: 直接修)
//	valid + dispatch/plan (or default)  → planner writes spec (需求 / 复杂 bug)
func decideAutoIntakeRoute(decision triage.Decision, hasProductSpec, preSpecDecisionGrill bool) intakeRoute {
	if decision.NoOp {
		return intakeSkip
	}
	switch decision.Disposition {
	case triage.DispositionOutOfScope:
		return intakeMarkOutOfScope
	case triage.DispositionUnclear:
		return intakeHoldUnclear
	}
	if labelsContainFold(decision.ApplyLabels, kindFeatureLabel) && !hasProductSpec && !preSpecDecisionGrill {
		return intakeHoldForProductSpec
	}
	if labelsContainFold(decision.ApplyLabels, dispatchImplementLabel) {
		return intakeRouteToImplement
	}
	// dispatch/plan, or any other valid disposition → spec-first (safe default).
	return intakeRouteToPlan
}

func autoIntakeEnabled() bool {
	return strings.TrimSpace(os.Getenv(autoIntakeEnvVar)) == "1"
}

func labelsContainFold(labels []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

func labelNames(labels []forge.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if n := strings.TrimSpace(l.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// reconcileAutoIntake drives the top of the looper:auto flowchart for Plane
// projects. Env-gated (LOOPER_PLANE_AUTO_INTAKE=1) so it is zero-risk to existing
// deployments until enabled. Each looper:auto work item is classified once via the
// shared triage.Decide; the resulting dispatch decision stamps looper:plan or
// looper:worker-ready and the existing planner/worker discovery takes it from there.
func (r *Runtime) reconcileAutoIntake(ctx context.Context) {
	if !autoIntakeEnabled() {
		return
	}
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	r.mu.RUnlock()
	if repositories == nil || cfg.Agent.Vendor == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	executor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor:              *cfg.Agent.Vendor,
			Model:               cfg.Agent.Model,
			Params:              cfg.Agent.Params,
			Env:                 cfg.Agent.Env,
			NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
		},
		Repos:  repositories,
		LogDir: cfg.Daemon.LogDir,
		Now:    now,
	})
	llm := coordinatorrole.NewAgentLLM(executor, now,
		time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds)*time.Second,
		time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds)*time.Second,
	)
	plane := 0
	for _, project := range cfg.Projects {
		if config.ResolvedProjectProviderKind(cfg, project) != config.ProviderKindPlane {
			continue
		}
		if !autoIntakeProjectEnabled(cfg, project.ID) {
			// A planner-only manual run or an isolated E2E project must not mutate
			// unrelated Plane items merely because the process inherited the global
			// LOOPER_PLANE_AUTO_INTAKE=1 environment variable.
			continue
		}
		plane++
		r.reconcileAutoIntakeProject(ctx, &cfg, project, llm, logger, now)
	}
	if logger != nil {
		logger.Info("auto-intake: tick", map[string]any{"planeProjects": plane, "totalProjects": len(cfg.Projects)})
	}
}

func autoIntakeProjectEnabled(cfg config.Config, projectID string) bool {
	roles := config.ProjectRoleConfigs(cfg, projectID)
	return roles.Planner.AutoDiscovery || roles.Worker.AutoDiscovery
}

func (r *Runtime) reconcileAutoIntakeProject(ctx context.Context, cfg *config.Config, project config.ProjectRefConfig, llm triage.LLM, logger bootstrap.Logger, now func() time.Time) {
	gateway, planeProjectID, ok := planeDocForProject(cfg, project.ID)
	if !ok || gateway == nil {
		return
	}
	provider, found := forgejoProviderByID(*cfg, project.Provider)
	if !found {
		return
	}
	client, err := forge.NewPlaneClientFromConfig(provider, project.Repo)
	if err != nil {
		if logger != nil {
			logger.Warn("auto-intake: plane client build failed", map[string]any{"projectId": project.ID, "error": err.Error()})
		}
		return
	}
	// Partition by assignee, the SAME key the worker discovers on (planeAssigneeId), so
	// when several people run looper on one Plane project each daemon only classifies
	// its OWN assigned looper:auto items — no duplicate classification, no racing to
	// stamp labels/threads, no N× agent burn. Empty means "classify everything", which
	// is only safe for a single-tenant deploy, so warn.
	assignee := strings.TrimSpace(config.ProjectRoleConfigs(*cfg, project.ID).Worker.Triggers.PlaneAssigneeID)
	if assignee == "" && logger != nil {
		logger.Warn("auto-intake: no roles.worker.triggers.planeAssigneeId set — classifying ALL looper:auto items; unsafe when multiple people run looper on the same project", map[string]any{"projectId": project.ID})
	}
	items, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{Labels: []string{autoLabel}, Assignee: assignee})
	if err != nil {
		if logger != nil {
			logger.Warn("auto-intake: list looper:auto items failed", map[string]any{"projectId": project.ID, "error": err.Error()})
		}
		return
	}
	if logger != nil {
		logger.Info("auto-intake: listed items", map[string]any{"projectId": project.ID, "planeProjectId": planeProjectID, "count": len(items), "assignee": assignee})
	}
	// Bound fresh classifications this tick; leftovers drain on the next one.
	classifyBudget := maxAutoIntakeClassificationsPerTick
	preSpecDecisionGrill := config.ProjectRoleConfigs(*cfg, project.ID).Planner.PreSpecDecisionGrill
	for _, item := range items {
		r.reconcileAutoIntakeItem(ctx, gateway, client, planeProjectID, project, item, llm, logger, now, &classifyBudget, preSpecDecisionGrill)
	}
}

func (r *Runtime) reconcileAutoIntakeItem(ctx context.Context, gateway *planedoc.Gateway, client *forge.PlaneClient, planeProjectID string, project config.ProjectRefConfig, item forge.Issue, llm triage.LLM, logger bootstrap.Logger, now func() time.Time, classifyBudget *int, preSpecDecisionGrill bool) {
	names := labelNames(item.Labels)
	// Already routed into the pipeline — nothing to do.
	if labelsContainFold(names, planTriggerLabel) || labelsContainFold(names, workerReadyLabel) {
		return
	}
	// Terminal intake states that need a human, not another classification.
	if labelsContainFold(names, intakeOutOfScopeLabel) || labelsContainFold(names, intakeNeedsHumanLabel) {
		return
	}
	// Already in the pipeline: a coordinator/planner/worker loop for this item exists.
	// looper:auto is a durable peer trigger that stays on the item for its whole life,
	// so once a routing label is retired mid-flight (e.g. the planner drops looper:plan
	// while the spec awaits human approval, or a completed planner leaves only looper:auto)
	// the item would otherwise look "fresh" and get re-classified, clobbering the in-flight
	// or already-approved work. The loop is the source of truth: ANY existing loop (live OR
	// terminal) means this item already went through triage once, so we do NOT re-classify.
	// (A genuine retry of a needs-human dead-end is an EXPLICIT operation that retires the
	// stale loop first — never an implicit "the loop is terminal so re-run it", which also
	// re-did items whose planner spec was already written and human-approved.)
	if repos := r.services.Repositories; repos != nil && repos.Loops != nil {
		if existing, err := repos.Loops.GetByTargetID(ctx, fmt.Sprintf("issue:%s:%d", project.Repo, item.Number)); err == nil && existing != nil {
			// A coordinator loop is only the notification/thread anchor created
			// immediately before classification. If the daemon stops in that
			// window, recovery requeues the anchor but there is no queue item that
			// could ever finish it. Resume classification and reuse that anchor;
			// planner/worker loops still prove the item was actually routed.
			if domain.LoopType(existing.Type) != domain.LoopTypeCoordinator {
				return
			}
		}
	}
	workItemID := planedoc.WorkItemIDFromURL(item.HTMLURL)
	if workItemID == "" {
		return
	}

	// Held awaiting a product spec: re-check the gate WITHOUT re-classifying (we
	// already know it is a feature). When the spec appears, drop the hold and route
	// to the planner.
	if labelsContainFold(names, intakeAwaitingProductLabel) {
		if preSpecDecisionGrill {
			// Upgrade recovery: a V1 item may already carry the old product-spec hold.
			// V2 deliberately researches first, so retire the hold without requiring a
			// product document and let the planner's product barrier make that decision.
			_ = gateway.RemoveWorkItemLabel(ctx, planeProjectID, workItemID, intakeAwaitingProductLabel)
			r.routeIntake(ctx, gateway, planeProjectID, workItemID, planTriggerLabel, "<p>🔬 已启用技术 Spec 前的需求调研/GRILL；先进入 planner，由调研结果判断是否真正需要正式产品 Spec。</p>", logger, project.ID, item.Number)
			return
		}
		present, _, err := gateway.HasProductSpec(ctx, planeProjectID, workItemID)
		if err != nil || !present {
			return
		}
		_ = gateway.RemoveWorkItemLabel(ctx, planeProjectID, workItemID, intakeAwaitingProductLabel)
		r.routeIntake(ctx, gateway, planeProjectID, workItemID, planTriggerLabel, "<p>✅ product spec 已补齐,进入技术方案(planner)。</p>", logger, project.ID, item.Number)
		return
	}

	// Per-tick classification cap: a fresh item beyond this tick's budget waits for the
	// next tick, so a burst of freshly-labelled items can't run an unbounded number of
	// classification agents back-to-back and stall the reconcile loop.
	if classifyBudget != nil {
		if *classifyBudget <= 0 {
			if logger != nil {
				logger.Info("auto-intake: per-tick classification cap reached — deferring item to next tick", map[string]any{"projectId": project.ID, "item": item.Number})
			}
			return
		}
		*classifyBudget--
	}

	// Fresh looper:auto item — classify once (flowchart node A/B). Open the shared
	// task thread NOW (before classifying) via a card-anchor coordinator loop, so the
	// whole run (classify → spec → worker → shepherd) lives on ONE Feishu thread and
	// the classification isn't a black box: 🧭 分类中 → its verdict + reasoning →
	// 实现中 (the worker joins the same task-keyed anchor).
	coordLoopID := r.createIntakeAnchor(ctx, project, item, logger)
	// route is resolved below; the defer captures its final value so the shared task
	// anchor retires to the routing outcome (parked → 转人工/超范围/等产品方案;
	// routed → a bridge the downstream loop overwrites) instead of freezing on 分类中.
	route := intakeSkip
	defer func() { r.completeIntakeAnchor(ctx, coordLoopID, route) }()

	comments := intakeComments(ctx, client, item.Number)
	decision := triage.Decide(ctx, llm, triage.Input{
		Issue: triage.Issue{
			Number:   item.Number,
			Title:    item.Title,
			Body:     item.Body,
			URL:      item.HTMLURL,
			Labels:   names,
			Comments: comments,
		},
		RepoContext: triage.RepoContext{Repo: project.Repo, WorkingDirectory: project.RepoPath},
		Config:      triage.Config{OutOfScopeLabel: intakeOutOfScopeLabel, UnclearLabel: intakeNeedsHumanLabel},
		Now:         now().UTC(),
	})

	present, _, _ := gateway.HasProductSpec(ctx, planeProjectID, workItemID)
	route = decideAutoIntakeRoute(decision, present, preSpecDecisionGrill)

	// Post the verdict + reasoning into the shared task thread BEFORE routing, so
	// "why simple bug / why no spec" is transparent and interruptible (HITL).
	r.postIntakeReasoning(ctx, coordLoopID, decision, route)

	// Stamp the LLM's audit labels (kind/*, complexity/*, dispatch/*) so the
	// classification is visible on the item, mirroring the GitHub coordinator.
	if len(decision.ApplyLabels) > 0 {
		if _, err := client.AddIssueLabels(ctx, item.Number, decision.ApplyLabels); err != nil && logger != nil {
			logger.Warn("auto-intake: stamp audit labels failed", map[string]any{"projectId": project.ID, "item": item.Number, "error": err.Error()})
		}
	}

	switch route {
	case intakeRouteToPlan:
		r.routeIntake(ctx, gateway, planeProjectID, workItemID, planTriggerLabel, intakeComment(decision, "技术方案(planner)"), logger, project.ID, item.Number)
	case intakeRouteToImplement:
		r.routeIntake(ctx, gateway, planeProjectID, workItemID, workerReadyLabel, intakeComment(decision, "直接实现(worker)"), logger, project.ID, item.Number)
	case intakeHoldForProductSpec:
		_ = gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, intakeAwaitingProductLabel)
		// Plane-side node E ask. The Feishu @product card is a separate surface driven
		// off the same hold label once the item is picked up downstream.
		comment, err := gateway.RequestProductSpec(ctx, planeProjectID, workItemID, "产品负责人", item.Title)
		if err != nil && logger != nil {
			logger.Warn("auto-intake: request product spec failed", map[string]any{"projectId": project.ID, "item": item.Number, "error": err.Error()})
		}
		if err == nil && coordLoopID != "" {
			if notifyGateway, ok := r.shepherdNotifyGateway(); ok {
				actionURL := planedoc.WorkItemCommentURL(item.HTMLURL, comment.ID)
				owner := strings.TrimSpace(config.ProjectProductOwner(r.config, project.ID).FeishuOpenID)
				mentions := []string{}
				if owner != "" {
					mentions = []string{owner}
				}
				_ = notifyGateway.PostThreadDecisionCard(ctx, coordLoopID, "这个功能还缺 product spec。请前往 Plane 的具体评论补充方案页链接或正文；飞书回复不会被读取。", actionURL, mentions)
			}
		}
		r.logIntake(logger, project.ID, item.Number, "hold: awaiting product spec")
	case intakeHoldUnclear:
		_ = gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, intakeNeedsHumanLabel)
		_ = gateway.CommentOnWorkItem(ctx, planeProjectID, workItemID, intakeComment(decision, "拿不准,等人确认(HITL)"))
		r.logIntake(logger, project.ID, item.Number, "hold: unclear → needs-human")
	case intakeMarkOutOfScope:
		_ = gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, intakeOutOfScopeLabel)
		_ = gateway.CommentOnWorkItem(ctx, planeProjectID, workItemID, intakeComment(decision, "超出范围,已标记"))
		r.logIntake(logger, project.ID, item.Number, "out-of-scope")
	default:
		// intakeSkip: the classifier produced no valid decision (e.g. a
		// non-conforming answer that failed schema validation). Stamp needs-human so
		// a person can classify it by hand AND so we don't re-run the LLM on this
		// item every tick — matching the flowchart's "拿不准 → HITL 问人" leaf.
		_ = gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, intakeNeedsHumanLabel)
		_ = gateway.CommentOnWorkItem(ctx, planeProjectID, workItemID, "<p>🧭 looper 无法自动分类这条(分类器未产出有效结论),已转人工。请补充 kind/dispatch 或直接打 looper:plan / looper:worker-ready。</p>")
		r.logIntake(logger, project.ID, item.Number, "skip → needs-human")
	}
}

// routeIntake stamps the pipeline trigger label (looper:plan or looper:worker-ready)
// and drops an audit comment. The existing planner/worker discovery picks it up.
func (r *Runtime) routeIntake(ctx context.Context, gateway *planedoc.Gateway, planeProjectID, workItemID, triggerLabel, commentHTML string, logger bootstrap.Logger, projectID string, number int64) {
	if err := gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, triggerLabel); err != nil {
		if logger != nil {
			logger.Warn("auto-intake: stamp trigger label failed", map[string]any{"projectId": projectID, "item": number, "label": triggerLabel, "error": err.Error()})
		}
		return
	}
	if strings.TrimSpace(commentHTML) != "" {
		_ = gateway.CommentOnWorkItem(ctx, planeProjectID, workItemID, commentHTML)
	}
	r.logIntake(logger, projectID, number, "routed → "+triggerLabel)
}

func (r *Runtime) logIntake(logger bootstrap.Logger, projectID string, number int64, outcome string) {
	if logger != nil {
		logger.Info("auto-intake", map[string]any{"projectId": projectID, "item": number, "outcome": outcome})
	}
}

// createIntakeAnchor opens the shared task thread at the classification stage by
// creating a card-anchor coordinator loop (no queue item → the scheduler never runs
// it) and rendering its 🧭 分类中 header. Every downstream loop (planner, worker,
// shepherd) for this work item collapses onto the SAME anchor by task_key
// (issue:repo:<seq>), so classify → spec → worker → merge is ONE Feishu thread.
// Returns the coordinator loop id, or "" when notifications are off / creation fails.
func (r *Runtime) createIntakeAnchor(ctx context.Context, project config.ProjectRefConfig, item forge.Issue, logger bootstrap.Logger) string {
	gw, ok := r.shepherdNotifyGateway()
	if !ok || r.services.Loops == nil {
		return ""
	}
	targetID := fmt.Sprintf("issue:%s:%d", project.Repo, item.Number)
	if repos := r.services.Repositories; repos != nil && repos.Loops != nil {
		if existing, err := repos.Loops.GetByTargetID(ctx, targetID); err == nil && existing != nil && domain.LoopType(existing.Type) == domain.LoopTypeCoordinator {
			status := domain.LoopStatus(existing.Status)
			if status != domain.LoopStatusRunning {
				if status != domain.LoopStatusQueued {
					if _, err := r.services.Loops.TransitionStatus(ctx, existing.ID, loops.TransitionInput{Status: domain.LoopStatusQueued}); err != nil {
						status = ""
					} else {
						status = domain.LoopStatusQueued
					}
				}
				if status == domain.LoopStatusQueued {
					if _, err := r.services.Loops.TransitionStatus(ctx, existing.ID, loops.TransitionInput{Status: domain.LoopStatusRunning}); err == nil {
						status = domain.LoopStatusRunning
					}
				}
			}
			if status == domain.LoopStatusRunning {
				gw.RefreshThreadHeader(ctx, existing.ID, nil, 0)
				return existing.ID
			}
		}
	}
	meta, err := json.Marshal(map[string]any{"issueNumber": item.Number, "issueUrl": item.HTMLURL, "title": strings.TrimSpace(item.Title)})
	if err != nil {
		return ""
	}
	metaStr := string(meta)
	loop, err := r.services.Loops.Create(ctx, loops.CreateInput{
		ProjectID:    project.ID,
		Type:         domain.LoopTypeCoordinator,
		Target:       domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, ProjectID: project.ID, Repo: project.Repo, IssueNumber: item.Number},
		Status:       domain.LoopStatusRunning,
		MetadataJSON: &metaStr,
	})
	if err != nil {
		if logger != nil {
			logger.Warn("auto-intake: create anchor loop failed", map[string]any{"projectId": project.ID, "item": item.Number, "error": err.Error()})
		}
		return ""
	}
	gw.RefreshThreadHeader(ctx, loop.ID, nil, 0) // posts the 🧭 分类中 anchor card (looper:auto)
	_ = gw.PostThreadNote(ctx, loop.ID, "🤖 已接手,正在分类…", nil)
	return loop.ID
}

// postIntakeReasoning posts the classifier's verdict + reasoning into the shared task
// thread. Best-effort; a no-op when the anchor wasn't created or the route is a skip.
func (r *Runtime) postIntakeReasoning(ctx context.Context, coordLoopID string, decision triage.Decision, route intakeRoute) {
	if coordLoopID == "" {
		return
	}
	routeText := map[intakeRoute]string{
		intakeRouteToPlan:        "写技术方案(planner)→ 评审 → 实现",
		intakeRouteToImplement:   "直接实现(worker),不写 spec",
		intakeHoldForProductSpec: "缺 product spec,@产品补齐后再走",
		intakeHoldUnclear:        "拿不准,转人工确认",
		intakeMarkOutOfScope:     "超出范围,已标记",
	}[route]
	if routeText == "" {
		return
	}
	gw, ok := r.shepherdNotifyGateway()
	if !ok {
		return
	}
	kind := "(未分类)"
	for _, l := range decision.ApplyLabels {
		if strings.HasPrefix(strings.TrimSpace(l), "kind/") {
			kind = strings.TrimSpace(l)
			break
		}
	}
	text := fmt.Sprintf("🧭 分类:%s → %s", kind, routeText)
	if reason := strings.TrimSpace(decision.CommentBody); reason != "" {
		text += "\n理由:" + reason
	}
	_ = gw.PostThreadNote(ctx, coordLoopID, text, nil)
}

// completeIntakeAnchor retires the card-anchor coordinator loop once routing is done
// and repaints the shared task anchor to the routing OUTCOME. A parked item (needs
// human / out-of-scope / awaiting product) has no downstream loop to take the anchor
// over, so without this repaint its header would freeze on "🧭 分类中"; a routed item
// gets a bridge label the planner/worker overwrites once it renders. Best-effort.
func (r *Runtime) completeIntakeAnchor(ctx context.Context, coordLoopID string, route intakeRoute) {
	if coordLoopID == "" || r.services.Loops == nil {
		return
	}
	_, _ = r.services.Loops.TransitionStatus(ctx, coordLoopID, loops.TransitionInput{Status: domain.LoopStatusCompleted})
	if gw, ok := r.shepherdNotifyGateway(); ok {
		gw.FinalizeIntakeAnchor(ctx, coordLoopID, intakeOutcomeKey(route))
	}
}

// intakeOutcomeKey names the anchor outcome for a routing decision (see
// notify.FinalizeIntakeAnchor), so a retired looper:auto anchor shows where the task
// went instead of freezing on 分类中.
func intakeOutcomeKey(route intakeRoute) string {
	switch route {
	case intakeRouteToPlan:
		return "routed_plan"
	case intakeRouteToImplement:
		return "routed_worker"
	case intakeHoldForProductSpec:
		return "hold_product"
	case intakeMarkOutOfScope:
		return "out_of_scope"
	default: // intakeHoldUnclear, intakeSkip
		return "needs_human"
	}
}

func intakeComments(ctx context.Context, client *forge.PlaneClient, number int64) []triage.Comment {
	raw, err := client.ListIssueComments(ctx, number)
	if err != nil {
		return nil
	}
	out := make([]triage.Comment, 0, len(raw))
	for _, c := range raw {
		out = append(out, triage.Comment{ID: c.ID, Author: c.User.Login, Body: c.Body, CreatedAt: c.UpdatedAt, UpdatedAt: c.UpdatedAt})
	}
	return out
}

func intakeComment(decision triage.Decision, routeText string) string {
	summary := strings.TrimSpace(decision.CommentBody)
	if summary == "" {
		summary = "已分类。"
	}
	return fmt.Sprintf("<p>🧭 looper 分类:%s → %s</p>", html.EscapeString(summary), html.EscapeString(routeText))
}

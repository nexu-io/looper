package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	coordinatorrole "github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
)

// workerReadyLabel is the work-item label the worker discovers on (node I). node H
// stamps it once the tech spec is approved, handing off from planner to worker.
const workerReadyLabel = "looper:worker-ready"

type specApprovalState struct {
	Awaiting         bool
	Dispatched       bool
	IssueURL         string
	JudgedHash       string
	Revision         int
	ContentHash      string
	RequestCommentID string
	RequestedAt      string
	StrictDispatchID string
}

type specApprovalJudgeFunc func(context.Context, []planedoc.PageComment, string) (specApprovalVerdict, error)

// loopSpecApprovalState reads a completed planner loop's revision-bound node H gate.
func loopSpecApprovalState(metadataJSON *string) specApprovalState {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return specApprovalState{}
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return specApprovalState{}
	}
	state := specApprovalState{}
	state.Awaiting, _ = meta["awaitingSpecApproval"].(bool)
	state.Dispatched, _ = meta["specApprovedDispatched"].(bool)
	if s, ok := meta["issueUrl"].(string); ok {
		state.IssueURL = strings.TrimSpace(s)
	} else if s, ok := meta["issueURL"].(string); ok {
		state.IssueURL = strings.TrimSpace(s)
	}
	// specApprovalJudgedHash is the coalescing cursor: the fingerprint of the human
	// comment set last sent to the LLM judge, so an unchanged set isn't re-judged.
	if s, ok := meta["specApprovalJudgedHash"].(string); ok {
		state.JudgedHash = strings.TrimSpace(s)
	}
	if value, ok := meta["specApprovalRevision"].(float64); ok {
		state.Revision = int(value)
	}
	state.ContentHash, _ = meta["specApprovalContentHash"].(string)
	state.RequestCommentID, _ = meta["specApprovalRequestCommentID"].(string)
	state.RequestedAt, _ = meta["specApprovalRequestedAt"].(string)
	state.StrictDispatchID, _ = meta["strictDispatchId"].(string)
	return state
}

// reconcileSpecApproval drives flowchart node H → node I. For completed planner loops
// on Plane projects whose tech spec is awaiting review, it polls the spec page for a
// HUMAN approval (a non-[looper] approve comment) and — once approved — stamps
// looper:worker-ready on the work item so the worker picks it up (node I), drops an
// audit comment on the page, and marks the loop dispatched so it fires exactly once.
// Polling, since looper has no Plane webhook. Best-effort; a no-op without storage.
func (r *Runtime) reconcileSpecApproval(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	judgeOverride := r.specApprovalJudge
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	completed, err := repositories.Loops.ListByStatuses(ctx, []string{"completed"})
	if err != nil {
		return
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	// Built lazily — only when a loop actually has a fresh comment set to judge — so an
	// idle tick with nothing awaiting never constructs an executor.
	var specJudge triage.LLM
	for _, loop := range completed {
		if !strings.EqualFold(strings.TrimSpace(loop.Type), "planner") {
			continue
		}
		state := loopSpecApprovalState(loop.MetadataJSON)
		if !state.Awaiting || state.Dispatched {
			continue
		}
		// Older/in-flight planner loops may have opened the revision gate before
		// issueUrl was copied into loop metadata. Recover it from the durable latest
		// planner checkpoint so approval polling does not silently wedge after an
		// upgrade or daemon restart.
		if state.IssueURL == "" {
			state.IssueURL = latestPlannerIssueURL(ctx, repositories, loop.ID)
		}
		owner := config.ProjectOwnerActor(cfg, loop.ProjectID)
		ownerPlaneID := strings.TrimSpace(owner.PlaneID)
		// Fail closed: without an explicit Plane identity Looper cannot distinguish its
		// local technical owner from product/design/other commenters.
		if ownerPlaneID == "" || state.Revision < 1 || strings.TrimSpace(state.ContentHash) == "" || strings.TrimSpace(state.RequestedAt) == "" {
			if logger != nil {
				logger.Warn("spec approval: owner identity or revision boundary missing; waiting", map[string]any{"loopId": loop.ID, "ownerPlaneIdConfigured": ownerPlaneID != "", "revision": state.Revision})
			}
			continue
		}
		gateway, planeProjectID, ok := r.resolvePlaneDoc(&cfg, loop.ProjectID)
		if !ok || gateway == nil {
			continue
		}
		workItemID := planedoc.WorkItemIDFromURL(state.IssueURL)
		if workItemID == "" {
			continue
		}
		specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.TechSpecLinkTitle)
		if err != nil || !found {
			continue
		}
		pageID := planedoc.PageIDFromURL(specURL)
		contentMatches, err := specApprovalContentMatches(ctx, gateway, planeProjectID, pageID, state.ContentHash)
		if err != nil {
			continue
		}
		if !contentMatches {
			// The approval request is revision-bound. A page edit after REVIEW means the
			// reviewed artifact no longer exists, so silently polling forever is both
			// confusing and unsafe. Leave one visible, idempotent explanation on Plane,
			// then fail the planner loop with an explicit re-plan/re-review requirement.
			marker := fmt.Sprintf("<!-- looper:spec-approval-invalidated loop=%s revision=%d hash=%s -->", loop.ID, state.Revision, state.ContentHash)
			pageComments, listErr := gateway.ListPageComments(ctx, planeProjectID, pageID)
			if listErr != nil {
				continue
			}
			commentExists := false
			for _, comment := range pageComments {
				if strings.Contains(comment.CommentHTML, marker) {
					commentExists = true
					break
				}
			}
			if !commentExists {
				body := planedoc.SignComment(marker+"<p>⚠️ 技术方案在 GRILL + REVIEW 后发生了变更，当前审批请求已失效。请重新启动 planner，完成新一轮 GRILL + REVIEW 后再审批。</p>", "reviewer", "")
				if err := gateway.CommentOnPageURL(ctx, planeProjectID, specURL, body); err != nil {
					continue
				}
			}
			if err := invalidateSpecApproval(ctx, repositories, loop.ID, nowISO, "Plane spec page changed after GRILL + REVIEW"); err != nil {
				continue
			}
			if logger != nil {
				logger.Warn("spec approval: Plane page changed after review; approval invalidated", map[string]any{"loopId": loop.ID, "revision": state.Revision})
			}
			continue
		}
		comments, err := gateway.ListHumanSpecComments(ctx, planeProjectID, specURL)
		if err != nil {
			continue
		}
		comments = eligibleSpecApprovalComments(comments, ownerPlaneID, state.RequestedAt, state.RequestCommentID)
		if len(comments) == 0 {
			continue // no human reply yet — nothing to judge, no LLM call
		}
		// Coalesce: judge only when the human-comment set changed since the last
		// judgment (a burst that piled up during a long run drains as ONE aggregated
		// request on the next tick). hashSpecComments is the cursor.
		hash := hashSpecComments(comments)
		if hash == state.JudgedHash {
			continue
		}
		workingDir := projectRepoPath(cfg, loop.ProjectID)
		if workingDir == "" {
			continue
		}
		// Pass the human comments through to the LLM verbatim and let it decide whether
		// any is a real, unconditional go-ahead (negation/conditional/multilingual are
		// the model's job, not a keyword table).
		var verdict specApprovalVerdict
		if judgeOverride != nil {
			verdict, err = judgeOverride(ctx, comments, workingDir)
		} else {
			if specJudge == nil {
				specJudge = buildSpecApprovalLLM(cfg, repositories, now)
			}
			if specJudge == nil {
				continue
			}
			verdict, err = judgeSpecApproval(ctx, specJudge, comments, workingDir)
		}
		if err != nil {
			if logger != nil {
				logger.Warn("spec approval: llm judge failed (retry next tick)", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue // safe fallback: never dispatch on a failed/uncertain judgment
		}
		if !verdict.Approved {
			// A negative verdict has no downstream side effects, so it is safe to
			// coalesce this exact comment set. Approved verdicts intentionally do not
			// advance the cursor until dispatch completes; transient label/state/audit
			// failures must retry with unchanged comments on the next tick.
			if err := setSpecApprovalJudgedHash(ctx, repositories, loop.ID, hash, nowISO); err != nil && logger != nil {
				logger.Warn("spec approval: persist judge cursor failed (continuing)", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			if logger != nil {
				logger.Info("spec not yet approved (node H waiting)", map[string]any{"loopId": loop.ID, "reason": verdict.Reason})
			}
			continue
		}
		// The judge may have taken minutes. Re-read the authority-bearing page at
		// the last possible point before the first dispatch side effect so an edit
		// during judgment cannot ship content that never passed REVIEW.
		contentMatches, err = specApprovalContentMatches(ctx, gateway, planeProjectID, pageID, state.ContentHash)
		if err != nil {
			continue
		}
		if !contentMatches {
			marker := fmt.Sprintf("<!-- looper:spec-approval-invalidated loop=%s revision=%d hash=%s -->", loop.ID, state.Revision, state.ContentHash)
			pageComments, listErr := gateway.ListPageComments(ctx, planeProjectID, pageID)
			if listErr != nil {
				continue
			}
			commentExists := false
			for _, comment := range pageComments {
				if strings.Contains(comment.CommentHTML, marker) {
					commentExists = true
					break
				}
			}
			if !commentExists {
				body := planedoc.SignComment(marker+"<p>⚠️ 技术方案在 GRILL + REVIEW 后发生了变更，当前审批请求已失效。请重新启动 planner，完成新一轮 GRILL + REVIEW 后再审批。</p>", "reviewer", "")
				if err := gateway.CommentOnPageURL(ctx, planeProjectID, specURL, body); err != nil {
					continue
				}
			}
			if err := invalidateSpecApproval(ctx, repositories, loop.ID, nowISO, "Plane spec page changed during approval judgment"); err != nil {
				continue
			}
			if logger != nil {
				logger.Warn("spec approval: Plane page changed during judgment; approval invalidated", map[string]any{"loopId": loop.ID, "revision": state.Revision})
			}
			continue
		}
		by := ownerPlaneID
		// Strict dispatches use the Plane-authoritative, revision-bound handoff. Legacy
		// projects retain the worker-ready label bridge during the migration window.
		if state.StrictDispatchID != "" {
			client, ok, clientErr := planeClientForCWD(&cfg, workingDir)
			if clientErr != nil || !ok {
				if logger != nil {
					logger.Warn("spec approval: strict Plane client unavailable", map[string]any{"loopId": loop.ID, "error": fmt.Sprint(clientErr)})
				}
				continue
			}
			approvalCommentID := strings.TrimSpace(comments[len(comments)-1].ID)
			if err := client.HandoffStrictDispatch(ctx, state.StrictDispatchID, planestrict.HandoffInput{
				ApprovalActorMemberID: ownerPlaneID,
				ApprovalCommentID:     approvalCommentID,
				SpecContentHash:       state.ContentHash,
				SpecRevision:          state.Revision,
			}); err != nil {
				if logger != nil {
					logger.Warn("spec approval: strict planner-to-worker handoff failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
				}
				continue
			}
		} else if err := gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, workerReadyLabel); err != nil {
			if logger != nil {
				logger.Warn("spec approval: stamp worker-ready failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		// Reflect the dispatch in Plane's own state column (node I): Todo → In Progress.
		// Do not mark the handoff complete until both label and state converge. Label
		// addition is idempotent, so a transient state failure safely retries next tick.
		if err := gateway.SetWorkItemState(ctx, planeProjectID, workItemID, "In Progress"); err != nil {
			if logger != nil {
				logger.Warn("spec approval: set In Progress state failed; retrying", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		reasonHTML := ""
		if reason := strings.TrimSpace(verdict.Reason); reason != "" {
			reasonHTML = "<br><i>looper 判定:" + html.EscapeString(reason) + "</i>"
		}
		auditMarker := fmt.Sprintf("<!-- looper:spec-approved loop=%s revision=%d hash=%s -->", loop.ID, state.Revision, state.ContentHash)
		audit := planedoc.SignComment(auditMarker+"<p>✅ 已由 "+html.EscapeString(strings.TrimSpace(by))+" 批准,进入实现(node I)。"+reasonHTML+"</p>", "reviewer", "")
		pageComments, listErr := gateway.ListPageComments(ctx, planeProjectID, planedoc.PageIDFromURL(specURL))
		if listErr != nil {
			continue
		}
		auditExists := false
		for _, comment := range pageComments {
			if strings.Contains(comment.CommentHTML, auditMarker) {
				auditExists = true
				break
			}
		}
		if !auditExists {
			if err := gateway.CommentOnPageURL(ctx, planeProjectID, specURL, audit); err != nil {
				if logger != nil {
					logger.Warn("spec approval: audit comment failed; retrying", map[string]any{"loopId": loop.ID, "error": err.Error()})
				}
				continue
			}
		}
		if err := markSpecApprovalDispatched(ctx, repositories, loop.ID, nowISO); err != nil {
			if logger != nil {
				logger.Warn("spec approval: mark dispatched failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if logger != nil {
			logger.Info("spec approved — dispatched worker", map[string]any{"loopId": loop.ID, "workItem": workItemID, "approvedBy": by})
		}
	}
}

func specApprovalContentMatches(ctx context.Context, gateway *planedoc.Gateway, planeProjectID, pageID, expectedHash string) (bool, error) {
	pageContent, err := gateway.PageContent(ctx, planeProjectID, pageID)
	if err != nil {
		return false, err
	}
	pageSum := sha256.Sum256([]byte(pageContent))
	return hex.EncodeToString(pageSum[:]) == strings.TrimSpace(expectedHash), nil
}

func latestPlannerIssueURL(ctx context.Context, repositories *storage.Repositories, loopID string) string {
	if repositories == nil || repositories.Runs == nil || strings.TrimSpace(loopID) == "" {
		return ""
	}
	run, err := repositories.Runs.GetLatestByLoopID(ctx, loopID)
	if err != nil || run == nil || run.CheckpointJSON == nil {
		return ""
	}
	return plannerIssueURLFromCheckpoint(run.CheckpointJSON)
}

func plannerIssueURLFromCheckpoint(checkpointJSON *string) string {
	if checkpointJSON == nil || strings.TrimSpace(*checkpointJSON) == "" {
		return ""
	}
	var checkpoint struct {
		Issue *struct {
			URL string `json:"url"`
		} `json:"issue"`
	}
	if json.Unmarshal([]byte(*checkpointJSON), &checkpoint) != nil || checkpoint.Issue == nil {
		return ""
	}
	return strings.TrimSpace(checkpoint.Issue.URL)
}

// eligibleSpecApprovalComments enforces both authority and time boundaries. Only the
// configured local owner can approve, and only with a comment created after the
// current revision's request. Old approvals and wrong-role replies are ignored.
func eligibleSpecApprovalComments(comments []planedoc.PageComment, ownerPlaneID, requestedAt, requestCommentID string) []planedoc.PageComment {
	ownerPlaneID = strings.TrimSpace(ownerPlaneID)
	boundary, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(requestedAt))
	if ownerPlaneID == "" || err != nil {
		return nil
	}
	out := make([]planedoc.PageComment, 0, len(comments))
	for _, comment := range comments {
		if strings.TrimSpace(comment.Actor) != ownerPlaneID || strings.TrimSpace(comment.ID) == strings.TrimSpace(requestCommentID) {
			continue
		}
		createdAt, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(comment.CreatedAt))
		if parseErr != nil || !createdAt.After(boundary) {
			continue
		}
		out = append(out, comment)
	}
	return out
}

// markSpecApprovalDispatched flips the loop's specApprovedDispatched metadata flag so
// the reconcile hands off to the worker exactly once.
func markSpecApprovalDispatched(ctx context.Context, repos *storage.Repositories, loopID, nowISO string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	meta := map[string]any{}
	if loop.MetadataJSON != nil && strings.TrimSpace(*loop.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &meta)
	}
	meta["specApprovedDispatched"] = true
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	updated := *loop
	s := string(encoded)
	updated.MetadataJSON = &s
	updated.UpdatedAt = nowISO
	return repos.Loops.Upsert(ctx, updated)
}

// invalidateSpecApproval makes a post-REVIEW page edit visible and fail-closed.
// The loop remains inspectable with a durable reason, but is no longer eligible for
// either approval polling or worker dispatch until a fresh planner run reviews the
// new content and opens a new revision-bound gate.
func invalidateSpecApproval(ctx context.Context, repos *storage.Repositories, loopID, nowISO, reason string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	meta := map[string]any{}
	if loop.MetadataJSON != nil && strings.TrimSpace(*loop.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &meta)
	}
	meta["awaitingSpecApproval"] = false
	meta["specApprovalInvalidated"] = true
	meta["specApprovalInvalidReason"] = strings.TrimSpace(reason)
	meta["nodeHPhase"] = "approval_invalidated"
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	updated := *loop
	updated.Status = "failed"
	value := string(encoded)
	updated.MetadataJSON = &value
	updated.UpdatedAt = nowISO
	return repos.Loops.Upsert(ctx, updated)
}

// setSpecApprovalJudgedHash records the fingerprint of the human comment set just sent
// to the LLM judge, so an unchanged set is skipped (no repeat agent run) next tick.
func setSpecApprovalJudgedHash(ctx context.Context, repos *storage.Repositories, loopID, hash, nowISO string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	meta := map[string]any{}
	if loop.MetadataJSON != nil && strings.TrimSpace(*loop.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &meta)
	}
	meta["specApprovalJudgedHash"] = hash
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	updated := *loop
	s := string(encoded)
	updated.MetadataJSON = &s
	updated.UpdatedAt = nowISO
	return repos.Loops.Upsert(ctx, updated)
}

// specApprovalVerdict is the LLM judge's structured answer for the node H gate.
type specApprovalVerdict struct {
	Approved bool   `json:"approved"`
	By       string `json:"by"`
	Reason   string `json:"reason"`
}

// hashSpecComments fingerprints a human comment set (id + text, in order) so the
// reconciler can tell whether anything changed since the last judgment.
func hashSpecComments(comments []planedoc.PageComment) string {
	var b strings.Builder
	for _, c := range comments {
		text := strings.TrimSpace(c.CommentStripped)
		if text == "" {
			text = strings.TrimSpace(c.CommentHTML)
		}
		b.WriteString(c.ID)
		b.WriteByte(0)
		b.WriteString(text)
		b.WriteByte(0x1e)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// projectRepoPath returns the local clone path of a configured project (the working
// directory the judge agent runs in), or "" if the project is unknown.
func projectRepoPath(cfg config.Config, projectID string) string {
	for _, p := range cfg.Projects {
		if p.ID == projectID {
			return strings.TrimSpace(p.RepoPath)
		}
	}
	return ""
}

// buildSpecApprovalLLM constructs the agent-backed LLM used to judge spec approval,
// mirroring the auto-intake triage setup. Returns nil when no agent vendor is set.
func buildSpecApprovalLLM(cfg config.Config, repos *storage.Repositories, now func() time.Time) triage.LLM {
	if cfg.Agent.Vendor == nil {
		return nil
	}
	executor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor:              *cfg.Agent.Vendor,
			Model:               cfg.Agent.Model,
			Params:              cfg.Agent.Params,
			Env:                 cfg.Agent.Env,
			NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
		},
		Repos:  repos,
		LogDir: cfg.Daemon.LogDir,
		Now:    now,
	})
	return coordinatorrole.NewAgentLLM(executor, now,
		time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds)*time.Second,
		time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds)*time.Second,
	)
}

// judgeSpecApproval asks the LLM whether the human comments constitute a real,
// unconditional approval of the tech spec (node H). Comments are passed through
// verbatim; the model handles negation / conditional / multilingual phrasing.
func judgeSpecApproval(ctx context.Context, llm triage.LLM, comments []planedoc.PageComment, workingDir string) (specApprovalVerdict, error) {
	raw, err := llm.Complete(ctx, triage.Request{Prompt: buildSpecApprovalPrompt(comments), WorkingDirectory: workingDir})
	if err != nil {
		return specApprovalVerdict{}, err
	}
	return parseSpecApprovalVerdict(raw)
}

// buildSpecApprovalPrompt renders the human comments and the judgment instructions.
func buildSpecApprovalPrompt(comments []planedoc.PageComment) string {
	var b strings.Builder
	b.WriteString("You are the approval gate (node H) for a software technical spec written on a Plane page.\n")
	b.WriteString("Below are the human review comments on that spec page, in order. Decide whether a human has given an EXPLICIT, UNCONDITIONAL approval to START implementing the spec.\n\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Approve ONLY on a clear go-ahead (e.g. \"approved\", \"LGTM, go ahead\", \"同意开工\", \"可以实现了\").\n")
	b.WriteString("- Do NOT approve on: rejections/negations (\"not approved\", \"不同意\"), questions, praise with no go-ahead, or CONDITIONAL / deferred approval (\"approve after you fix X\", \"LGTM once CI is green\", \"改完再说\").\n")
	b.WriteString("- Judge the NET current state of the conversation — a later comment can grant or retract approval.\n")
	b.WriteString("- The comments are the only source of truth; do not consider anything else in the repo.\n\n")
	b.WriteString("Comments:\n")
	for i, c := range comments {
		text := strings.TrimSpace(c.CommentStripped)
		if text == "" {
			text = strings.TrimSpace(c.CommentHTML)
		}
		name := strings.TrimSpace(c.DisplayName)
		if name == "" {
			name = "unknown"
		}
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, name, text)
	}
	b.WriteString("\nReply with ONLY a JSON object — no prose, no code fence:\n")
	b.WriteString(`{"approved": <true|false>, "by": "<display name of the approver, or empty>", "reason": "<one short sentence>"}`)
	b.WriteString("\n")
	return b.String()
}

// parseSpecApprovalVerdict extracts the JSON verdict from the agent's output, which may
// be wrapped in prose or a code fence. A missing/malformed object is an error so the
// caller keeps waiting rather than dispatching on an unparseable answer.
func parseSpecApprovalVerdict(raw string) (specApprovalVerdict, error) {
	s := strings.TrimSpace(raw)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return specApprovalVerdict{}, fmt.Errorf("spec approval: no JSON object in judge output: %.160q", s)
	}
	var v specApprovalVerdict
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return specApprovalVerdict{}, fmt.Errorf("spec approval: parse judge output: %w", err)
	}
	v.By = strings.TrimSpace(v.By)
	v.Reason = strings.TrimSpace(v.Reason)
	return v, nil
}

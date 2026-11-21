package planner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/planner/decisions"
	"github.com/nexu-io/looper/internal/renderer/statichtml"
	"github.com/nexu-io/looper/internal/storage"
)

func (r *Runner) runAuthorDecisionBriefStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" || checkpoint.PipelineVersion < 2 || checkpoint.Decisions != nil {
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	baselineHead, err := r.requirementWorktreeHead(ctx, input.Project, *worktree)
	if err != nil {
		return checkpoint, err
	}
	prompt := buildDecisionAuthorPrompt(*issue)
	result, err := r.runPlannerAgent(ctx, input, worktree.Path, prompt, "requirement-author")
	if err != nil {
		return checkpoint, err
	}
	if err := r.assertRequirementAgentDidNotEditBusinessRepo(ctx, input.Project, *worktree, baselineHead); err != nil {
		return checkpoint, err
	}
	brief, err := decisionBriefFromAgentResult(result)
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	brief.Revision = 1
	checkpoint.Decisions = &decisions.State{Brief: brief, Stage: "authored", Requests: map[decisions.Role]decisions.RequestReceipt{}, Answers: map[string]decisions.Answer{}}
	checkpoint.Phase = "requirement_grill_product"
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runRequirementGrillStep(ctx context.Context, input stepInput, stage string) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" || checkpoint.PipelineVersion < 2 {
		return checkpoint, nil
	}
	if checkpoint.Decisions == nil {
		return checkpoint, &loopError{message: "requirement grill: decision brief missing", kind: FailureNonRetryable}
	}
	wantedStage := "grilled_" + stage
	if checkpoint.Decisions.Stage == wantedStage {
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	baselineHead, err := r.requirementWorktreeHead(ctx, input.Project, *worktree)
	if err != nil {
		return checkpoint, err
	}
	prompt := buildRequirementGrillPrompt(*issue, *checkpoint.Decisions, stage)
	result, err := r.runPlannerAgent(ctx, input, worktree.Path, prompt, "requirement-grill-"+stage)
	if err != nil {
		return checkpoint, err
	}
	if err := r.assertRequirementAgentDidNotEditBusinessRepo(ctx, input.Project, *worktree, baselineHead); err != nil {
		return checkpoint, err
	}
	brief, err := decisionBriefFromAgentResult(result)
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	brief.Revision = checkpoint.Decisions.Brief.Revision + 1
	if stage == "final" {
		if len(brief.Questions) != 0 {
			return checkpoint, &loopError{message: "final requirement grill must return questions=[]; unresolved question " + brief.Questions[0].ID, kind: FailureManualIntervention}
		}
	}
	checkpoint.Decisions.Brief = brief
	checkpoint.Decisions.Stage = wantedStage
	if stage == "final" {
		checkpoint.Decisions.ReopenReason = ""
	}
	checkpoint.Phase = wantedStage
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runRouteProductDecisionsStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.PipelineVersion < 2 || checkpoint.Decisions == nil {
		return checkpoint, nil
	}
	state := checkpoint.Decisions
	if state.Stage == "product_resolved" || state.Stage == "awaiting_product" || state.Stage == "awaiting_product_spec" {
		return checkpoint, nil
	}
	if state.Brief.FormalProductSpec.Required {
		if err := r.ensureFormalProductSpecRequest(ctx, input, state); err != nil {
			return checkpoint, err
		}
		state.Stage = "awaiting_product_spec"
		checkpoint.Phase = state.Stage
		checkpoint.Wait = &checkpointPlannerWait{Reason: "等待产品负责人在 Plane work item 关联正式产品 Spec", ResumeStep: stepGrillDownstreamDecisions}
		return checkpoint, nil
	}
	questions := decisions.QuestionsForRole(state.Brief, decisions.RoleProduct)
	if len(questions) == 0 {
		state.Stage = "product_resolved"
		checkpoint.Phase = state.Stage
		return checkpoint, nil
	}
	if err := r.ensureDecisionRequest(ctx, input, state, decisions.RoleProduct, questions); err != nil {
		return checkpoint, err
	}
	state.Stage = "awaiting_product"
	checkpoint.Phase = state.Stage
	checkpoint.Wait = &checkpointPlannerWait{Reason: "等待产品负责人在 Plane work item 回答产品决策", ResumeStep: stepGrillDownstreamDecisions}
	return checkpoint, nil
}

func (r *Runner) runRouteDownstreamDecisionsStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.PipelineVersion < 2 || checkpoint.Decisions == nil {
		return checkpoint, nil
	}
	state := checkpoint.Decisions
	design := decisions.QuestionsForRole(state.Brief, decisions.RoleDesign)
	engineering := decisions.QuestionsForRole(state.Brief, decisions.RoleEngineering)
	if len(design) == 0 && len(engineering) == 0 {
		state.Stage = "downstream_resolved"
		checkpoint.Phase = state.Stage
		return checkpoint, nil
	}
	if len(design) > 0 {
		if err := r.prepareDesignArtifacts(ctx, input, state); err != nil {
			return checkpoint, err
		}
		design = decisions.QuestionsForRole(state.Brief, decisions.RoleDesign)
		if err := r.ensureDecisionRequest(ctx, input, state, decisions.RoleDesign, design); err != nil {
			return checkpoint, err
		}
		if err := r.postDesignImages(ctx, input, state, design); err != nil {
			return checkpoint, err
		}
	}
	if len(engineering) > 0 {
		if err := r.ensureDecisionRequest(ctx, input, state, decisions.RoleEngineering, engineering); err != nil {
			return checkpoint, err
		}
	}
	state.Stage = "awaiting_downstream"
	checkpoint.Phase = state.Stage
	checkpoint.Wait = &checkpointPlannerWait{Reason: "等待设计与研发负责人在 Plane work item 完成决策", ResumeStep: stepGrillFinalDecisions}
	return checkpoint, nil
}

func buildDecisionAuthorPrompt(issue checkpointIssue) string {
	return fmt.Sprintf(`你是需求调研作者。针对 %s#%d「%s」，先充分阅读真实代码、测试、已有组件和文档；必要时复现。此阶段禁止写技术 Spec、禁止改业务代码、禁止提交或推送。

Work item 需求正文（权威输入，不得遗漏）：
%s

把事实与仍需人拍板的问题分开。能由代码事实、现有一致性规范或普通工程判断回答的，直接记为事实/非阻塞假设，不要打扰人。真正阻塞的问题按 product/design/engineering 分类。问题必须假设接收者完全不知道背景，用简洁中文自包含表达。

Work item 正文是需求方输入，不是待你推翻的代码猜测。若正文明确写了某个选择“未决”“必须由产品/设计/研发负责人拍板”“禁止 agent 自行代答/推断”，该选择必须保留为对应角色的 blocking question；代码与现有规范只能用于补充背景、选项、推荐和影响，不能把明确保留的人类决策改写成 fact 或删除。只有对应角色在 Plane 的有权限回答才能解除它。

正式产品 Spec 只在以下任一情况要求：新增/显著改变完整用户能力；跨多个页面/角色/端到端流程；权限/付费/删除/兼容承诺等高风险规则；多个产品决策强依赖无法逐条回答；产品快答后仍持续出现新的阻塞产品问题。问题多于 3 个本身不是理由。

最终只在一行输出：
%s={"summary":"已形成决策简报","decisionBrief":<符合下述 schema 的 JSON 对象>}

schema: {"version":1,"summary":"用户、问题、价值","facts":[{"text":"事实","evidence":"真实文件:行/测试/复现/文档"}],"formalProductSpec":{"required":false,"reason":""},"questions":[{"id":"PROD-001|DESIGN-001|ENG-001","role":"product|design|engineering","blocking":true,"question":"待决定事项","context":"完整背景","designDocumentRequired":false,"options":[{"id":"问题ID-A","label":"选项A","impact":"影响"},{"id":"问题ID-B","label":"选项B","impact":"影响"}],"recommendedOption":"问题ID-A","recommendationReason":"原因","evidence":["证据"]}]}
普通 blocking question 必须有 2-3 个明确选项并给推荐。新页面、多步骤流程或信息架构改变必须输出 role=design、designDocumentRequired=true、options=[] 的阻塞问题，要求设计负责人提供设计稿/设计文档 URL，不得压缩成临时 HTML 方案。没有真实阻塞问题时 questions=[]。`, issue.Repo, issue.IssueNumber, issue.Title, strings.TrimSpace(issue.Body), agent.CompletionMarker)
}

func buildRequirementGrillPrompt(issue checkpointIssue, state decisions.State, stage string) string {
	encoded, _ := json.Marshal(state)
	stageRule := "只冻结产品边界：删除可由事实回答的问题；保留真正的产品阻塞项。设计/研发问题可先记录，但本轮不会发送。"
	if stage == "downstream" {
		stageRule = "产品边界已经冻结。结合正式产品 Spec/产品回答重新调研，只输出仍未解决的设计与研发阻塞问题；不得重新发明产品范围。局部视觉/交互取舍的 DESIGN 问题必须给 2-3 个选项，每个 option 在 html 字段内提供完整、自包含、无脚本、无图片/字体/外链、只用 inline/style CSS 的最小静态 HTML，用相同 viewport 和示例数据直观展示差异。新页面、多步骤流程或信息架构改变不得用临时 HTML 代替设计稿：输出 designDocumentRequired=true、options=[] 的 DESIGN 阻塞问题，要求设计负责人在 Plane 回答设计稿/设计文档 URL。"
	} else if stage == "final" {
		stageRule = "所有角色已经回答。用答案再次对抗检查需求是否完整；把答案转成 facts，并删除已经解决的问题。若没有新的真实矛盾，questions 必须是 []。若答案矛盾或不足，才保留 blocking 问题让流程 fail closed；不得用非阻塞问题填充 questions。"
	}
	return fmt.Sprintf(`你是一个全新的、没有参与上一轮写作的对抗式 requirement GRILL agent。针对 %s#%d「%s」，核对真实代码、测试和已有规范，挑战下面的决策简报。禁止写技术 Spec、禁止改业务代码、禁止提交或推送。

Work item 需求正文（权威输入）：
%s

本轮规则：%s

ID 是机器协议，不是可改写的文案：已有问题必须原样保留其 ID；新问题只允许使用与角色对应的 PROD-001 / DESIGN-001 / ENG-001 这类“大写角色前缀 + 连字符 + 三位数字”ID，并避开已有编号。严禁 q_*、snake_case、无角色前缀或省略连字符。每个 blocking question 必须有 2-3 个明确选项与推荐；选项 ID 必须是完整问题 ID 加单个安全后缀（例如 DESIGN-001-A），禁止斜杠、点号或路径片段。已由回答解决的问题应转成 facts 并从 questions 删除。

Work item 的显式决策要求具有需求权威：如果正文明确说某个选择未决、必须由指定角色拍板或禁止 agent 代答，你不得用代码现状、通用规范或个人推荐删除它。只有当前状态中带 actor/comment 审计信息的对应角色 Plane 回答，才能把它转成 fact。没有这种回答就必须保留原 blocking question。

当前状态（包含已验证回答的 actor/comment 审计信息）：
%s

问题必须短、通俗、自包含；接收者无需读代码。禁止为了显得完整而虚构问题。最终只在一行输出：
%s={"summary":"requirement grill 完成","decisionBrief":<完整的 version=1 JSON 对象>}`, issue.Repo, issue.IssueNumber, issue.Title, strings.TrimSpace(issue.Body), stageRule, string(encoded), agent.CompletionMarker)
}

func decisionBriefFromAgentResult(result AgentResult) (decisions.Brief, error) {
	if !strings.EqualFold(result.Status, "completed") {
		return decisions.Brief{}, fmt.Errorf("decision brief agent did not complete: %s", firstNonEmpty(result.Summary, result.Stderr, result.Status))
	}
	type completion struct {
		DecisionBrief json.RawMessage `json:"decisionBrief"`
	}
	for _, stream := range []string{result.Stdout, result.Stderr, result.Summary} {
		lines := decisionCompletionLines(stream)
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			prefix := agent.CompletionMarkerPrefix
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			var wrapper completion
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &wrapper); err != nil || len(wrapper.DecisionBrief) == 0 {
				continue
			}
			var brief decisions.Brief
			if err := json.Unmarshal(wrapper.DecisionBrief, &brief); err != nil {
				return decisions.Brief{}, fmt.Errorf("decode decision brief: %w", err)
			}
			if err := decisions.ValidateBrief(brief); err != nil {
				return decisions.Brief{}, err
			}
			return brief, nil
		}
	}
	return decisions.Brief{}, fmt.Errorf("agent output is missing structured decisionBrief")
}

func decisionCompletionLines(stream string) []string {
	rawLines := strings.Split(strings.ReplaceAll(stream, "\r\n", "\n"), "\n")
	trusted := make([]string, 0, len(rawLines))
	jsonlStream := false
	for _, raw := range rawLines {
		var event map[string]any
		if json.Unmarshal([]byte(raw), &event) != nil {
			continue
		}
		texts, recognized := trustedDecisionCompletionEvent(event)
		if recognized {
			jsonlStream = true
			for _, value := range texts {
				trusted = append(trusted, strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")...)
			}
		}
	}
	if jsonlStream {
		return trusted
	}
	// Non-JSONL executors emit the assistant's final text directly. Preserve that
	// legacy protocol, but never mix raw lines into a recognized JSONL event stream:
	// command/tool output in those events is untrusted repository-controlled data.
	return rawLines
}

// trustedDecisionCompletionEvent extracts only assistant-authored fields from the
// three supported JSONL protocols. It deliberately recognizes tool-only events too
// (returning no text) so a marker echoed from a repository file or command output
// can never be reinterpreted as Looper's structured decision authority.
func trustedDecisionCompletionEvent(event map[string]any) ([]string, bool) {
	typ, _ := event["type"].(string)
	switch typ {
	case "thread.started", "item.started", "turn.completed", "turn.failed", "error": // Codex envelopes without trusted text.
		return nil, true
	case "item.completed":
		item, _ := event["item"].(map[string]any)
		if item == nil {
			return nil, true
		}
		if itemType, _ := item["type"].(string); itemType == "agent_message" {
			if text := firstStringField(item, "text", "message"); text != "" {
				return []string{text}, true
			}
		}
		return nil, true
	case "agent_message":
		if text := firstStringField(event, "message", "text"); text != "" {
			return []string{text}, true
		}
		return nil, true
	case "step_start", "text", "tool_use", "step_finish": // OpenCode.
		part, _ := event["part"].(map[string]any)
		if part != nil {
			if partType, _ := part["type"].(string); partType == "text" {
				if text, _ := part["text"].(string); text != "" {
					return []string{text}, true
				}
			}
		}
		return nil, true
	case "system", "user": // Claude stream envelopes without assistant authority.
		if _, ok := event["session_id"]; ok {
			return nil, true
		}
	case "assistant": // Claude assistant text blocks; tool_use blocks are excluded.
		message, _ := event["message"].(map[string]any)
		content, _ := message["content"].([]any)
		texts := make([]string, 0, len(content))
		for _, value := range content {
			block, _ := value.(map[string]any)
			if blockType, _ := block["type"].(string); blockType == "text" {
				if text, _ := block["text"].(string); text != "" {
					texts = append(texts, text)
				}
			}
		}
		return texts, true
	case "result": // Claude's terminal, assistant-authored result.
		if text, _ := event["result"].(string); text != "" {
			return []string{text}, true
		}
		return nil, true
	}
	return nil, false
}

func firstStringField(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, _ := value[key].(string); text != "" {
			return text
		}
	}
	return ""
}

func (r *Runner) requirementWorktreeHead(ctx context.Context, project storage.ProjectRecord, worktree checkpointWorktree) (string, error) {
	if r.git == nil {
		return "", &loopError{message: "requirement-agent read-only guard requires git gateway", kind: FailureNonRetryable}
	}
	root, err := plannerWorktreeRoot(project)
	if err != nil {
		return "", err
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: root, WorktreePath: worktree.Path, BaseRef: worktree.BaseBranch})
	if err != nil {
		return "", err
	}
	if inspect.HasUncommittedChanges {
		return "", &loopError{message: "requirement worktree was dirty before the read-only agent started", kind: FailureManualIntervention}
	}
	return inspect.HeadSHA, nil
}

func (r *Runner) assertRequirementAgentDidNotEditBusinessRepo(ctx context.Context, project storage.ProjectRecord, worktree checkpointWorktree, baselineHead string) error {
	head, err := r.requirementWorktreeHead(ctx, project, worktree)
	if err != nil {
		return err
	}
	if head != baselineHead {
		return &loopError{message: "requirement agent modified the business worktree; refusing to continue before technical spec", kind: FailureManualIntervention}
	}
	return nil
}

func (r *Runner) ensureFormalProductSpecRequest(ctx context.Context, input stepInput, state *decisions.State) error {
	questions := []decisions.Question{{ID: "PROD-000", Role: decisions.RoleProduct, Blocking: true, Question: "请提供正式产品 Spec", Context: state.Brief.FormalProductSpec.Reason}}
	return r.ensureDecisionRequest(ctx, input, state, decisions.RoleProduct, questions)
}

func (r *Runner) ensureDecisionRequest(ctx context.Context, input stepInput, state *decisions.State, role decisions.Role, questions []decisions.Question) error {
	if state.Requests == nil {
		state.Requests = map[decisions.Role]decisions.RequestReceipt{}
	}
	if state.RequestedQuestions == nil {
		state.RequestedQuestions = map[decisions.Role][]decisions.Question{}
	}
	state.RequestedQuestions[role] = append([]decisions.Question(nil), questions...)
	if receipt, ok := state.Requests[role]; ok && receipt.Revision == state.Brief.Revision && receipt.CommentID != "" {
		return nil
	}
	strictDispatchID := strings.TrimSpace(stringFromAnyDefault(parseJSONObject(input.QueueItem.PayloadJSON)["strictDispatchId"]))
	if strictDispatchID != "" {
		gateway, ok := r.github.(strictDispatchGateway)
		if !ok {
			return &loopError{message: "strict role request gateway is unavailable", kind: FailureNonRetryable}
		}
		created, err := gateway.CreateStrictRoleRequest(ctx, StrictRoleRequestInput{
			Repo: derefString(input.Loop.Repo), CWD: input.Project.RepoPath,
			DispatchID: strictDispatchID, LoopID: input.Loop.ID, DecisionRevision: state.Brief.Revision,
			Role: role, BriefSummary: state.Brief.Summary, Questions: questions,
		})
		if err != nil {
			return &loopError{message: "create strict decision request: " + err.Error(), kind: FailureRetryableTransient}
		}
		if strings.TrimSpace(created.RoleRequestID) == "" || strings.TrimSpace(created.CommentID) == "" || strings.TrimSpace(created.CreatedAt) == "" || strings.TrimSpace(created.EligibleMemberID) == "" {
			return &loopError{message: "strict decision request receipt was incomplete", kind: FailureNonRetryable}
		}
		state.Requests[role] = decisions.RequestReceipt{
			Role: role, Revision: state.Brief.Revision, RoleRequestID: created.RoleRequestID, CommentID: created.CommentID,
			CreatedAt: created.CreatedAt, EligibleMemberID: created.EligibleMemberID,
		}
		issue, issueErr := requireIssue(input.Checkpoint)
		if issueErr != nil {
			return issueErr
		}
		return r.notifyDecisionRoleToMember(ctx, input, role, state.Brief.Revision, questions, issue.URL, created.EligibleMemberID)
	}
	if r.planeDoc == nil {
		return &loopError{message: "decision request requires Plane", kind: FailureNonRetryable}
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return &loopError{message: "decision request Plane gateway unavailable", kind: FailureRetryableTransient}
	}
	issue, err := requireIssue(input.Checkpoint)
	if err != nil {
		return err
	}
	workItemID := planedoc.WorkItemIDFromURL(issue.URL)
	if workItemID == "" {
		return &loopError{message: "decision request work item id unavailable", kind: FailureNonRetryable}
	}
	actor := decisionActor(r.projectRoleConfig, input.Project.ID, role)
	if strings.TrimSpace(actor.PlaneID) == "" {
		return &loopError{message: fmt.Sprintf("%s 决策缺少 planeId 配置；请在 projects[%s] 对应负责人中补充", role, input.Project.ID), kind: FailureManualIntervention}
	}
	marker := fmt.Sprintf("<!-- looper:decision-request v=1 loop=%s role=%s revision=%d -->", input.Loop.ID, role, state.Brief.Revision)
	existing, err := gateway.ListWorkItemComments(ctx, planeProjectID, workItemID)
	if err != nil {
		return &loopError{message: "list decision comments: " + err.Error(), kind: FailureRetryableTransient}
	}
	for _, comment := range existing {
		if strings.Contains(comment.CommentHTML, marker) {
			state.Requests[role] = decisions.RequestReceipt{Role: role, Revision: state.Brief.Revision, CommentID: comment.ID, CreatedAt: comment.CreatedAt}
			return r.notifyDecisionRole(ctx, input, role, state.Brief.Revision, questions, issue.URL)
		}
	}
	body := renderDecisionRequest(marker, state.Brief.Summary, role, questions)
	created, err := gateway.CreateWorkItemComment(ctx, planeProjectID, workItemID, planedoc.SignComment(body, "requirement-router", derefString(r.agentModel)))
	if err != nil {
		return &loopError{message: "create decision request: " + err.Error(), kind: FailureRetryableTransient}
	}
	if strings.TrimSpace(created.ID) == "" || strings.TrimSpace(created.CreatedAt) == "" {
		return &loopError{message: "Plane decision request receipt lacked id/created_at", kind: FailureNonRetryable}
	}
	state.Requests[role] = decisions.RequestReceipt{Role: role, Revision: state.Brief.Revision, CommentID: created.ID, CreatedAt: created.CreatedAt}
	return r.notifyDecisionRole(ctx, input, role, state.Brief.Revision, questions, issue.URL)
}

func decisionActor(cfg *config.Config, projectID string, role decisions.Role) config.FeishuActorConfig {
	if cfg == nil {
		return config.FeishuActorConfig{}
	}
	switch role {
	case decisions.RoleProduct:
		owner := config.ProjectProductOwner(*cfg, projectID)
		return config.FeishuActorConfig{FeishuOpenID: owner.FeishuOpenID, PlaneID: owner.PlaneID}
	case decisions.RoleDesign:
		return config.ProjectDesignOwner(*cfg, projectID)
	case decisions.RoleEngineering:
		return config.ProjectOwnerActor(*cfg, projectID)
	default:
		return config.FeishuActorConfig{}
	}
}

func decisionActorForPlaneMember(cfg *config.Config, projectID, memberID string) config.FeishuActorConfig {
	memberID = strings.TrimSpace(memberID)
	if cfg == nil || memberID == "" {
		return config.FeishuActorConfig{}
	}
	actors := []config.FeishuActorConfig{
		decisionActor(cfg, projectID, decisions.RoleProduct),
		decisionActor(cfg, projectID, decisions.RoleDesign),
		decisionActor(cfg, projectID, decisions.RoleEngineering),
		config.ProjectQAActor(*cfg, projectID),
	}
	for _, actor := range actors {
		if strings.TrimSpace(actor.PlaneID) == memberID {
			return actor
		}
	}
	return config.FeishuActorConfig{PlaneID: memberID}
}

func renderDecisionRequest(marker, summary string, role decisions.Role, questions []decisions.Question) string {
	var b strings.Builder
	b.WriteString(marker)
	b.WriteString("<p><b>")
	b.WriteString(html.EscapeString(roleLabel(role)))
	b.WriteString("需要你拍板</b></p><p><b>背景：</b>")
	b.WriteString(html.EscapeString(summary))
	b.WriteString("</p>")
	for _, question := range questions {
		b.WriteString("<p><b>")
		b.WriteString(html.EscapeString(question.ID + " · " + question.Question))
		b.WriteString("</b><br>")
		b.WriteString(html.EscapeString(question.Context))
		if question.DesignDocumentRequired {
			b.WriteString("<br><b>所需产物：</b>请提供可访问的设计稿或设计文档 URL；临时 HTML 选项不足以解除该门禁。")
		}
		for _, option := range question.Options {
			b.WriteString("<br>• ")
			b.WriteString(html.EscapeString(option.ID + "：" + option.Label + " —— " + option.Impact))
		}
		if question.RecommendedOption != "" {
			b.WriteString("<br>建议：<b>")
			b.WriteString(html.EscapeString(question.RecommendedOption))
			b.WriteString("</b> —— ")
			b.WriteString(html.EscapeString(question.RecommendationReason))
		}
		b.WriteString("</p>")
	}
	formalProductSpec := len(questions) == 1 && questions[0].ID == "PROD-000"
	designDocument := false
	standardDecision := false
	for _, question := range questions {
		designDocument = designDocument || question.DesignDocumentRequired
		standardDecision = standardDecision || !question.DesignDocumentRequired
	}
	if formalProductSpec {
		b.WriteString("<p>请在 Plane 创建或打开正式产品 Spec 页面，并在当前 work item 的 Links 中以 <code>looper:product-spec</code> 作为标题关联。普通 work item 评论和飞书回复都不会解除正式 Spec 门禁；链接完成后 Looper 会自动读取并继续。</p>")
	} else {
		if standardDecision {
			b.WriteString("<p>普通选择题请逐行按 <code>问题ID: 选项ID</code> 回答；若建议选项都不合适，使用 <code>问题ID: 自定义: 清晰决定</code>。待定、稍后讨论或含多个选项都不会解除门禁。</p>")
		}
		if designDocument {
			b.WriteString("<p>设计稿门禁请按 <code>问题ID: https://设计稿或设计文档链接</code> 回答。缺失或非 HTTP(S) 链接不会解除门禁。</p>")
		}
		b.WriteString("<p>请在本 work item 新发评论；飞书回复不会被读取。</p>")
	}
	return b.String()
}

func roleLabel(role decisions.Role) string {
	switch role {
	case decisions.RoleProduct:
		return "产品负责人"
	case decisions.RoleDesign:
		return "设计负责人"
	case decisions.RoleEngineering:
		return "研发负责人（Looper owner）"
	default:
		return string(role)
	}
}

func (r *Runner) notifyDecisionRole(ctx context.Context, input stepInput, role decisions.Role, revision int, questions []decisions.Question, issueURL string) error {
	return r.notifyDecisionRoleToMember(ctx, input, role, revision, questions, issueURL, "")
}

func (r *Runner) notifyDecisionRoleToMember(ctx context.Context, input stepInput, role decisions.Role, revision int, questions []decisions.Question, issueURL, eligibleMemberID string) error {
	if r.postThreadNote == nil && r.postThreadNoteWithUUID == nil {
		return nil
	}
	actor := decisionActor(r.projectRoleConfig, input.Project.ID, role)
	if strings.TrimSpace(eligibleMemberID) != "" {
		actor = decisionActorForPlaneMember(r.projectRoleConfig, input.Project.ID, eligibleMemberID)
	}
	var mentions []string
	if actor.FeishuOpenID != "" {
		mentions = []string{actor.FeishuOpenID}
	}
	ids := make([]string, 0, len(questions))
	for _, question := range questions {
		ids = append(ids, question.ID)
	}
	sort.Strings(ids)
	displayed := ids
	suffix := ""
	if len(displayed) > 3 {
		displayed = displayed[:3]
		suffix = fmt.Sprintf("，另 %d 个见 Plane", len(ids)-3)
	}
	text := fmt.Sprintf("🙋 %s，有 %d 个需求问题需要你拍板（%s%s）。完整背景、选项和回答格式见 Plane work item；请直接在 Plane 评论回答，飞书回复不会被读取：%s", roleLabel(role), len(questions), strings.Join(displayed, "、"), suffix, issueURL)
	if len(questions) == 1 && questions[0].ID == "PROD-000" {
		text = fmt.Sprintf("🙋 %s，这个需求需要正式产品 Spec。请在 Plane 创建或打开产品方案页，并在当前 work item 的 Links 中以 looper:product-spec 作为标题关联；普通评论和飞书回复不会解除门禁，链接完成后 Looper 会自动读取并继续：%s", roleLabel(role), issueURL)
	}
	var err error
	if r.postThreadNoteWithUUID != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("decision-note:%s:%s:%d", input.Loop.ID, role, revision)))
		err = r.postThreadNoteWithUUID(ctx, input.Loop.ID, text, mentions, fmt.Sprintf("%x", sum[:16]))
	} else {
		err = r.postThreadNote(ctx, input.Loop.ID, text, mentions)
	}
	if err != nil && r.logger != nil {
		r.logger.Warn("planner: decision notification failed", map[string]any{"loopId": input.Loop.ID, "role": role, "error": err.Error()})
	}
	return err
}

func (r *Runner) prepareDesignArtifacts(ctx context.Context, input stepInput, state *decisions.State) error {
	if strings.TrimSpace(r.decisionArtifactRoot) == "" {
		return &loopError{message: "design renderer artifact root unavailable (storage.dbPath must be configured)", kind: FailureManualIntervention}
	}
	worktree, err := requireWorktree(input.Checkpoint)
	if err != nil {
		return err
	}
	artifactRoot, err := canonicalArtifactRoot(r.decisionArtifactRoot)
	if err != nil {
		return &loopError{message: "decision artifact root is unsafe: " + err.Error(), kind: FailureManualIntervention}
	}
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		return &loopError{message: "create decision artifact root: " + err.Error(), kind: FailureManualIntervention}
	}
	if err := rejectSymlinkComponentsWithin(artifactRoot, artifactRoot); err != nil {
		return &loopError{message: "decision artifact root is unsafe: " + err.Error(), kind: FailureManualIntervention}
	}
	worktreePath, err := filepath.EvalSymlinks(worktree.Path)
	if err != nil {
		return &loopError{message: "resolve business worktree: " + err.Error(), kind: FailureManualIntervention}
	}
	worktreePath, _ = filepath.Abs(worktreePath)
	if rel, relErr := filepath.Rel(worktreePath, artifactRoot); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &loopError{message: "decision artifact root must be outside the business worktree", kind: FailureNonRetryable}
	}
	for qi := range state.Brief.Questions {
		question := &state.Brief.Questions[qi]
		if question.Role != decisions.RoleDesign || !question.Blocking {
			continue
		}
		if question.DesignDocumentRequired {
			continue
		}
		if len(question.Options) < 2 || len(question.Options) > 3 {
			return &loopError{message: "design question " + question.ID + " requires 2-3 visual options", kind: FailureManualIntervention}
		}
		for oi := range question.Options {
			option := &question.Options[oi]
			if strings.TrimSpace(option.HTML) == "" {
				return &loopError{message: "design option " + option.ID + " is missing inline static html", kind: FailureManualIntervention}
			}
			safe, sanitizeErr := statichtml.Sanitize(option.HTML)
			if sanitizeErr != nil {
				return &loopError{message: "unsafe design option " + option.ID + ": " + sanitizeErr.Error(), kind: FailureManualIntervention}
			}
			dir, pathErr := confinedArtifactPath(artifactRoot, input.Loop.ID, fmt.Sprint(state.Brief.Revision), question.ID)
			if pathErr != nil {
				return &loopError{message: "unsafe design artifact path: " + pathErr.Error(), kind: FailureManualIntervention}
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			htmlPath, pathErr := confinedArtifactPath(dir, option.ID+".html")
			if pathErr != nil {
				return &loopError{message: "unsafe design HTML path: " + pathErr.Error(), kind: FailureManualIntervention}
			}
			pngPath, pathErr := confinedArtifactPath(dir, option.ID+".png")
			if pathErr != nil {
				return &loopError{message: "unsafe design PNG path: " + pathErr.Error(), kind: FailureManualIntervention}
			}
			manifestPath, pathErr := confinedArtifactPath(dir, option.ID+".render.json")
			if pathErr != nil {
				return &loopError{message: "unsafe design renderer manifest path: " + pathErr.Error(), kind: FailureManualIntervention}
			}
			if err := os.WriteFile(htmlPath, []byte(safe), 0o600); err != nil {
				return err
			}
			browserPath := ""
			if r.projectRoleConfig != nil {
				browserPath = derefString(r.projectRoleConfig.Tools.BrowserPath)
			}
			manifest, err := statichtml.Render(ctx, safe, pngPath, statichtml.Options{BrowserPath: browserPath})
			if err != nil {
				return &loopError{message: "render design option " + option.ID + ": " + err.Error(), kind: FailureManualIntervention}
			}
			if err := persistRendererManifest(manifestPath, manifest); err != nil {
				return &loopError{message: "persist design renderer manifest " + option.ID + ": " + err.Error(), kind: FailureManualIntervention}
			}
			option.HTMLPath = htmlPath
			option.PNGPath = pngPath
			option.ManifestPath = manifestPath
		}
	}
	return nil
}

func persistRendererManifest(path string, manifest statichtml.Manifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(path, encoded, 0o600)
}

func confinedArtifactPath(root string, parts ...string) (string, error) {
	rootAbs, err := canonicalArtifactRoot(root)
	if err != nil {
		return "", err
	}
	for _, part := range parts {
		if filepath.IsAbs(part) {
			return "", fmt.Errorf("absolute artifact component is forbidden")
		}
	}
	candidate, err := filepath.Abs(filepath.Join(append([]string{rootAbs}, parts...)...))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootAbs, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path escapes artifact root")
	}
	if err := rejectSymlinkComponentsWithin(rootAbs, candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

// canonicalArtifactRoot resolves operating-system ancestor aliases (macOS /var is
// commonly a system symlink) while rejecting the configured root itself as a link.
// For a not-yet-created root it resolves the nearest existing ancestor and appends
// the missing suffix, giving confinement checks one stable canonical namespace.
func canonicalArtifactRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := []string{}
	current := abs
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if len(missing) == 0 && info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("artifact root symlink is forbidden: %s", current)
			}
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for artifact root %s", abs)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// rejectSymlinkComponentsWithin checks only the Looper-controlled root and its
// descendants. System ancestor aliases have already been canonicalized above.
func rejectSymlinkComponentsWithin(root, candidate string) error {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("path escapes artifact root")
	}
	current := root
	components := []string{"."}
	if relative != "." {
		components = append(components, strings.Split(relative, string(filepath.Separator))...)
	}
	for _, component := range components {
		if component != "." {
			current = filepath.Join(current, component)
		}
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component is forbidden: %s", current)
		}
	}
	return nil
}

func (r *Runner) postDesignImages(ctx context.Context, input stepInput, state *decisions.State, questions []decisions.Question) error {
	if r.postThreadImage == nil {
		return &loopError{message: "design image notifier is not configured", kind: FailureManualIntervention}
	}
	if state.ImageMessages == nil {
		state.ImageMessages = map[string]string{}
	}
	for _, question := range questions {
		for _, option := range question.Options {
			if strings.TrimSpace(state.ImageMessages[option.ID]) != "" {
				continue
			}
			// The visible Feishu UUID is derived only from the logical artifact identity,
			// not PNG bytes. A crash after remote send but before checkpoint persistence
			// may re-render/upload, yet the thread message remains exactly-once.
			sum := sha256.Sum256([]byte(fmt.Sprintf("design-image:%s:%d:%s", input.Loop.ID, state.Brief.Revision, option.ID)))
			uuid := fmt.Sprintf("%x", sum[:16])
			messageID, err := r.postThreadImage(ctx, input.Loop.ID, option.PNGPath, uuid)
			if err != nil {
				return &loopError{message: "send design image " + option.ID + ": " + err.Error(), kind: FailureRetryableTransient}
			}
			if strings.TrimSpace(messageID) == "" {
				return &loopError{message: "send design image " + option.ID + " returned no message id", kind: FailureManualIntervention}
			}
			state.ImageMessages[option.ID] = messageID
			// Persist each receipt immediately. The stable visible UUID prevents a
			// duplicate message, while this per-image checkpoint also prevents needless
			// re-upload and preserves the Feishu receipt for audit after a crash.
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepRouteDownstreamDecisions, input.Checkpoint); err != nil {
				return &loopError{message: "persist design image receipt " + option.ID + ": " + err.Error(), kind: FailureRetryableAfterResume}
			}
		}
	}
	return nil
}

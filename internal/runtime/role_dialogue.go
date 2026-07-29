package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/planner/decisions"
	"github.com/nexu-io/looper/internal/storage"
)

type roleDialogueQuestionVerdict struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Answer string `json:"answer"`
	Reason string `json:"reason"`
}

type roleDialogueVerdict struct {
	Reply     string                        `json:"reply"`
	Questions []roleDialogueQuestionVerdict `json:"questions"`
}

func judgeRoleDialogue(ctx context.Context, llm triage.LLM, item planestrict.PendingRoleMessage, workingDir string) (roleDialogueVerdict, error) {
	raw, err := llm.Complete(ctx, triage.Request{Prompt: buildRoleDialoguePrompt(item), WorkingDirectory: workingDir})
	if err != nil {
		return roleDialogueVerdict{}, err
	}
	return parseRoleDialogueVerdict(raw, item.RoleRequest.Questions)
}

func buildRoleDialoguePrompt(item planestrict.PendingRoleMessage) string {
	var b strings.Builder
	b.WriteString("你是 Looper 的需求决策对话协调者。你要判断负责人最新回复是否解决了每一个原问题，并直接用简短、通俗的中文回复负责人。\n\n")
	b.WriteString("规则：\n")
	b.WriteString("- 对每个问题只能输出 decided、delegated、still_open 之一，并且必须恰好覆盖全部问题 ID。\n")
	b.WriteString("- decided：人类已经给出足够明确的决定；answer 写可直接执行的最终决定。\n")
	b.WriteString("- delegated：人类明确让 Looper 自己决定（如“你自己定”）；你必须基于上下文选出一个安全、具体、可执行的决定写入 answer。\n")
	b.WriteString("- still_open：回答含糊、只回答了一部分、提出追问、彼此冲突，或缺少继续研发所需信息；answer 留空，reply 中补足背景后只追问必要问题。\n")
	b.WriteString("- PROD-000 表示正式产品 Spec 门禁，永远只能 still_open；可以解释和答疑，但必须引导负责人关联 looper:product-spec。\n")
	b.WriteString("- 不要因为礼貌话、猜测或“我不懂”就判 decided；只有明确授权“你自己定”才可 delegated。\n")
	b.WriteString("- 如果全部收敛，reply 要复述最终决策并说明将继续推进；否则说明已理解的部分和仍需回答的最少问题。\n\n")
	b.WriteString("原问题：\n")
	for _, question := range item.RoleRequest.Questions {
		fmt.Fprintf(&b, "[%s] %s\n背景：%s\n", question.ID, question.Question, question.Context)
		for _, option := range question.Options {
			fmt.Fprintf(&b, "- %s：%s（%s）\n", option.ID, option.Label, option.Impact)
		}
		if question.RecommendedOption != "" {
			fmt.Fprintf(&b, "建议：%s（%s）\n", question.RecommendedOption, question.RecommendationReason)
		}
	}
	b.WriteString("\n对话历史：\n")
	for _, message := range item.History {
		speaker := "负责人"
		if message.Kind == "looper_reply" {
			speaker = "Looper"
		}
		fmt.Fprintf(&b, "%s：%s\n", speaker, message.Body)
	}
	b.WriteString("\n只输出一个 JSON 对象，不要代码块或其他文字：\n")
	b.WriteString(`{"reply":"给负责人的中文回复","questions":[{"id":"原问题ID","status":"decided|delegated|still_open","answer":"最终可执行决定或空字符串","reason":"一句话判断依据"}]}`)
	b.WriteString("\n")
	return b.String()
}

func parseRoleDialogueVerdict(raw string, questions []planestrict.RoleQuestion) (roleDialogueVerdict, error) {
	value := strings.TrimSpace(raw)
	start, end := strings.Index(value, "{"), strings.LastIndex(value, "}")
	if start < 0 || end < start {
		return roleDialogueVerdict{}, fmt.Errorf("role dialogue: no JSON object in model output: %.160q", value)
	}
	var verdict roleDialogueVerdict
	if err := json.Unmarshal([]byte(value[start:end+1]), &verdict); err != nil {
		return roleDialogueVerdict{}, fmt.Errorf("role dialogue: parse model output: %w", err)
	}
	verdict.Reply = strings.TrimSpace(verdict.Reply)
	if verdict.Reply == "" || len(verdict.Reply) > 5000 {
		return roleDialogueVerdict{}, fmt.Errorf("role dialogue: reply is empty or too long")
	}
	expected := make(map[string]planestrict.RoleQuestion, len(questions))
	for _, question := range questions {
		expected[question.ID] = question
	}
	seen := make(map[string]bool, len(verdict.Questions))
	for index := range verdict.Questions {
		item := &verdict.Questions[index]
		item.ID = strings.TrimSpace(item.ID)
		item.Status = strings.TrimSpace(item.Status)
		item.Answer = strings.TrimSpace(item.Answer)
		item.Reason = strings.TrimSpace(item.Reason)
		question, ok := expected[item.ID]
		if !ok || seen[item.ID] {
			return roleDialogueVerdict{}, fmt.Errorf("role dialogue: unknown or duplicate question %q", item.ID)
		}
		seen[item.ID] = true
		if item.Status != "decided" && item.Status != "delegated" && item.Status != "still_open" {
			return roleDialogueVerdict{}, fmt.Errorf("role dialogue: invalid status for %s", item.ID)
		}
		if item.Status != "still_open" && item.Answer == "" {
			return roleDialogueVerdict{}, fmt.Errorf("role dialogue: resolved question %s has no answer", item.ID)
		}
		if question.ID == "PROD-000" && item.Status != "still_open" {
			return roleDialogueVerdict{}, fmt.Errorf("role dialogue: formal product spec cannot be resolved in chat")
		}
	}
	if len(seen) != len(expected) {
		return roleDialogueVerdict{}, fmt.Errorf("role dialogue: model did not cover every question")
	}
	return verdict, nil
}

func roleDialogueEvaluation(verdict roleDialogueVerdict) planestrict.RoleMessageEvaluation {
	evaluation := planestrict.RoleMessageEvaluation{Resolved: true, Questions: make([]planestrict.RoleQuestionEvaluation, 0, len(verdict.Questions))}
	for _, item := range verdict.Questions {
		if item.Status == "still_open" {
			evaluation.Resolved = false
		}
		evaluation.Questions = append(evaluation.Questions, planestrict.RoleQuestionEvaluation{
			ID: item.ID, Status: item.Status, Answer: item.Answer, Reason: item.Reason,
		})
	}
	return evaluation
}

func applyRoleDialogueVerdict(state *decisions.State, request decisions.RequestReceipt, pending planestrict.PendingRoleMessage, verdict roleDialogueVerdict) {
	if state.Answers == nil {
		state.Answers = map[string]decisions.Answer{}
	}
	commentID := ""
	if pending.Message.CommentID != nil {
		commentID = *pending.Message.CommentID
	}
	for _, item := range verdict.Questions {
		if item.Status == "still_open" {
			delete(state.Answers, item.ID)
			continue
		}
		questionHash := ""
		for _, question := range state.RequestedQuestions[request.Role] {
			if question.ID == item.ID {
				questionHash = decisions.QuestionHash(question)
				break
			}
		}
		state.Answers[item.ID] = decisions.Answer{
			QuestionID: item.ID, Value: item.Answer, Revision: request.Revision,
			QuestionHash: questionHash, CommentID: commentID,
			Actor: request.EligibleMemberID, CreatedAt: pending.Message.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		}
	}
}

func deterministicRoleReplyID(messageID string) string {
	sum := sha256.Sum256([]byte("looper-role-reply:" + strings.TrimSpace(messageID)))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func strictDispatchID(metadataJSON *string) string {
	if metadataJSON == nil {
		return ""
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(*metadataJSON), &metadata) != nil {
		return ""
	}
	value, _ := metadata["strictDispatchId"].(string)
	return strings.TrimSpace(value)
}

func strictRoleConversationEnabled(state *decisions.State) bool {
	if state == nil {
		return false
	}
	for _, request := range state.Requests {
		if strings.TrimSpace(request.RoleRequestID) != "" {
			return true
		}
	}
	return false
}

func projectByID(cfg config.Config, projectID string) (config.ProjectRefConfig, bool) {
	for _, project := range cfg.Projects {
		if project.ID == projectID {
			return project, true
		}
	}
	return config.ProjectRefConfig{}, false
}

func processPendingStrictRoleMessage(ctx context.Context, cfg config.Config, loop storage.LoopRecord, state *decisions.State, llm triage.LLM) (bool, error) {
	dispatchID := strictDispatchID(loop.MetadataJSON)
	if dispatchID == "" || !strictRoleConversationEnabled(state) {
		return false, nil
	}
	project, ok := projectByID(cfg, loop.ProjectID)
	if !ok {
		return false, fmt.Errorf("role dialogue: project %s is not configured", loop.ProjectID)
	}
	client, ok, err := planeClientForRepo(&cfg, project.Repo)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("Plane strict client is unavailable")
		}
		return false, err
	}
	pendingResponse, dispatch, err := client.PendingStrictRoleMessages(ctx, dispatchID)
	if err != nil {
		return false, err
	}
	if len(pendingResponse.Pending) == 0 {
		return false, nil
	}
	item := pendingResponse.Pending[len(pendingResponse.Pending)-1]
	request, found := state.Requests[decisions.Role(item.RoleRequest.Role)]
	if !found || request.RoleRequestID != item.RoleRequest.ID {
		return false, fmt.Errorf("role dialogue: pending request does not match planner checkpoint")
	}
	if llm == nil {
		return false, fmt.Errorf("role dialogue: decision model is not configured")
	}
	verdict, err := judgeRoleDialogue(ctx, llm, item, project.RepoPath)
	if err != nil {
		return false, err
	}
	evaluation := roleDialogueEvaluation(verdict)
	processedMessageIDs := make([]string, 0, len(pendingResponse.Pending))
	for _, pending := range pendingResponse.Pending {
		if pending.RoleRequest.ID == item.RoleRequest.ID {
			processedMessageIDs = append(processedMessageIDs, pending.Message.ID)
		}
	}
	_, err = client.ReplyStrictRoleMessage(ctx, dispatch, item.RoleRequest.ID, planestrict.RoleMessageReplyInput{
		ClientMessageID: deterministicRoleReplyID(item.Message.ID), InReplyToMessageID: item.Message.ID, ProcessedMessageIDs: processedMessageIDs,
		Reply: verdict.Reply, Evaluation: evaluation,
	})
	if err != nil {
		return false, err
	}
	applyRoleDialogueVerdict(state, request, item, verdict)
	return true, nil
}

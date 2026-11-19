package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// feishuCollaborationCard is the task anchor. Activity stays in the thread;
// only the current update belongs on the main card.
func (g *Gateway) feishuCollaborationCard(ctx context.Context, loop *storage.LoopRecord, color, label, sourceLabel, sourceURL, prURL string, tail []string, elapsed int64) (string, bool) {
	title := loopTitleFromMetadata(loop.MetadataJSON)
	if title == "" {
		title = sourceLabel
	}
	if title == "" {
		title = "协作任务"
	}
	brief, persistedElapsed := g.feishuTaskUpdate(ctx, loop, tail)
	if elapsed == 0 || feishuLoopStatusTerminal(loop.Status) {
		elapsed = persistedElapsed
	}
	if brief == "" {
		brief = strings.ReplaceAll(label, "Looper ", "")
	}
	if color == "grey" {
		color = "neutral"
	}
	label = strings.ReplaceAll(label, "Looper ", "")
	resumeOnly := loop.Status == "awaiting_human" && loopAwaitingExplicitResume(loop.MetadataJSON)
	if loop.Status == "awaiting_human" {
		if resumeOnly {
			label = "已暂停"
		} else {
			label = "待回复"
		}
	}
	heading := []any{map[string]any{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []any{
		taskCardMarkdown("<text_tag color='"+color+"'>"+label+"</text_tag>", "notation"),
	}}}
	if sourceURL != "" {
		heading = append(heading, map[string]any{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []any{
			map[string]any{"tag": "interactive_container", "width": "auto", "padding": "0px",
				"behaviors": []any{map[string]any{"type": "open_url", "default_url": sourceURL}},
				"elements":  []any{taskCardMarkdown("<text_tag color='blue'>"+sourceLabel+" ↗</text_tag>", "notation")}},
		}})
	}
	if elapsed > 0 && loop.Status != "awaiting_human" {
		heading = append(heading, map[string]any{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []any{
			taskCardMarkdown("<font color='grey'>· "+humanizeElapsedSeconds(elapsed)+"</font>", "notation"),
		}})
	}
	if owner := g.looperAttribution(loop.ProjectID); owner != "" {
		heading = append(heading, map[string]any{"tag": "column", "width": "weighted", "weight": 1, "elements": []any{}})
		heading = append(heading, map[string]any{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []any{
			taskCardMarkdown(owner, "notation"),
		}})
	}
	elements := []any{
		map[string]any{"tag": "column_set", "flex_mode": "none", "horizontal_align": "left", "horizontal_spacing": "8px", "columns": heading},
		taskCardMarkdown("**"+title+"**", "normal"),
		map[string]any{"tag": "column_set", "flex_mode": "none", "columns": []any{
			map[string]any{"tag": "column", "width": "weighted", "weight": 1, "background_style": "grey-50", "padding": "10px", "vertical_spacing": "4px", "elements": []any{
				taskCardMarkdown(taskCardExcerpt(brief, 180), "normal"),
			}},
		}},
	}
	if loop.Status == "awaiting_human" {
		waiting := "尚未指定回复负责人"
		mentions := []string{}
		if ask, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok {
			for _, id := range ask.MentionOpenIDs {
				if strings.TrimSpace(id) != "" {
					mentions = append(mentions, taskCardPerson(id))
				}
			}
		}
		if len(mentions) == 0 {
			if owner := g.ownerOpenID(loop.ProjectID); owner != "" {
				mentions = append(mentions, taskCardPerson(owner))
			}
		}
		if resumeOnly {
			waiting = "已按停止要求暂停；如需继续，请在话题 @bot 回复「继续」"
		} else if len(mentions) > 0 {
			waiting = "等待 " + strings.Join(mentions, "、") + " 回复；请在话题 @bot 直接写结论或选择"
		} else {
			waiting = "请在话题 @bot 回复「继续」或写明你的决定"
		}
		elements = append(elements, taskCardMarkdown(waiting, "notation"))
	}
	if len([]rune(brief)) > 180 {
		elements = append(elements, taskCardDetails("完整内容", []any{taskCardMarkdown(brief, "normal")}))
	}

	// These are real source/delivery URLs. Replies use Feishu's native task thread;
	// no callback or approval is implied by clicking a link.
	columns := []any{}
	for _, item := range [][2]string{{"查看交付", prURL}} {
		if strings.TrimSpace(item[1]) == "" {
			continue
		}
		buttonType := "default"
		if len(columns) == 0 {
			buttonType = "primary_filled"
		}
		button := map[string]any{"tag": "button", "text": map[string]any{"tag": "plain_text", "content": item[0]}, "type": buttonType, "size": "small", "behaviors": []any{map[string]any{"type": "open_url", "default_url": item[1]}}}
		columns = append(columns, map[string]any{"tag": "column", "width": "auto", "elements": []any{button}})
	}
	if len(columns) > 0 {
		elements = append(elements, map[string]any{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": columns})
	}
	// The bridge can deliver one task across several repositories. Read its
	// actual PR references without forcing them into the legacy single PR slot.
	var metadata struct {
		Bridge struct {
			Outcome struct {
				PullRequests []string `json:"pullRequests"`
			} `json:"outcome"`
		} `json:"bridge"`
	}
	if loop.MetadataJSON != nil {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &metadata)
	}
	for _, link := range metadata.Bridge.Outcome.PullRequests {
		label := strings.Replace(strings.TrimPrefix(link, "https://github.com/"), "/pull/", " #", 1)
		elements = append(elements, taskCardMarkdown("["+label+" ↗]("+link+")", "normal"))
	}
	raw, err := json.Marshal(map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true, "width_mode": "default", "summary": map[string]any{"content": label + " · " + title}}, "body": map[string]any{"direction": "vertical", "padding": "12px", "vertical_spacing": "8px", "elements": elements}})
	return string(raw), err == nil
}

// loopAwaitingExplicitResume identifies a human-requested stop that has not
// been explicitly resumed yet. It is presentation-only: the durable loop
// status remains the authority for whether work can run.
func loopAwaitingExplicitResume(metadataJSON *string) bool {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return false
	}
	var meta struct {
		Bridge struct {
			Outcome struct {
				Summary  string `json:"summary"`
				NextStep string `json:"nextStep"`
			} `json:"outcome"`
		} `json:"bridge"`
	}
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return false
	}
	text := strings.TrimSpace(meta.Bridge.Outcome.Summary + "\n" + meta.Bridge.Outcome.NextStep)
	return strings.Contains(text, "保持停止") || strings.Contains(text, "停止要求") || strings.Contains(text, "明确恢复")
}

func (g *Gateway) looperAttribution(projectID string) string {
	if id := g.ownerOpenID(projectID); id != "" {
		name := "任务负责人"
		if g.resolveOwnerName != nil {
			if resolved := strings.TrimSpace(g.resolveOwnerName(id)); resolved != "" {
				name = resolved
			}
		}
		return "<text_tag color='neutral'>" + html.EscapeString(name) + "的 Looper</text_tag>"
	}
	return ""
}

// Names are display text. Encoding one as an @ resource makes Feishu reject
// the entire card and leaves its previous status frozen.
func taskCardPerson(person string) string {
	person = strings.TrimSpace(person)
	if strings.HasPrefix(person, "ou_") && !strings.ContainsAny(person, " <>\"'\t\r\n") {
		return "<at id=" + person + "></at>"
	}
	return html.EscapeString(person)
}

func taskCardMarkdown(text, size string) map[string]any {
	return map[string]any{"tag": "markdown", "content": text, "text_size": size}
}

func taskCardDetails(title string, elements []any) map[string]any {
	return map[string]any{
		"tag": "collapsible_panel", "expanded": false, "padding": "0px",
		"header": map[string]any{
			"title":         taskCardMarkdown("<font color='grey'>"+title+"</font>", "notation"),
			"icon":          map[string]any{"tag": "standard_icon", "token": "down-small-ccm_outlined", "color": "grey", "size": "16px 16px"},
			"icon_position": "follow_text", "icon_expanded_angle": -180,
		},
		"elements": elements,
	}
}

// Markdown is rendered by Feishu, not flattened into a plain-text message.
// The agent writes the short answer; the transport does not infer which parts
// of that answer are safe to hide.
func buildFeishuUpdateCard(payload SystemNotificationPayload) ([]byte, error) {
	elements := []any{}
	if title := strings.TrimSpace(payload.Title); title != "" {
		elements = append(elements, taskCardMarkdown("**"+title+"**", "normal"))
	}
	if body := strings.TrimSpace(payload.Body); body != "" {
		elements = append(elements, taskCardMarkdown(body, "normal"))
	}
	if mentions := feishuMentionMarkup(payload.MentionOpenIds); mentions != "" {
		elements = append(elements, taskCardMarkdown(mentions, "notation"))
	}
	return json.Marshal(map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true}, "body": map[string]any{"padding": "12px", "vertical_spacing": "8px", "elements": elements}})
}

func taskCardExcerpt(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// Read existing execution records instead of copying delivery summaries into
// another field. The latest run bounds the summary, so earlier runs cannot leak
// into the current task phase after a restart.
func (g *Gateway) feishuTaskUpdate(ctx context.Context, loop *storage.LoopRecord, tail []string) (string, int64) {
	var run *storage.RunRecord
	if g.repositories.Runs != nil {
		run, _ = g.repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
	}
	elapsed := int64(0)
	if run != nil && run.EndedAt != nil {
		started, e1 := time.Parse(time.RFC3339Nano, run.StartedAt)
		ended, e2 := time.Parse(time.RFC3339Nano, *run.EndedAt)
		if e1 == nil && e2 == nil && ended.After(started) {
			elapsed = int64(ended.Sub(started).Seconds())
		}
	}
	if loop.Status == "awaiting_human" {
		if ask, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok && ask.Status == "awaiting" && strings.TrimSpace(ask.Question) != "" {
			return ask.Question, elapsed
		}
	}
	if feishuLoopStatusTerminal(loop.Status) && run != nil {
		if loop.Status == "completed" && g.repositories.AgentExecutions != nil {
			execution, _ := g.repositories.AgentExecutions.GetLatestByRunID(ctx, run.ID)
			if execution != nil && execution.Summary != nil && strings.TrimSpace(*execution.Summary) != "" {
				return strings.TrimSpace(*execution.Summary), elapsed
			}
		}
		for _, text := range []*string{run.ErrorMessage, run.Summary} {
			if text != nil && strings.TrimSpace(*text) != "" {
				return strings.TrimSpace(*text), elapsed
			}
		}
	}
	if loop.Status == "running" {
		if phase := feishuPhaseFromTail(tail); phase != "" {
			return phase, 0
		}
		return "", 0
	}
	for _, key := range []string{"summary", "changeSummary", "resultSummary", "outcome"} {
		if brief := loopWorkerString(loop.MetadataJSON, key); brief != "" {
			return brief, elapsed
		}
	}
	return "", elapsed
}

// The question remains part of the conversation, unchanged after a reply.
// Numbering is presentation only; the agent interprets the human's words.
func buildFeishuThreadQuestion(card HITLAskCard) ([]byte, error) {
	question := strings.TrimSpace(card.Question)
	elements := []any{taskCardMarkdown("**"+question+"**", "normal")}
	details := []any{}
	if rec := strings.TrimSpace(card.Recommendation); rec != "" {
		details = append(details, taskCardMarkdown(rec, "normal"))
	}
	options := []string{}
	for _, option := range card.Options {
		option = strings.TrimSpace(option)
		if option == "" {
			continue
		}
		line := fmt.Sprintf("%d. %s", len(options)+1, option)
		if strings.EqualFold(option, strings.TrimSpace(card.RecommendedOption)) {
			line += "（推荐）"
		}
		if consequence := strings.TrimSpace(card.Consequences[option]); consequence != "" {
			details = append(details, taskCardMarkdown("**"+option+"**\n"+consequence, "normal"))
		}
		options = append(options, line)
	}
	if len(options) > 0 {
		elements = append(elements, map[string]any{"tag": "column_set", "columns": []any{
			map[string]any{"tag": "column", "width": "weighted", "weight": 1, "background_style": "grey-50", "padding": "8px", "elements": []any{taskCardMarkdown(strings.Join(options, "\n"), "normal")}},
		}})
	}
	if len(details) > 0 {
		elements = append(elements, taskCardDetails("判断依据与验证详情", details))
	}
	mentions := feishuMentionMarkup(card.MentionOpenIds)
	if mentions == "" && strings.TrimSpace(card.OwnerOpenID) != "" {
		mentions = feishuMentionMarkup([]string{card.OwnerOpenID})
	}
	if mentions != "" {
		elements = append(elements, taskCardMarkdown(mentions+" 在话题里 @bot 回复即可，也可以追问。", "notation"))
	}
	return json.Marshal(map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true, "width_mode": "default", "summary": map[string]any{"content": question}}, "body": map[string]any{"padding": "12px", "vertical_spacing": "8px", "elements": elements}})
}

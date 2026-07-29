// Package decisions defines the versioned, authority-bearing protocol used before
// a Plane technical spec is authored. It deliberately contains no transport code.
package decisions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const ProtocolVersion = 1

type Role string

const (
	RoleProduct     Role = "product"
	RoleDesign      Role = "design"
	RoleEngineering Role = "engineering"
)

type Fact struct {
	Text     string `json:"text"`
	Evidence string `json:"evidence"`
}

type FormalProductSpec struct {
	Required bool   `json:"required"`
	Reason   string `json:"reason"`
}

type Option struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Impact       string `json:"impact"`
	HTML         string `json:"html,omitempty"`
	HTMLPath     string `json:"htmlPath,omitempty"`
	PNGPath      string `json:"pngPath,omitempty"`
	ManifestPath string `json:"manifestPath,omitempty"`
}

type Question struct {
	ID                     string   `json:"id"`
	Role                   Role     `json:"role"`
	Blocking               bool     `json:"blocking"`
	Question               string   `json:"question"`
	Context                string   `json:"context"`
	Options                []Option `json:"options,omitempty"`
	RecommendedOption      string   `json:"recommendedOption,omitempty"`
	RecommendationReason   string   `json:"recommendationReason,omitempty"`
	Evidence               []string `json:"evidence"`
	DesignDocumentRequired bool     `json:"designDocumentRequired,omitempty"`
}

type Brief struct {
	Version           int               `json:"version"`
	Revision          int               `json:"revision,omitempty"`
	Summary           string            `json:"summary"`
	Facts             []Fact            `json:"facts"`
	FormalProductSpec FormalProductSpec `json:"formalProductSpec"`
	Questions         []Question        `json:"questions"`
}

type RequestReceipt struct {
	Role             Role   `json:"role"`
	Revision         int    `json:"revision"`
	RoleRequestID    string `json:"roleRequestId,omitempty"`
	CommentID        string `json:"commentId"`
	CreatedAt        string `json:"createdAt"`
	EligibleMemberID string `json:"eligibleMemberId,omitempty"`
}

type Answer struct {
	QuestionID   string `json:"questionId"`
	Value        string `json:"value"`
	Revision     int    `json:"revision"`
	QuestionHash string `json:"questionHash"`
	CommentID    string `json:"commentId"`
	Actor        string `json:"actor"`
	CreatedAt    string `json:"createdAt"`
}

type State struct {
	Brief              Brief                   `json:"brief"`
	Stage              string                  `json:"stage"`
	ReopenReason       string                  `json:"reopenReason,omitempty"`
	ProductSpec        string                  `json:"productSpec,omitempty"`
	Requests           map[Role]RequestReceipt `json:"requests,omitempty"`
	RequestedQuestions map[Role][]Question     `json:"requestedQuestions,omitempty"`
	Answers            map[string]Answer       `json:"answers,omitempty"`
	DecisionLog        string                  `json:"decisionLogCommentId,omitempty"`
	ImageMessages      map[string]string       `json:"imageMessages,omitempty"`
}

var questionIDPattern = regexp.MustCompile(`^(PROD|DESIGN|ENG)-[0-9]{3}$`)
var optionIDPattern = regexp.MustCompile(`^(PROD|DESIGN|ENG)-[0-9]{3}-[A-Z0-9]{1,16}$`)

func ValidateBrief(brief Brief) error {
	if brief.Version != ProtocolVersion {
		return fmt.Errorf("decision brief version %d is unsupported", brief.Version)
	}
	if strings.TrimSpace(brief.Summary) == "" {
		return fmt.Errorf("decision brief summary is required")
	}
	if len(brief.Facts) == 0 {
		return fmt.Errorf("decision brief requires at least one evidenced fact")
	}
	for i, fact := range brief.Facts {
		if strings.TrimSpace(fact.Text) == "" || strings.TrimSpace(fact.Evidence) == "" {
			return fmt.Errorf("fact %d requires text and evidence", i+1)
		}
	}
	seen := map[string]bool{}
	for _, question := range brief.Questions {
		if !questionIDPattern.MatchString(question.ID) || seen[question.ID] {
			return fmt.Errorf("question id %q is invalid or duplicated", question.ID)
		}
		seen[question.ID] = true
		prefix := strings.SplitN(question.ID, "-", 2)[0]
		if (prefix == "PROD" && question.Role != RoleProduct) || (prefix == "DESIGN" && question.Role != RoleDesign) || (prefix == "ENG" && question.Role != RoleEngineering) {
			return fmt.Errorf("question %s does not match role %s", question.ID, question.Role)
		}
		if strings.TrimSpace(question.Question) == "" || strings.TrimSpace(question.Context) == "" || len(question.Evidence) == 0 {
			return fmt.Errorf("question %s requires question, standalone context, and evidence", question.ID)
		}
		optionIDs := map[string]bool{}
		for _, option := range question.Options {
			if !optionIDPattern.MatchString(option.ID) || !strings.HasPrefix(option.ID, question.ID+"-") || optionIDs[option.ID] {
				return fmt.Errorf("question %s has invalid option id %q", question.ID, option.ID)
			}
			optionIDs[option.ID] = true
		}
		if question.DesignDocumentRequired && (!question.Blocking || question.Role != RoleDesign || len(question.Options) != 0) {
			return fmt.Errorf("question %s design-document gate must be a blocking design question without options", question.ID)
		}
		if question.Blocking && len(question.Options) == 0 && !question.DesignDocumentRequired {
			return fmt.Errorf("blocking question %s requires 2-3 explicit options", question.ID)
		}
		if len(question.Options) > 0 {
			if len(question.Options) < 2 || len(question.Options) > 3 {
				return fmt.Errorf("question %s must provide 2-3 options", question.ID)
			}
			if !optionIDs[question.RecommendedOption] || strings.TrimSpace(question.RecommendationReason) == "" {
				return fmt.Errorf("question %s requires a valid recommendation", question.ID)
			}
		}
	}
	return nil
}

func CanonicalHash(brief Brief) (string, error) {
	encoded, err := json.Marshal(brief)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func QuestionsForRole(brief Brief, role Role) []Question {
	out := make([]Question, 0)
	for _, question := range brief.Questions {
		if question.Role == role && question.Blocking {
			out = append(out, question)
		}
	}
	return out
}

func UnansweredBlocking(state State, roles ...Role) []Question {
	var out []Question
	for _, role := range roles {
		questions := state.RequestedQuestions[role]
		if len(questions) == 0 {
			questions = QuestionsForRole(state.Brief, role)
		}
		for _, question := range questions {
			if !question.Blocking {
				continue
			}
			if question.ID == "PROD-000" && strings.TrimSpace(state.ProductSpec) != "" {
				continue
			}
			answer, ok := state.Answers[question.ID]
			request, requested := state.Requests[role]
			questionHash := QuestionHash(question)
			if !ok || !requested || answer.Revision != request.Revision || answer.QuestionHash == "" || answer.QuestionHash != questionHash {
				out = append(out, question)
			}
		}
	}
	return out
}

func QuestionHash(question Question) string {
	encoded, err := json.Marshal(question)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// ParseAnswers accepts concise Plane replies such as "DESIGN-001: DESIGN-001-B".
// Question IDs are mandatory; ambiguous prose never silently unblocks a decision.
func ParseAnswers(text string, questions []Question) map[string]string {
	answers, _ := ParseAnswerSet(text, questions)
	return answers
}

// ParseAnswerSet also reports question IDs that received two different valid
// options in the same comment. Callers must clear any earlier answer for those IDs
// so an internally contradictory newer reply cannot leave a decision unblocked.
func ParseAnswerSet(text string, questions []Question) (map[string]string, map[string]bool) {
	known := map[string]Question{}
	for _, question := range questions {
		known[question.ID] = question
	}
	out := map[string]string{}
	conflicted := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		left, right, ok := strings.Cut(line, ":")
		if !ok {
			left, right, ok = strings.Cut(line, "：")
		}
		id := strings.TrimSpace(left)
		value := strings.TrimSpace(right)
		if !ok || value == "" {
			continue
		}
		question, exists := known[id]
		if !exists {
			continue
		}
		if question.DesignDocumentRequired {
			if !validDesignDocumentURL(value) {
				delete(out, id)
				conflicted[id] = true
				continue
			}
		} else if len(question.Options) > 0 {
			valid := false
			mentionsKnownOption := false
			for _, option := range question.Options {
				if strings.EqualFold(value, option.ID) {
					value = option.ID
					valid = true
					break
				}
				if strings.Contains(strings.ToUpper(value), strings.ToUpper(option.ID)) {
					mentionsKnownOption = true
				}
			}
			if !valid {
				// A custom decision is authority-bearing, so make it explicit rather than
				// guessing whether arbitrary prose is a decision. Invalid or ambiguous
				// replacements invalidate an earlier answer for this question.
				custom, customOK := explicitCustomDecision(value)
				if mentionsKnownOption || strings.HasPrefix(strings.ToUpper(value), strings.ToUpper(question.ID+"-")) || !customOK {
					delete(out, id)
					conflicted[id] = true
					continue
				}
				value = custom
			}
		}
		if conflicted[id] {
			continue
		}
		if previous, seen := out[id]; seen && !strings.EqualFold(previous, value) {
			delete(out, id)
			conflicted[id] = true
			continue
		}
		out[id] = value
	}
	return out, conflicted
}

func validDesignDocumentURL(value string) bool {
	if strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

func explicitCustomDecision(value string) (string, bool) {
	value = strings.TrimSpace(value)
	var custom string
	for _, prefix := range []string{"自定义:", "自定义：", "CUSTOM:", "custom:"} {
		if strings.HasPrefix(value, prefix) {
			custom = strings.TrimSpace(strings.TrimPrefix(value, prefix))
			break
		}
	}
	if custom == "" || len([]rune(custom)) > 1000 {
		return "", false
	}
	normalized := strings.ToLower(strings.Trim(strings.TrimSpace(custom), "。.!！"))
	for _, nonDecision := range []string{"待定", "我再看看", "稍后", "不确定", "需要讨论", "tbd", "later", "unsure"} {
		if normalized == nonDecision {
			return "", false
		}
	}
	return custom, true
}

func DecisionLogMarkdown(state State) string {
	ids := make([]string, 0, len(state.Answers))
	for id := range state.Answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("### Decision Log\n\n")
	fmt.Fprintf(&b, "- **需求结论**: %s\n", state.Brief.Summary)
	if state.Brief.FormalProductSpec.Required {
		fmt.Fprintf(&b, "- **正式产品 Spec**: 必须提供（%s）\n", state.Brief.FormalProductSpec.Reason)
	} else {
		b.WriteString("- **正式产品 Spec**: 不需要；本需求可由逐项事实与决策收敛。\n")
	}
	for i, fact := range state.Brief.Facts {
		fmt.Fprintf(&b, "- **事实 %d**: %s（证据：%s）\n", i+1, fact.Text, fact.Evidence)
	}
	for _, id := range ids {
		answer := state.Answers[id]
		fmt.Fprintf(&b, "- **%s**: %s（revision `%d`, actor `%s`, comment `%s`, %s）\n", id, answer.Value, answer.Revision, answer.Actor, answer.CommentID, answer.CreatedAt)
	}
	if len(ids) == 0 {
		b.WriteString("- **真人决策**: 无；final requirement GRILL 未发现需要产品、设计或研发负责人拍板的阻塞问题。\n")
	}
	return b.String()
}

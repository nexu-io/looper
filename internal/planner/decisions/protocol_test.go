package decisions

import (
	"strings"
	"testing"
)

func TestValidateBriefAndRoleAuthority(t *testing.T) {
	brief := Brief{Version: 1, Summary: "用户需要选择按钮位置", Facts: []Fact{{Text: "当前按钮在顶部", Evidence: "app/view.tsx:10"}}, Questions: []Question{{ID: "DESIGN-001", Role: RoleDesign, Blocking: true, Question: "按钮放在哪里？", Context: "用户完成编辑后需要导出。", Evidence: []string{"app/view.tsx:10"}, Options: []Option{{ID: "DESIGN-001-A", Label: "顶部", HTML: "<button>顶部</button>"}, {ID: "DESIGN-001-B", Label: "底部", HTML: "<button>底部</button>"}}, RecommendedOption: "DESIGN-001-A", RecommendationReason: "与现有页面一致"}}}
	if err := ValidateBrief(brief); err != nil {
		t.Fatalf("ValidateBrief() error = %v", err)
	}
	brief.Questions[0].Role = RoleProduct
	if err := ValidateBrief(brief); err == nil {
		t.Fatal("role/id mismatch must fail")
	}
}

func TestValidateBriefRejectsPathLikeOptionID(t *testing.T) {
	brief := Brief{Version: 1, Summary: "x", Facts: []Fact{{Text: "fact", Evidence: "x.go:1"}}, Questions: []Question{{ID: "DESIGN-001", Role: RoleDesign, Blocking: true, Question: "选哪个？", Context: "完整背景", Evidence: []string{"x.go:1"}, Options: []Option{{ID: "DESIGN-001-A", Label: "A"}, {ID: "DESIGN-001-../../escape", Label: "B"}}, RecommendedOption: "DESIGN-001-A", RecommendationReason: "一致"}}}
	if err := ValidateBrief(brief); err == nil {
		t.Fatal("path-like option id must be rejected")
	}
}

func TestDesignDocumentGateRequiresHTTPURLAndNoHTMLChoices(t *testing.T) {
	question := Question{ID: "DESIGN-002", Role: RoleDesign, Blocking: true, Question: "请提供新流程设计稿", Context: "这是跨三步的新用户流程。", Evidence: []string{"product-spec"}, DesignDocumentRequired: true}
	brief := Brief{Version: 1, Summary: "新流程", Facts: []Fact{{Text: "跨三步", Evidence: "product-spec"}}, Questions: []Question{question}}
	if err := ValidateBrief(brief); err != nil {
		t.Fatalf("valid design document gate rejected: %v", err)
	}
	for _, answer := range []string{"DESIGN-002: https://design.example/doc/123", "DESIGN-002: http://plane.example/pages/abc"} {
		if got := ParseAnswers(answer, []Question{question}); got["DESIGN-002"] == "" {
			t.Fatalf("design URL rejected: %q => %#v", answer, got)
		}
	}
	for _, answer := range []string{"DESIGN-002: 稍后补", "DESIGN-002: file:///tmp/mockup", "DESIGN-002: https://a.example/x https://b.example/y"} {
		if got, invalidated := ParseAnswerSet(answer, []Question{question}); len(got) != 0 || !invalidated["DESIGN-002"] {
			t.Fatalf("invalid design doc answer accepted: %q => %#v %#v", answer, got, invalidated)
		}
	}
	brief.Questions[0].Options = []Option{{ID: "DESIGN-002-A"}, {ID: "DESIGN-002-B"}}
	if err := ValidateBrief(brief); err == nil {
		t.Fatal("design document gate with HTML choices must fail validation")
	}
}

func TestUnansweredBlockingRequiresCurrentRequestRevisionAndQuestionHash(t *testing.T) {
	question := Question{ID: "ENG-001", Role: RoleEngineering, Blocking: true, Question: "如何重试？", Context: "完整背景", Evidence: []string{"x.go:1"}}
	state := State{
		Brief:    Brief{Version: 1, Revision: 2, Summary: "x", Facts: []Fact{{Text: "fact", Evidence: "x.go:1"}}, Questions: []Question{question}},
		Requests: map[Role]RequestReceipt{RoleEngineering: {Role: RoleEngineering, Revision: 2}},
		Answers:  map[string]Answer{"ENG-001": {QuestionID: "ENG-001", Value: "旧答案", Revision: 1, QuestionHash: QuestionHash(question)}},
	}
	if got := UnansweredBlocking(state, RoleEngineering); len(got) != 1 {
		t.Fatalf("stale revision incorrectly unblocked: %#v", got)
	}
	state.Answers["ENG-001"] = Answer{QuestionID: "ENG-001", Value: "当前答案", Revision: 2, QuestionHash: QuestionHash(question)}
	if got := UnansweredBlocking(state, RoleEngineering); len(got) != 0 {
		t.Fatalf("current answer remained blocked: %#v", got)
	}
	question.Context = "变化后的背景"
	state.Brief.Questions[0] = question
	if got := UnansweredBlocking(state, RoleEngineering); len(got) != 1 {
		t.Fatalf("changed question incorrectly reused old answer: %#v", got)
	}
}

func TestParseAnswersRequiresQuestionIDAndValidOption(t *testing.T) {
	questions := []Question{{ID: "DESIGN-001", Role: RoleDesign, Options: []Option{{ID: "DESIGN-001-A"}, {ID: "DESIGN-001-B"}}}, {ID: "ENG-001", Role: RoleEngineering}}
	got := ParseAnswers("选 B\nDESIGN-001: DESIGN-001-B\nENG-001：采用队列", questions)
	if got["DESIGN-001"] != "DESIGN-001-B" || got["ENG-001"] != "采用队列" || len(got) != 2 {
		t.Fatalf("ParseAnswers() = %#v", got)
	}
	if got := ParseAnswers("DESIGN-001: DESIGN-001-X", questions); len(got) != 0 {
		t.Fatalf("invalid option must not unblock: %#v", got)
	}
	if got := ParseAnswers("DESIGN-001: 自定义: 两个方案都不合适，请保留现有位置并增加快捷键", questions); got["DESIGN-001"] != "两个方案都不合适，请保留现有位置并增加快捷键" {
		t.Fatalf("clear custom decision must be accepted: %#v", got)
	}
	if got := ParseAnswers("DESIGN-001: DESIGN-001-A 或 DESIGN-001-B 都行", questions); len(got) != 0 {
		t.Fatalf("prose mixing option IDs must remain blocked: %#v", got)
	}
	if got := ParseAnswers("DESIGN-001: DESIGN-001-A\nDESIGN-001: DESIGN-001-B", questions); len(got) != 0 {
		t.Fatalf("conflicting options in one comment must remain blocked: %#v", got)
	}
	if got, conflicted := ParseAnswerSet("DESIGN-001: DESIGN-001-A\nDESIGN-001: DESIGN-001-B", questions); len(got) != 0 || !conflicted["DESIGN-001"] {
		t.Fatalf("conflict receipt = answers %#v, conflicts %#v", got, conflicted)
	}
	for _, invalid := range []string{"DESIGN-001: DESIGN-001-X", "DESIGN-001: 待定", "DESIGN-001: 自定义: 我再看看"} {
		if got, invalidated := ParseAnswerSet(invalid, questions); len(got) != 0 || !invalidated["DESIGN-001"] {
			t.Fatalf("invalid replacement %q = answers %#v, invalidated %#v", invalid, got, invalidated)
		}
	}
}

func TestUnansweredBlockingUsesRequestedQuestionAfterFreshBriefDropsAnsweredProductQuestion(t *testing.T) {
	productQuestion := Question{ID: "PROD-001", Role: RoleProduct, Blocking: true, Question: "范围？", Context: "完整背景", Options: []Option{{ID: "PROD-001-A"}, {ID: "PROD-001-B"}}}
	state := State{
		Brief:              Brief{Version: 1, Revision: 3, Questions: nil},
		Requests:           map[Role]RequestReceipt{RoleProduct: {Role: RoleProduct, Revision: 2}},
		RequestedQuestions: map[Role][]Question{RoleProduct: {productQuestion}},
		Answers:            map[string]Answer{},
	}
	if got := UnansweredBlocking(state, RoleProduct); len(got) != 1 || got[0].ID != "PROD-001" {
		t.Fatalf("requested product authority disappeared with fresh brief: %#v", got)
	}
}

func TestDecisionLogIncludesFactsWhenNoHumanDecisionWasNeeded(t *testing.T) {
	log := DecisionLogMarkdown(State{Brief: Brief{
		Version: 1,
		Summary: "导出失败后需要可恢复",
		Facts:   []Fact{{Text: "现有规范已经规定重试节奏", Evidence: "craft/state-coverage.md:84"}},
	}})
	for _, want := range []string{"导出失败后需要可恢复", "现有规范已经规定重试节奏", "craft/state-coverage.md:84", "真人决策**: 无"} {
		if !strings.Contains(log, want) {
			t.Fatalf("DecisionLogMarkdown() missing %q:\n%s", want, log)
		}
	}
}

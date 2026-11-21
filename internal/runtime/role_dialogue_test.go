package runtime

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/planestrict"
)

func TestParseRoleDialogueVerdictRequiresExactQuestionCoverage(t *testing.T) {
	questions := []planestrict.RoleQuestion{{ID: "ENG-001"}, {ID: "ENG-002"}}
	_, err := parseRoleDialogueVerdict(`{"reply":"继续确认", "questions":[{"id":"ENG-001","status":"still_open","answer":"","reason":"不明确"}]}`, questions)
	if err == nil || !strings.Contains(err.Error(), "every question") {
		t.Fatalf("parseRoleDialogueVerdict() error = %v, want missing coverage", err)
	}
}

func TestParseRoleDialogueVerdictAcceptsDelegationWithConcreteDecision(t *testing.T) {
	questions := []planestrict.RoleQuestion{{ID: "DESIGN-001"}}
	verdict, err := parseRoleDialogueVerdict(`{"reply":"收到，我采用右上角的次要按钮。", "questions":[{"id":"DESIGN-001","status":"delegated","answer":"采用右上角次要按钮","reason":"负责人明确授权 Looper 决定"}]}`, questions)
	if err != nil {
		t.Fatalf("parseRoleDialogueVerdict() error = %v", err)
	}
	if verdict.Questions[0].Status != "delegated" || !roleDialogueEvaluation(verdict).Resolved {
		t.Fatalf("verdict = %#v, want resolved delegation", verdict)
	}
}

func TestParseRoleDialogueVerdictNeverLetsChatResolveFormalProductSpec(t *testing.T) {
	questions := []planestrict.RoleQuestion{{ID: "PROD-000"}}
	_, err := parseRoleDialogueVerdict(`{"reply":"继续", "questions":[{"id":"PROD-000","status":"delegated","answer":"Looper 自己写","reason":"用户授权"}]}`, questions)
	if err == nil || !strings.Contains(err.Error(), "formal product spec") {
		t.Fatalf("parseRoleDialogueVerdict() error = %v, want formal spec guard", err)
	}
}

func TestDeterministicRoleReplyIDIsStableUUID(t *testing.T) {
	first := deterministicRoleReplyID("message-1")
	second := deterministicRoleReplyID("message-1")
	if first != second || len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Fatalf("deterministicRoleReplyID() = %q, %q", first, second)
	}
}

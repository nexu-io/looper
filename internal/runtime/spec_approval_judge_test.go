package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/infra/planedoc"
)

// fakeSpecLLM is a triage.LLM stand-in that returns a canned answer and records the
// prompt it was handed.
type fakeSpecLLM struct {
	out       string
	err       error
	gotPrompt string
	calls     int
}

func (f *fakeSpecLLM) Complete(_ context.Context, req triage.Request) (string, error) {
	f.calls++
	f.gotPrompt = req.Prompt
	return f.out, f.err
}

func TestParseSpecApprovalVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		raw      string
		wantErr  bool
		wantOK   bool
		wantBy   string
		wantReas string
	}{
		{"clean approve", `{"approved": true, "by": "杨瑾龙", "reason": "explicit go-ahead"}`, false, true, "杨瑾龙", "explicit go-ahead"},
		{"clean reject", `{"approved": false, "by": "", "reason": "asked to fix section 2 first"}`, false, false, "", "asked to fix section 2 first"},
		{"wrapped in prose", "Here is my verdict:\n```json\n{\"approved\": true, \"by\": \"eli\", \"reason\": \"ok\"}\n```\nDone.", false, true, "eli", "ok"},
		{"trims whitespace", `{"approved": true, "by": "  eli  ", "reason": "  yes  "}`, false, true, "eli", "yes"},
		{"no json", "I could not decide.", true, false, "", ""},
		{"malformed json", `{"approved": tru`, true, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseSpecApprovalVerdict(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got verdict %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Approved != tc.wantOK || v.By != tc.wantBy || v.Reason != tc.wantReas {
				t.Fatalf("got %+v, want approved=%v by=%q reason=%q", v, tc.wantOK, tc.wantBy, tc.wantReas)
			}
		})
	}
}

func TestJudgeSpecApproval(t *testing.T) {
	t.Parallel()
	comments := []planedoc.PageComment{
		{ID: "1", CommentStripped: "看着不错", DisplayName: "杨瑾龙"},
		{ID: "2", CommentStripped: "但先把第2节的验收标准补上", DisplayName: "chaoxiaoche"},
		{ID: "3", CommentStripped: "补好了,同意开工", DisplayName: "杨瑾龙"},
	}

	// The judge relays the LLM's structured verdict.
	llm := &fakeSpecLLM{out: `{"approved": true, "by": "杨瑾龙", "reason": "explicit go-ahead after the fix"}`}
	v, err := judgeSpecApproval(context.Background(), llm, comments, "/tmp/repo")
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if !v.Approved || v.By != "杨瑾龙" {
		t.Fatalf("got %+v, want approved by 杨瑾龙", v)
	}
	// The prompt must carry every human comment verbatim (transparent pass-through) and
	// the rule that rejects conditional/negated approvals.
	for _, want := range []string{"看着不错", "但先把第2节的验收标准补上", "补好了,同意开工", "UNCONDITIONAL", "CONDITIONAL"} {
		if !strings.Contains(llm.gotPrompt, want) {
			t.Fatalf("prompt missing %q; prompt=\n%s", want, llm.gotPrompt)
		}
	}

	// A negative verdict from the LLM stays not-approved.
	no := &fakeSpecLLM{out: `{"approved": false, "by": "", "reason": "still conditional"}`}
	if v, err := judgeSpecApproval(context.Background(), no, comments, "/tmp/repo"); err != nil || v.Approved {
		t.Fatalf("got approved=%v err=%v, want not approved", v.Approved, err)
	}

	// An LLM error propagates so the caller falls back to not-dispatching.
	boom := &fakeSpecLLM{err: errors.New("agent timed out")}
	if _, err := judgeSpecApproval(context.Background(), boom, comments, "/tmp/repo"); err == nil {
		t.Fatal("expected error to propagate from a failing LLM")
	}
}

func TestHashSpecComments(t *testing.T) {
	t.Parallel()
	base := []planedoc.PageComment{{ID: "1", CommentStripped: "看着不错"}}
	if hashSpecComments(base) != hashSpecComments([]planedoc.PageComment{{ID: "1", CommentStripped: "看着不错"}}) {
		t.Fatal("same comment set must hash equal (stable cursor)")
	}
	// A new comment (the burst that piled up) changes the hash → re-judge.
	withMore := append(append([]planedoc.PageComment{}, base...), planedoc.PageComment{ID: "2", CommentStripped: "同意开工"})
	if hashSpecComments(base) == hashSpecComments(withMore) {
		t.Fatal("a new comment must change the hash so the drain re-judges")
	}
	// Edited text on the same id also changes the hash.
	edited := []planedoc.PageComment{{ID: "1", CommentStripped: "其实还要再想想"}}
	if hashSpecComments(base) == hashSpecComments(edited) {
		t.Fatal("edited comment text must change the hash")
	}
}

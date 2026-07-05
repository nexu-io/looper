package loops

import "testing"

func TestHumanInboxAppendReadClearCap(t *testing.T) {
	base := `{"worker":{"title":"x"}}`
	m1, err := AppendHumanMessage(&base, HumanMessage{At: "t1", Text: "为什么推荐中文?"})
	if err != nil {
		t.Fatalf("append error = %v", err)
	}
	got := ReadHumanInbox(&m1)
	if len(got) != 1 || got[0].Text != "为什么推荐中文?" {
		t.Fatalf("ReadHumanInbox = %+v", got)
	}
	m2, _ := AppendHumanMessage(&m1, HumanMessage{At: "t2", Text: "那就用英文"})
	if len(ReadHumanInbox(&m2)) != 2 {
		t.Fatalf("want 2 messages")
	}
	// Cap.
	cur := m2
	for i := 0; i < humanInboxCap+5; i++ {
		cur, _ = AppendHumanMessage(&cur, HumanMessage{At: "t", Text: string(rune('a' + i))})
	}
	if n := len(ReadHumanInbox(&cur)); n != humanInboxCap {
		t.Fatalf("cap not enforced: %d want %d", n, humanInboxCap)
	}
	// Clear removes the key (and preserves others).
	cleared, _ := ClearHumanInbox(&cur)
	if len(ReadHumanInbox(&cleared)) != 0 {
		t.Fatal("inbox not cleared")
	}
}

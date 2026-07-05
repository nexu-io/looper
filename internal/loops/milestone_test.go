package loops

import "testing"

func TestMilestoneAppendReadCapDedup(t *testing.T) {
	var meta *string
	base := `{"worker":{"title":"x"}}`
	meta = &base
	m, err := AppendMilestone(meta, Milestone{At: "2026-07-05T10:00:00.000Z", Text: "已定夺:中文"})
	if err != nil {
		t.Fatalf("AppendMilestone error = %v", err)
	}
	got := ReadMilestones(&m)
	if len(got) != 1 || got[0].Text != "已定夺:中文" {
		t.Fatalf("ReadMilestones = %+v", got)
	}
	// Dedup: same text as previous is dropped.
	m2, _ := AppendMilestone(&m, Milestone{At: "2026-07-05T10:00:05.000Z", Text: "已定夺:中文"})
	if n := len(ReadMilestones(&m2)); n != 1 {
		t.Fatalf("dedup failed, len = %d", n)
	}
	// Cap: keep only the last milestonesCap.
	cur := m2
	for i := 0; i < milestonesCap+5; i++ {
		cur, _ = AppendMilestone(&cur, Milestone{At: "2026-07-05T10:00:00.000Z", Text: string(rune('a' + i))})
	}
	if n := len(ReadMilestones(&cur)); n != milestonesCap {
		t.Fatalf("cap not enforced, len = %d want %d", n, milestonesCap)
	}
}

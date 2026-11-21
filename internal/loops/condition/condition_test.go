package condition

import "testing"

func TestSetReadAndClearPreserveUnrelatedMetadata(t *testing.T) {
	raw := `{"worker":{"title":"keep"}}`
	encoded, err := Set(&raw, Record{Kind: ReviewUpdated, Since: "2026-07-15T12:00:00.000Z", Fingerprint: "head-1"})
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	record, ok := Read(&encoded)
	if !ok || record.Kind != ReviewUpdated || record.Fingerprint != "head-1" {
		t.Fatalf("Read() = %#v, %v", record, ok)
	}
	cleared, err := Clear(&encoded)
	if err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if _, ok := Read(&cleared); ok {
		t.Fatal("Read() found condition after Clear()")
	}
	if cleared != `{"worker":{"title":"keep"}}` {
		t.Fatalf("Clear() = %s, want unrelated metadata preserved", cleared)
	}
}

func TestReadRejectsUnknownCondition(t *testing.T) {
	raw := `{"blockedCondition":{"kind":"mystery"}}`
	if record, ok := Read(&raw); ok {
		t.Fatalf("Read() = %#v, true; want unknown condition rejected", record)
	}
}

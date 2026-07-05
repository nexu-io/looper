package loops

import "testing"

func TestTakeoverResumeRoundTrip(t *testing.T) {
	if _, ok := ReadTakeoverResume(nil); ok {
		t.Fatal("ReadTakeoverResume(nil) should be absent")
	}
	base := `{"worker":{"title":"x"}}`
	meta, err := WriteTakeoverResume(&base, TakeoverResume{SessionID: "019f-abc"})
	if err != nil {
		t.Fatalf("WriteTakeoverResume error = %v", err)
	}
	tr, ok := ReadTakeoverResume(&meta)
	if !ok || tr.SessionID != "019f-abc" {
		t.Fatalf("ReadTakeoverResume = %+v ok=%v", tr, ok)
	}
	// Preserves other keys.
	if got, _ := ReadTakeoverResume(&meta); got.SessionID == "" {
		t.Fatal("session id lost")
	}
	cleared, err := ClearTakeoverResume(&meta)
	if err != nil {
		t.Fatalf("ClearTakeoverResume error = %v", err)
	}
	if _, ok := ReadTakeoverResume(&cleared); ok {
		t.Fatal("marker should be gone after clear")
	}
}

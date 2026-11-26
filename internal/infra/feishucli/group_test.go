package feishucli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestGroupDescriptionFitsAPIAndAnnouncementIsPinned(t *testing.T) {
	var calls []string
	c := Client{}
	c.Run = func(_ context.Context, _ string, input []byte, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if strings.Contains(call, "+chat-create") {
			for i, arg := range args {
				if arg == "--description" && len([]rune(args[i+1])) != 100 {
					t.Fatalf("description length=%d", len([]rune(args[i+1])))
				}
			}
			return []byte(`{"ok":true,"data":{"chat_id":"oc_group"}}`), nil
		}
		if strings.HasPrefix(call, "api GET ") {
			return []byte(`{"ok":true,"data":{"items":[{"block_id":"oc_group","block_type":1}]}}`), nil
		}
		if strings.Contains(call, "/children") {
			var body map[string]any
			if err := json.Unmarshal(input, &body); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(input), "https://plane.example/module") {
				t.Fatal("announcement lost module link")
			}
		}
		return []byte(`{"ok":true,"data":{}}`), nil
	}
	if _, err := c.CreateGroup(context.Background(), "Module", strings.Repeat("长", 150), []string{"ou_owner"}); err != nil {
		t.Fatal(err)
	}
	if err := c.InitializeAnnouncement(context.Background(), "oc_group", "Module", "https://plane.example/module", "One topic per task"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 || !strings.Contains(calls[3], "top_notice/put_top_notice") || !strings.Contains(calls[3], `"action_type":"2"`) {
		t.Fatalf("announcement not pinned: %v", calls)
	}
}

package notify

import (
	"context"
	"testing"
)

func TestGatewayHumanAttentionSkipsFeishuAppDelivery(t *testing.T) {
	// awaiting_human without a Feishu ask reaches Feishu app (remote operator alert).
	// awaiting_human with a delivered Feishu ask is local-only (no duplicate card).
	// manual_intervention must still reach Feishu app (no HITL ask duplicate).
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

	ctx := context.Background()

	t.Run("awaiting_human without feishu ask reaches app", func(t *testing.T) {
		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)
		gateway.config.InApp = true

		records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
			LoopSeq:  7,
			Reason:   HumanAttentionAwaitingHuman,
			EntryKey: "run:hitl_feishu_remote",
		})
		if got := notificationStatus(records, "in_app"); got != "success" {
			t.Fatalf("in_app status = %q, want success; records=%#v", got, records)
		}
		if got := notificationStatus(records, "feishu_app"); got != "success" {
			t.Fatalf("feishu_app status = %q, want success for non-Feishu awaiting park", got)
		}
		if len(calls) == 0 {
			t.Fatal("awaiting_human without Feishu ask should reach Feishu app HTTP")
		}
	})

	t.Run("awaiting_human with feishu ask stays local-only", func(t *testing.T) {
		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)
		gateway.config.InApp = true
		gateway.state.askCards = map[string]askCardState{
			"loop_with_ask": {msgID: "om_ask_local", card: HITLAskCard{LoopID: "loop_with_ask"}},
		}

		records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
			LoopID:   "loop_with_ask",
			LoopSeq:  7,
			Reason:   HumanAttentionAwaitingHuman,
			EntryKey: "run:hitl_feishu_dedupe",
		})
		if got := notificationStatus(records, "in_app"); got != "success" {
			t.Fatalf("in_app status = %q, want success; records=%#v", got, records)
		}
		if got := notificationStatus(records, "feishu_app"); got != "" {
			t.Fatalf("feishu_app status = %q, want absent (local-only when Feishu ask exists)", got)
		}
		if len(calls) != 0 {
			t.Fatalf("feishu HTTP calls = %d, want 0 when Feishu ask already delivered", len(calls))
		}
	})

	t.Run("manual_intervention reaches app", func(t *testing.T) {
		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)
		gateway.config.InApp = true

		// Hard manual_intervention parks have no HITL ask duplicate — remote delivery stays on.
		manual := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
			LoopSeq:  8,
			Reason:   HumanAttentionManualIntervention,
			EntryKey: "queue:q_manual:t1",
		})
		if got := notificationStatus(manual, "in_app"); got != "success" {
			t.Fatalf("manual in_app status = %q, want success; records=%#v", got, manual)
		}
		if got := notificationStatus(manual, "feishu_app"); got != "success" {
			t.Fatalf("manual feishu_app status = %q, want success (remote alert for hard hold)", got)
		}
		if len(calls) == 0 {
			t.Fatal("manual_intervention NotifyHumanAttention should reach Feishu app HTTP")
		}

		// Ordinary Notify still delivers to Feishu app mode.
		ordinary := gateway.Notify(ctx, SystemNotificationPayload{
			Level:     "action_required",
			Title:     "Ordinary",
			Body:      "not human-attention",
			DedupeKey: "ordinary:1",
		})
		if got := notificationStatus(ordinary, "feishu_app"); got != "success" {
			t.Fatalf("ordinary feishu_app status = %q, want success", got)
		}
	})
}

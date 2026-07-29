package notify

import (
	"context"
	"testing"
)

func TestGatewayHumanAttentionSkipsFeishuAppDelivery(t *testing.T) {
	// awaiting_human is local-only so Feishu HITL ask cards are not duplicated.
	// manual_intervention must still reach Feishu app (no HITL ask duplicate).
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

	ctx := context.Background()
	var calls []capturedFeishuCall
	gateway := newFeishuAppGateway(t, appModeConfig(), &calls)
	// Enable in_app so permanent entry dedupe still has a durable record.
	gateway.config.InApp = true

	records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopSeq:  7,
		Reason:   HumanAttentionAwaitingHuman,
		EntryKey: "run:hitl_feishu_dedupe",
	})
	if got := notificationStatus(records, "in_app"); got != "success" {
		t.Fatalf("in_app status = %q, want success; records=%#v", got, records)
	}
	if got := notificationStatus(records, "feishu_app"); got != "" {
		t.Fatalf("feishu_app status = %q, want absent (local-only awaiting_human)", got)
	}
	if len(calls) != 0 {
		t.Fatalf("feishu HTTP calls = %d, want 0 for awaiting_human NotifyHumanAttention", len(calls))
	}

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
}

package collab

import "testing"

func TestBridgeTaskExplicitlyStoppedSuppressesExternalObservers(t *testing.T) {
	tests := []struct {
		name  string
		state Outcome
		want  bool
	}{
		{
			name:  "explicit stop waits for resume",
			state: Outcome{State: "needs_input", Summary: "PR 仍显示已合并；保持停止，未确认验收发布", NextStep: "保持停止，等待用户明确恢复后核对实际发布和人工验收。"},
			want:  true,
		},
		{
			name:  "ordinary acceptance wait remains observable",
			state: Outcome{State: "needs_input", Summary: "PR 已就绪，等待 QA 验收", NextStep: "QA 回复后继续核对发布。"},
			want:  false,
		},
		{
			name:  "completed outcome is not a stop marker",
			state: Outcome{State: "completed", Summary: "已完成", NextStep: "无需继续。"},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bridgeTaskExplicitlyStopped(bridgeTask{Outcome: tt.state}); got != tt.want {
				t.Fatalf("bridgeTaskExplicitlyStopped() = %v, want %v", got, tt.want)
			}
		})
	}
}

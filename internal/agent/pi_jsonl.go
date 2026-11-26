package agent

import (
	"encoding/json"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// Pi's --mode json streams activity while text print mode buffers until exit.
// Only completed assistant messages own the result; tool output and deltas do not.
func piJSONL(stdout string) (string, []string) {
	var texts, activity []string
	for _, line := range strings.Split(stdout, "\n") {
		var event struct {
			Type     string `json:"type"`
			ToolName string `json:"toolName"`
			Message  struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Type == "tool_execution_start" && event.ToolName != "" {
			activity = append(activity, "执行工具："+event.ToolName)
		}
		if event.Type != "message_end" || event.Message.Role != "assistant" {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				texts = append(texts, block.Text)
				for _, line := range strings.Split(block.Text, "\n") {
					if !strings.HasPrefix(strings.TrimSpace(line), CompletionMarkerPrefix) {
						activity = append(activity, line)
					}
				}
			}
		}
	}
	return strings.Join(texts, "\n"), activity
}

func (x *execution) piJSONMode() bool {
	if x.executor == nil || x.executor.effectiveConfig(x.input).Vendor != config.AgentVendorPi {
		return false
	}
	for i, arg := range x.args {
		if arg == "--mode=json" || (arg == "--mode" && i+1 < len(x.args) && x.args[i+1] == "json") {
			return true
		}
	}
	return false
}

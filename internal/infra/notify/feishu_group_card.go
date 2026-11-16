package notify

import (
	"encoding/json"
	"strings"
)

// FeishuGroupContext describes the shared collaboration scope, not task progress.
type FeishuGroupContext struct {
	Kind string
	Name string
	URL  string
	Rule string
}

func (c FeishuGroupContext) Description() string {
	parts := []string{c.Kind + " · " + c.Name}
	if c.URL != "" {
		parts = append(parts, c.URL)
	}
	return strings.Join(append(parts, c.Rule), "\n")
}

func BuildFeishuGroupCard(c FeishuGroupContext) ([]byte, error) {
	name := c.Name
	if c.URL != "" {
		name = "[" + name + "](" + c.URL + ")"
	}
	elements := []any{
		taskCardMarkdown("<text_tag color='blue'>"+c.Kind+"</text_tag> **"+name+"**", "normal"),
		taskCardMarkdown(c.Rule, "normal"),
	}
	return json.Marshal(map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true, "summary": map[string]any{"content": c.Kind + " · " + c.Name}}, "body": map[string]any{"padding": "12px", "vertical_spacing": "8px", "elements": elements}})
}

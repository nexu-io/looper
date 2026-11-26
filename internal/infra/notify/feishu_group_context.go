package notify

import "strings"

// FeishuGroupContext describes the shared collaboration scope, not task progress.
type FeishuGroupContext struct {
	Kind string
	Name string
	URL  string
	Rule string
}

func (c FeishuGroupContext) Description() string {
	// Links and usage instructions belong in the announcement; Feishu limits
	// the chat description to 100 characters.
	return strings.TrimSpace(c.Kind + " · " + c.Name)
}

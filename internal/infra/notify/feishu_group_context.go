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
	parts := []string{c.Kind + " · " + c.Name}
	if c.URL != "" {
		parts = append(parts, c.URL)
	}
	return strings.Join(append(parts, c.Rule), "\n")
}

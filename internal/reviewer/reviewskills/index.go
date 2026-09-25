package reviewskills

import (
	"fmt"
	"strings"
)

const previewSkillPath = "<run-local builtin skill path>"

const builtinSkillDescription = "Default Looper review method for implementation and spec PRs. Read before reviewing. Covers correctness, tests, concurrency, contracts, and comment quality."

// FormatIndex renders the prompt section that points the agent at resolved skills.
func FormatIndex(entries []Entry) string {
	var b strings.Builder
	b.WriteString("Review method skills:\n")
	b.WriteString("Every listed skill MUST be read from the given absolute paths before reviewing.\n")
	b.WriteString("required: true means the skill had to exist; required: false means it was optional and present. Do not skip a listed skill.\n")
	b.WriteString("Skill selection does not shrink review scope; if no specialty skill matches, still complete the base review using looper-review.")
	for _, entry := range entries {
		fmt.Fprintf(&b, "\n- name: %s\n  source: %s\n  required: %t\n  path: %s\n  description: %s", entry.Name, entry.Source, entry.Required, entry.Path, entry.Description)
	}
	return b.String()
}

// PreviewIndexPlaceholder describes the builtin skill without fabricating a filesystem path.
func PreviewIndexPlaceholder() string {
	return FormatIndex([]Entry{{
		Name:        "looper-review",
		Description: builtinSkillDescription,
		Path:        previewSkillPath,
		Source:      "builtin",
		Required:    true,
	}})
}

// ResumeReminder restates current absolute skill paths for a native resume prompt.
func ResumeReminder(entries []Entry) string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if path := strings.TrimSpace(entry.Path); path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	return "Review skills still required at: " + strings.Join(paths, ", ")
}

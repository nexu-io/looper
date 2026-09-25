package reviewskills

import (
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

const previewSkillPath = "<run-local builtin skill path>"

const builtinSkillDescription = "Default Looper review method for implementation and spec PRs. Read before reviewing. Covers correctness, tests, concurrency, contracts, and comment quality."

// FormatIndex renders the prompt section that points the agent at resolved skills.
func FormatIndex(entries []Entry) string {
	var b strings.Builder
	b.WriteString("Review method skills:\n")
	b.WriteString("Every listed skill MUST be read from the given absolute paths before reviewing.\n")
	b.WriteString("required: true means the skill had to exist; required: false means it was optional and present. Do not skip a listed skill.\n")
	b.WriteString("Skill selection does not shrink review scope; complete the full review using the resolved methods listed below.")
	for _, entry := range entries {
		if entry.Name == "looper-review" {
			b.WriteString(" If no specialty skill matches, still complete the base review using looper-review.")
			break
		}
	}
	for _, entry := range entries {
		fmt.Fprintf(&b, "\n- name: %s\n  source: %s\n  required: %t\n  path: %s\n  description: %s", entry.Name, entry.Source, entry.Required, entry.Path, entry.Description)
	}
	return b.String()
}

// PreviewIndexPlaceholder describes the default selection without runtime paths.
func PreviewIndexPlaceholder() string {
	return PreviewIndex(config.ReviewerSkillsConfig{})
}

// PreviewIndex describes configured references without claiming runtime availability.
func PreviewIndex(cfg config.ReviewerSkillsConfig) string {
	var b strings.Builder
	b.WriteString("Review method skills:\nPreview only: configured references and absolute paths are resolved at review time in the prepared worktree. Required references must resolve; optional references are included only if present.\n")
	seen := make(map[string]bool)
	if cfg.Mode != config.ReviewerSkillsModeReplace {
		fmt.Fprintf(&b, "- name: looper-review\n  source: builtin\n  required: true\n  path: %s\n  description: %s\n", previewSkillPath, builtinSkillDescription)
		seen["looper-review"] = true
	}
	for _, selection := range []struct {
		refs     []string
		required bool
	}{{cfg.Required, true}, {cfg.Optional, false}} {
		for _, ref := range selection.refs {
			ref = strings.TrimSpace(ref)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			fmt.Fprintf(&b, "- ref: %q\n  required: %t\n  path: <resolved at review time>\n", ref, selection.required)
		}
	}
	return strings.TrimSpace(b.String())
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

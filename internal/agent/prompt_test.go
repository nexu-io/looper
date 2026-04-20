package agent

import (
	"strings"
	"testing"
)

func TestAppendCompletionInstruction(t *testing.T) {
	t.Parallel()

	prompt := AppendCompletionInstruction("do the work")
	for _, needle := range []string{
		"do the work",
		"When finished, print exactly one final line to stdout in this format:",
		`__LOOPER_RESULT__={"summary":"<one-sentence summary>"}`,
		"Do not wrap that line in markdown.",
		"Do not print anything after that line.",
	} {
		if !strings.Contains(prompt, needle) {
			t.Fatalf("prompt = %q, want %q", prompt, needle)
		}
	}
}

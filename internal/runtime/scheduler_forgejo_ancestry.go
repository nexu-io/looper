package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func forgejoCompareCommits(ctx context.Context, cfg *config.Config, repo, cwd, base, head string) (string, error) {
	base, head = strings.TrimSpace(base), strings.TrimSpace(head)
	if base != "" && strings.EqualFold(base, head) {
		return "identical", nil
	}
	gitPath, cwd, err := forgejoGitLocation(cfg, repo, cwd)
	if err != nil {
		return "", err
	}
	if err := ensureForgejoCommits(ctx, gitPath, cwd, []string{base, head}); err != nil {
		return "", withForgejoConflictBoundary(err)
	}
	for i, pair := range [][2]string{{base, head}, {head, base}} {
		result, err := shell.Run(ctx, shell.Options{Command: gitPath, CWD: cwd, Args: []string{"merge-base", "--is-ancestor", pair[0], pair[1]}})
		if err == nil {
			if i == 0 {
				return "ahead", nil
			}
			return "behind", nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var commandErr *shell.CommandExecutionError
		if !errors.As(err, &commandErr) || result.ExitCode != 1 {
			return "", withForgejoConflictBoundary(fmt.Errorf("compare Forgejo commit ancestry: %w", err))
		}
	}
	// A shallow boundary hides parents even when both exact commit objects are
	// present. Positive ancestry is conclusive; two negative answers are not.
	result, err := shell.Run(ctx, shell.Options{Command: gitPath, CWD: cwd, Args: []string{"rev-parse", "--is-shallow-repository"}})
	if err != nil {
		return "", withForgejoConflictBoundary(fmt.Errorf("inspect Forgejo ancestry history: %w", err))
	}
	if strings.TrimSpace(result.Stdout) != "false" {
		return "", withForgejoConflictBoundary(fmt.Errorf("Forgejo commit ancestry is inconclusive in a shallow repository; fetch complete history before retrying"))
	}
	return "diverged", nil
}

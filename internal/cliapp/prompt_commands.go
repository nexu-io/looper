package cliapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/lifecycle"
	"github.com/spf13/cobra"
)

func (r *commandRuntime) promptPreview(cmd *cobra.Command, args []string) error {
	projectID := strings.TrimSpace(getStringFlag(cmd, "project"))
	role := strings.TrimSpace(getStringFlag(cmd, "role"))
	if projectID == "" {
		return fmt.Errorf("--project is required")
	}
	if role == "" {
		return fmt.Errorf("--role is required")
	}
	if !isPreviewInstructionRole(role) {
		return fmt.Errorf("--role must be one of: planner, worker, reviewer, fixer")
	}
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	project, err := configuredProjectByID(loaded.Config.Projects, projectID)
	if err != nil {
		return err
	}
	block := config.BuildCustomInstructionBlock(loaded.Config, projectID, role)
	order := []string{"Looper base role prompt"}
	sections := []string{
		"Looper base role prompt\n" + previewBaseRole(role),
	}
	if repoContext := previewRepositoryContextForRole(role, project.RepoPath); repoContext != "" {
		sections = append(sections, "Repository context / AGENTS.md\n"+repoContext)
		order = append(order, "repository context / AGENTS.md")
	}
	if block.Text != "" {
		sections = append(sections, previewInstructionSources(block)+"\n\n"+block.Text)
	} else {
		sections = append(sections, "Custom instructions\nSources: none\n(no custom instructions applied)")
	}
	order = append(order, "custom instructions", "lifecycle / safety constraints", "completion / output contract")
	sections = append(sections,
		"Lifecycle / safety constraints\n"+previewLifecycleSafety(role, project, loaded.Config),
		"Completion / output contract\n"+agent.AppendCompletionInstruction("<role prompt assembled above>"),
	)
	prompt := strings.Join(sections, "\n\n---\n\n")
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"project": projectID, "role": role, "order": order, "customInstructions": block, "prompt": prompt})
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), prompt)
	return err
}

func isPreviewInstructionRole(role string) bool {
	switch role {
	case "planner", "worker", "reviewer", "fixer":
		return true
	default:
		return false
	}
}

func previewInstructionSources(block config.CustomInstructionBlock) string {
	if len(block.Sources) == 0 {
		return "Custom instruction sources: none"
	}
	lines := []string{"Custom instruction sources:"}
	for _, source := range block.Sources {
		lines = append(lines, fmt.Sprintf("- %s: %s", source.Kind, source.Path))
	}
	return strings.Join(lines, "\n")
}

func previewLifecycleSafety(role string, project config.ProjectRefConfig, cfg config.Config) string {
	branch := "<branch>"
	baseBranch := promptBaseBranch(project.BaseBranch, cfg.Defaults.BaseBranch)
	allowRemote := cfg.Defaults.AllowAutoPush
	agentRuntime := promptAgentRuntime(cfg)
	agentModel := promptDerefString(cfg.Agent.Model)
	switch role {
	case "reviewer":
		return previewReviewerLifecycleSafety(promptDerefString(cfg.Tools.LooperPath), cfg.Disclosure, agentRuntime, agentModel)
	case "worker":
		if !previewWorkerAllowsRemoteLifecycle(cfg) {
			return previewNoRemoteLifecyclePromptInstruction(role, branch, baseBranch, cfg.Disclosure, agentRuntime, agentModel)
		}
		return lifecycle.PromptInstruction(role, branch, baseBranch, true, true, cfg.Disclosure, agentRuntime, agentModel)
	case "fixer":
		branch = ""
		baseBranch = ""
		if !allowRemote {
			return "Only repair Looper-provided fix items; do not change remote pull request state unless lifecycle policy allows it.\n\n" + previewNoRemoteLifecyclePromptInstruction(role, branch, baseBranch, cfg.Disclosure, agentRuntime, agentModel)
		}
		return "Only repair Looper-provided fix items; do not change remote pull request state unless lifecycle policy allows it.\n\n" + lifecycle.PromptInstruction(role, branch, baseBranch, true, false, cfg.Disclosure, agentRuntime, agentModel)
	default:
		if !allowRemote {
			return previewNoRemoteLifecyclePromptInstruction(role, branch, baseBranch, cfg.Disclosure, agentRuntime, agentModel)
		}
		return lifecycle.PromptInstruction(role, branch, baseBranch, true, true, cfg.Disclosure, agentRuntime, agentModel)
	}
}

func promptAgentRuntime(cfg config.Config) string {
	if cfg.Agent.Vendor == nil {
		return ""
	}
	return string(*cfg.Agent.Vendor)
}

func previewWorkerAllowsRemoteLifecycle(cfg config.Config) bool {
	return cfg.Defaults.AllowAutoPush && cfg.Defaults.OpenPRStrategy != config.OpenPRStrategyManual
}

func previewReviewerLifecycleSafety(looperCLIPath string, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) string {
	minimalSeedContract := strings.Join([]string{
		"PR handoff contract: Looper provides a minimal PR seed by default: repo, pr_number/url, base_ref, head_ref, head_sha, expected state/draft status, task intent, and optional scope such as paths, comment IDs, review thread IDs, or constraints.",
		"Child agents must fetch PR details on demand with `gh`: use `gh pr view <pr-url> -R <repo> --json number,title,body,state,isDraft,baseRefName,headRefName,headRefOid,url,labels` with the seeded PR URL or number plus repository for metadata and drift checks; `gh pr diff <pr-url> -R <repo> --name-only` before scoped diffs; fetch the full patch with `gh pr diff <pr-url> -R <repo> --patch` and filter locally or fetch refs and run `git diff <base>...<head> -- <path>` for relevant files only; paginated `gh api repos/{owner}/{repo}/pulls/{number}/comments --paginate`, `gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate`, and `gh api repos/{owner}/{repo}/issues/{number}/comments --paginate` for review feedback; and `gh pr checks <pr-url> -R <repo>` only when CI status matters.",
		"Safeguards: validate live headRefOid against seeded head_sha, baseRefName against seeded base_ref, and PR state/draft status against expectations before acting and before final conclusions; fail fast with structured auth, network, rate_limit, or pr_drift errors; never rely only on `gh pr view --comments`; do not pass full diffs or full comment dumps through parent context by default.",
	}, "\n")
	if strings.TrimSpace(looperCLIPath) == "" {
		return strings.Join([]string{
			minimalSeedContract,
			"GitHub operation contract: a trusted Looper CLI path was not detected for reviewer runs, so agents cannot safely publish a GitHub review. Do not call PATH-based `looper`, repository-local `go run ./cmd/looper`, `gh api repos/.../pulls/.../reviews`, or `gh pr review` directly; exit non-zero with the exact message `trusted looper review submit wrapper unavailable`.",
			lifecycle.DisclosurePromptInstruction("reviewer", disclosureCfg, agentRuntime, agentModel),
		}, "\n")
	}
	return strings.Join([]string{
		minimalSeedContract,
		"Use Looper's trusted `looper review submit` wrapper for review submission. Do not bypass approval, publication, or disclosure policy.",
		"GitHub operation contract: submit exactly one PR review for the run through the trusted Looper CLI review-submit wrapper, with review JSON on stdin. The wrapper validates inline anchors against the live PR diff before it calls GitHub; do not use PATH-based `looper`, repository-local `go run ./cmd/looper`, `gh api repos/.../pulls/.../reviews`, or `gh pr review` directly for review submission.",
		"Before posting, confirm the PR is still open, the head SHA still matches the expected head SHA, and the current GitHub user is still requested for review unless the run is manual.",
		"Every review body must include exactly one stable idempotency marker with id, head, and outcome fields: `<!-- looper:review id=... head=... outcome=clean|actionable -->`.",
		"For clean reviews, submit COMMENT unless reviewer policy allows APPROVE; for actionable reviews, submit COMMENT with resolvable inline comments whenever anchors can be validated.",
		lifecycle.DisclosurePromptInstruction("reviewer", disclosureCfg, agentRuntime, agentModel),
	}, "\n")
}

func previewNoRemoteLifecyclePromptInstruction(runner, branch, baseBranch string, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) string {
	return strings.Join([]string{
		"Agent-managed git/PR lifecycle policy: remote actions disabled by Looper configuration.",
		"Before finishing: inspect git status, staged and unstaged diffs, untracked files, and recent commit style; commit only relevant non-secret changes if needed; do not push branches, create pull requests, update pull request metadata, or otherwise change remote review state.",
		lifecycle.DisclosurePromptInstruction(runner, disclosureCfg, agentRuntime, agentModel),
		"Include a git_pr_lifecycle object in the final " + "__LOOPER_RESULT__" + " JSON with branch, baseBranch, commitShas, pushed, prNumber, prUrl, prAdopted, and actions {commit,push,pr}; use action source \"agent\" only for local commits you completed and \"none\" for disabled remote actions.",
		fmt.Sprintf("Expected lifecycle runner=%q branch=%q baseBranch=%q expectPush=%t expectPR=%t fallbackAllowed=%t.", runner, branch, baseBranch, false, false, true),
	}, "\n")
}

func configuredProjectByID(projects []config.ProjectRefConfig, id string) (config.ProjectRefConfig, error) {
	for _, project := range projects {
		if project.ID == id {
			return project, nil
		}
	}
	return config.ProjectRefConfig{}, fmt.Errorf("project not found: %s", id)
}

func previewBaseRole(role string) string {
	switch role {
	case "planner":
		return "Write a planning spec for the target GitHub issue."
	case "worker":
		return "Implement the requested work and prepare a pull request when policy allows."
	case "reviewer":
		return "Review the target pull request through Looper's trusted review workflow."
	case "fixer":
		return "Repair only the Looper-provided review findings."
	default:
		return "Unknown role; config validation accepts planner, worker, reviewer, and fixer."
	}
}

func previewRepositoryContext(repoPath string) string {
	if strings.TrimSpace(repoPath) == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(repoPath, "AGENTS.md"))
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		return "Repository path: " + repoPath
	}
	return "Repository path: " + repoPath + "\n\nAGENTS.md:\n" + strings.TrimSpace(string(raw))
}

func previewRepositoryContextForRole(role string, repoPath string) string {
	if role != "planner" {
		return ""
	}
	return previewRepositoryContext(repoPath)
}

func promptBaseBranch(projectBase *string, defaultBase string) string {
	if projectBase != nil && strings.TrimSpace(*projectBase) != "" {
		return *projectBase
	}
	return defaultBase
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func promptDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

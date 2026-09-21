package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
)

type changedFile struct {
	Status  string
	Path    string
	OldPath string
}

type fileGroup struct {
	ID    string
	Paths []string
}

func parseNameStatus(output string) []changedFile {
	var files []changedFile
	seen := make(map[string]struct{})
	fields := strings.Split(output, "\x00")
	i := 0
	for i < len(fields) {
		status := fields[i]
		i++
		if status == "" {
			continue
		}
		kind := status[:1]
		switch kind {
		case "R", "C":
			if i+1 >= len(fields) {
				return files
			}
			oldPath := fields[i]
			newPath := fields[i+1]
			i += 2
			if newPath == "" {
				continue
			}
			if _, ok := seen[newPath]; ok {
				continue
			}
			seen[newPath] = struct{}{}
			files = append(files, changedFile{Status: status, Path: newPath, OldPath: oldPath})
			if oldPath != "" && oldPath != newPath {
				if _, ok := seen[oldPath]; !ok {
					seen[oldPath] = struct{}{}
					files = append(files, changedFile{Status: "D", Path: oldPath})
				}
			}
		default:
			if i >= len(fields) {
				return files
			}
			path := fields[i]
			i++
			if path == "" {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			files = append(files, changedFile{Status: status, Path: path})
		}
	}
	return files
}

func groupChangedFiles(files []changedFile) []fileGroup {
	buckets := map[string][]string{}
	var other []string
	for _, file := range files {
		path := file.Path
		if path == "" {
			continue
		}
		if strings.HasSuffix(path, ".go") {
			dir := filepath.ToSlash(filepath.Dir(path))
			if dir == "." {
				dir = ""
			}
			key := "go:" + dir
			buckets[key] = append(buckets[key], path)
			continue
		}
		other = append(other, path)
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	groups := make([]fileGroup, 0, len(keys)+1)
	for _, key := range keys {
		paths := buckets[key]
		sort.Strings(paths)
		groups = append(groups, fileGroup{ID: key, Paths: paths})
	}
	if len(other) > 0 {
		sort.Strings(other)
		groups = append(groups, fileGroup{ID: "other", Paths: other})
	}
	return groups
}

func groupingPlanText(groups []fileGroup, all []changedFile) string {
	if len(groups) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Related-file group plan (opt-in execution): every changed path including deletions is assigned. Subtasks return findings only and must not publish. Other changed files remain in scope for cross-group contracts.\n")
	allPaths := make([]string, 0, len(all))
	for _, file := range all {
		allPaths = append(allPaths, file.Path)
	}
	sort.Strings(allPaths)
	b.WriteString("All changed paths: " + strings.Join(allPaths, ", ") + "\n")
	for _, group := range groups {
		b.WriteString("- " + group.ID + ": " + strings.Join(group.Paths, ", ") + "\n")
	}
	return strings.TrimSpace(b.String())
}

func mergeGroupedFindings(batches ...[]reviewerCommentOnlyFindingResult) []reviewerCommentOnlyFindingResult {
	var out []reviewerCommentOnlyFindingResult
	for _, batch := range batches {
		out = append(out, batch...)
	}
	return out
}

func otherPaths(group fileGroup, all []changedFile) []string {
	inGroup := make(map[string]struct{}, len(group.Paths))
	for _, path := range group.Paths {
		inGroup[path] = struct{}{}
	}
	var other []string
	for _, file := range all {
		if _, ok := inGroup[file.Path]; ok {
			continue
		}
		other = append(other, file.Path)
	}
	sort.Strings(other)
	return other
}

func groupingGitPath(r *Runner) string {
	if r != nil && r.projectRoleConfig != nil && r.projectRoleConfig.Tools.GitPath != nil {
		if path := strings.TrimSpace(*r.projectRoleConfig.Tools.GitPath); path != "" {
			return path
		}
	}
	return "git"
}

func listChangedPaths(ctx context.Context, gitPath, worktree, base, head string) ([]changedFile, error) {
	if strings.TrimSpace(worktree) == "" || strings.TrimSpace(base) == "" || strings.TrimSpace(head) == "" {
		return nil, fmt.Errorf("worktree and base/head SHAs are required")
	}
	if strings.TrimSpace(gitPath) == "" {
		gitPath = "git"
	}
	cmd := exec.CommandContext(ctx, gitPath, "-C", worktree, "diff", "--name-status", "-z", "-M", "--no-ext-diff", "--no-color", base+"..."+head)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseNameStatus(string(out)), nil
}

func relatedFileGroupsConfig(r *Runner, projectID string) config.ReviewerRelatedFileGroupsConfig {
	cfg := config.ReviewerRelatedFileGroupsConfig{MinChangedFiles: 24}
	if r != nil && r.projectRoleConfig != nil {
		cfg = config.ProjectRoleConfigs(*r.projectRoleConfig, projectID).Reviewer.Behavior.RelatedFileGroups
	}
	return cfg
}

func (r *Runner) applyRelatedFileGroups(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, worktreePath, skillIndex string) (string, error) {
	cfg := relatedFileGroupsConfig(r, input.Project.ID)
	if !cfg.Enabled {
		return skillIndex, nil
	}
	if checkpoint.Snapshot == nil {
		return skillIndex, nil
	}
	base := strings.TrimSpace(checkpoint.Snapshot.BaseSHA)
	head := strings.TrimSpace(checkpoint.Snapshot.HeadSHA)
	if last, _ := stringFromAny(parseJSONObject(input.Loop.MetadataJSON)["lastPublishedHeadSha"]); last != "" && last != head {
		base = last
	}
	files, err := listChangedPaths(ctx, groupingGitPath(r), worktreePath, base, head)
	if err != nil {
		return "", fmt.Errorf("related-file groups: enumerate changed paths: %w", err)
	}
	if len(files) < cfg.MinChangedFiles {
		return skillIndex, nil
	}
	groups := groupChangedFiles(files)
	plan := groupingPlanText(groups, files)
	if plan != "" {
		skillIndex = strings.TrimSpace(skillIndex + "\n\n" + plan)
	}
	findings, err := r.runGroupedFindingAgents(ctx, input, worktreePath, groups, files, base, head)
	if err != nil {
		return "", err
	}
	if len(findings) == 0 {
		return skillIndex, nil
	}
	payload, err := json.Marshal(findings)
	if err != nil {
		return "", err
	}
	skillIndex = skillIndex + "\n\nSubtask findings to merge (do not publish from subtasks; dedupe and apply disposition, then publish once through the existing wrapper):\n" + string(payload)
	return skillIndex, nil
}

func (r *Runner) runGroupedFindingAgents(ctx context.Context, input stepInput, worktreePath string, groups []fileGroup, all []changedFile, base, head string) ([]reviewerCommentOnlyFindingResult, error) {
	if r == nil || r.agentExecutor == nil {
		return nil, nil
	}
	agentVendor, agentModel, _, useSnapshot, err := r.identityFromRun(input.Run)
	if err != nil {
		return nil, fmt.Errorf("grouped review: resolve run agent identity: %w", err)
	}
	useSnap, snapVendor, snapModel := agentRunSnapshotFields(agentVendor, agentModel, useSnapshot)
	var batches [][]reviewerCommentOnlyFindingResult
	for _, group := range groups {
		prompt := groupedFindingPrompt(group, all, base, head)
		execution, err := r.agentExecutor.Start(ctx, AgentRunInput{
			ExecutionID: eventlog.NewEventID("agent"), ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID,
			Prompt: prompt, WorkingDirectory: worktreePath, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout,
			Metadata:       map[string]any{"loopType": "reviewer", "phase": "review-group", "groupId": group.ID, "repo": input.Repo, "prNumber": input.PRNumber},
			UseSnapshot:    useSnap,
			SnapshotVendor: snapVendor,
			SnapshotModel:  snapModel,
		})
		if err != nil {
			return nil, fmt.Errorf("grouped review %s: %w", group.ID, err)
		}
		result, err := execution.Wait(ctx)
		if err != nil {
			return nil, fmt.Errorf("grouped review %s: %w", group.ID, err)
		}
		if result.Status != "completed" {
			return nil, fmt.Errorf("grouped review %s: agent %s", group.ID, result.Status)
		}
		if err := r.assertGroupedWorktreeUnchanged(ctx, worktreePath, head); err != nil {
			return nil, fmt.Errorf("grouped review %s: %w", group.ID, err)
		}
		completion, parseErr := parseReviewerCommentOnlyCompletion(result)
		if parseErr != nil {
			return nil, fmt.Errorf("grouped review %s: %w", group.ID, parseErr)
		}
		batches = append(batches, completion.Findings)
	}
	return mergeGroupedFindings(batches...), nil
}

func (r *Runner) assertGroupedWorktreeUnchanged(ctx context.Context, worktree, head string) error {
	gitPath := groupingGitPath(r)
	rev := exec.CommandContext(ctx, gitPath, "-C", worktree, "rev-parse", "HEAD")
	out, err := rev.Output()
	if err != nil {
		return fmt.Errorf("worktree HEAD: %w", err)
	}
	got := strings.TrimSpace(string(out))
	want := strings.TrimSpace(head)
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("worktree HEAD drifted to %s, want %s", got, want)
	}
	status := exec.CommandContext(ctx, gitPath, "-C", worktree, "status", "--porcelain")
	st, err := status.Output()
	if err != nil {
		return fmt.Errorf("worktree status: %w", err)
	}
	if strings.TrimSpace(string(st)) != "" {
		return fmt.Errorf("worktree is dirty")
	}
	return nil
}

func groupedFindingPrompt(group fileGroup, all []changedFile, base, head string) string {
	return strings.Join([]string{
		"You are a grouped reviewer subtask. Do not publish a review or call review submit.",
		"Do not checkout another revision, edit files, format, generate output, or otherwise mutate the worktree. Inspect the fixed head only.",
		"Fixed base_sha=" + base + " head_sha=" + head + ".",
		"Review only these paths: " + strings.Join(group.Paths, ", "),
		"Other changed files (context; still check cross-group contracts): " + strings.Join(otherPaths(group, all), ", "),
		"Return __LOOPER_RESULT__ JSON with summary, outcome, and findings using disposition, severity, scopeBasis, scopeEvidence, title, body, optional path/line.",
	}, "\n")
}

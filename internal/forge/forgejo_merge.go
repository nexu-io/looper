package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type RepositoryMergeSettings struct {
	AllowMergeCommit bool `json:"allow_merge_commits"`
	AllowSquashMerge bool `json:"allow_squash_merge"`
	AllowRebaseMerge bool `json:"allow_rebase"`
}

type BranchProtection struct {
	Protected           bool     `json:"protected"`
	EnableStatusCheck   bool     `json:"enable_status_check"`
	StatusCheckContexts []string `json:"status_check_contexts"`
}

func (forgejo *ForgejoClient) GetRepositoryMergeSettings(ctx context.Context) (RepositoryMergeSettings, error) {
	var settings RepositoryMergeSettings
	err := forgejo.do(ctx, http.MethodGet, forgejo.repoPath(), nil, nil, &settings)
	return settings, err
}

func (forgejo *ForgejoClient) GetBranchProtection(ctx context.Context, branch string) (BranchProtection, error) {
	var protection BranchProtection
	err := forgejo.do(ctx, http.MethodGet, forgejo.repoPath("branches", url.PathEscape(strings.TrimSpace(branch))), nil, nil, &protection)
	return protection, err
}

func (forgejo *ForgejoClient) EnableAutoMerge(ctx context.Context, number int64, strategy, headSHA string) error {
	if strings.TrimSpace(headSHA) == "" {
		return fmt.Errorf("Forgejo auto-merge requires the reviewed head commit")
	}
	switch strategy {
	case "merge", "squash", "rebase":
	default:
		return fmt.Errorf("unsupported Forgejo auto-merge strategy %q", strategy)
	}
	if err := forgejo.requireCapability(ctx, "autoMerge", http.MethodPost, "/repos/{owner}/{repo}/pulls/{index}/merge"); err != nil {
		return err
	}
	// Forgejo's scheduled merge ignores head_commit_id, including when the head
	// changes after scheduling. Use only immediate merge, where the server checks
	// this expected commit and required checks. The reviewer publish checkpoint
	// owns retries while CI is pending; no server-side merge is left queued.
	return forgejo.do(ctx, http.MethodPost, forgejo.repoPath("pulls", strconv.FormatInt(number, 10), "merge"), nil, map[string]any{
		"Do": strategy, "head_commit_id": strings.TrimSpace(headSHA), "merge_when_checks_succeed": false,
		"force_merge": false, "delete_branch_after_merge": false,
	}, nil)
}

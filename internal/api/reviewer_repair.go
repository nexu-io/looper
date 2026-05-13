package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

type reviewerRepairRequest struct {
	ProjectID string `json:"projectId,omitempty"`
	Repo      string `json:"repo"`
	PRNumber  int64  `json:"prNumber"`
	Apply     bool   `json:"apply"`
}

func (h *Handler) buildReviewerRepairRouteResponse(r *http.Request) (reviewer.RepairResult, error) {
	if r.Method != http.MethodPost {
		return reviewer.RepairResult{}, apiError{
			code:    pkgapi.ErrorCodeMethodNotAllowed,
			status:  http.StatusMethodNotAllowed,
			message: "Unsupported method for /api/v1/reviewer/repair",
		}
	}
	var request reviewerRepairRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return reviewer.RepairResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Invalid repair request: %v", err)}
	}
	input := reviewer.RepairInput{
		ProjectID: strings.TrimSpace(request.ProjectID),
		Repo:      strings.TrimSpace(request.Repo),
		PRNumber:  request.PRNumber,
		Apply:     request.Apply,
	}
	if input.Repo == "" || input.PRNumber <= 0 {
		return reviewer.RepairResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo and positive prNumber are required"}
	}
	if h.context.RepairReviewer != nil {
		result, err := h.context.RepairReviewer(r.Context(), input)
		if err != nil {
			return result, mapReviewerRepairError(err)
		}
		if result.Applied && h.context.TriggerSchedulerTick != nil {
			h.context.TriggerSchedulerTick()
		}
		return result, nil
	}
	result, err := h.defaultReviewerRepair(r.Context(), input)
	if err != nil {
		return result, mapReviewerRepairError(err)
	}
	if result.Applied && h.context.TriggerSchedulerTick != nil {
		h.context.TriggerSchedulerTick()
	}
	return result, nil
}

func (h *Handler) defaultReviewerRepair(ctx context.Context, input reviewer.RepairInput) (reviewer.RepairResult, error) {
	if h.context.Runtime == nil {
		return reviewer.RepairResult{}, fmt.Errorf("runtime is not configured")
	}
	services := h.context.Runtime.Services()
	if services.Coordinator == nil || services.Repositories == nil {
		return reviewer.RepairResult{}, fmt.Errorf("runtime storage is not configured")
	}
	ghPath := ""
	if h.context.Config.Tools.GHPath != nil {
		ghPath = strings.TrimSpace(*h.context.Config.Tools.GHPath)
	}
	gateway := github.New(github.Options{GHPath: ghPath, Now: h.now})
	repairer := reviewer.NewRepairer(reviewer.RepairOptions{
		DB:           services.Coordinator.DB(),
		Repos:        services.Repositories,
		GitHub:       reviewerRepairGitHubAdapter{gateway: gateway},
		Now:          h.now,
		ReviewEvents: h.context.Config.Roles.Reviewer.Behavior.ReviewEvents,
	})
	return repairer.Repair(ctx, input)
}

func mapReviewerRepairError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, reviewer.ErrRepairLoopNotFound):
		return apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: err.Error()}
	case errors.Is(err, reviewer.ErrRepairActiveWork):
		return apiError{code: pkgapi.ErrorCodeLoopConflict, status: http.StatusConflict, message: err.Error()}
	default:
		return err
	}
}

type reviewerRepairGitHubAdapter struct {
	gateway *github.Gateway
}

func (a reviewerRepairGitHubAdapter) ViewPullRequest(ctx context.Context, input reviewer.ViewPullRequestInput) (reviewer.PullRequestDetail, error) {
	detail, err := a.gateway.ViewPullRequest(ctx, github.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return reviewer.PullRequestDetail{}, err
	}
	return reviewer.PullRequestDetail{
		Number:         detail.Number,
		Title:          detail.Title,
		Body:           detail.Body,
		State:          detail.State,
		IsDraft:        detail.IsDraft,
		ReviewDecision: detail.ReviewDecision,
		Labels:         detail.Labels,
		HeadSHA:        detail.HeadSHA,
		BaseSHA:        detail.BaseSHA,
		HeadRefName:    detail.HeadRefName,
		BaseRefName:    detail.BaseRefName,
		Author:         detail.Author,
		ReviewRequests: detail.ReviewRequests,
		HasConflicts:   detail.HasConflicts,
		Comments:       detail.Comments,
		IssueComments:  reviewerRepairCommentInfosToObjects(detail.IssueComments),
		Reviews:        detail.Reviews,
	}, nil
}

func (a reviewerRepairGitHubAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func reviewerRepairCommentInfosToObjects(items []github.CommentInfo) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"id":                item.ID,
			"author":            map[string]any{"login": item.Author},
			"authorAssociation": item.AuthorAssociation,
			"body":              item.Body,
			"createdAt":         item.CreatedAt,
			"updatedAt":         item.UpdatedAt,
			"url":               item.URL,
		})
	}
	return out
}

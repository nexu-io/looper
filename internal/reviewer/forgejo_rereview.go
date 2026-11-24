package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

func (r *Runner) supportsForgejoRereview(projectID string, loop storage.LoopRecord) bool {
	return r.forgejoProject(projectID) && !isManualReviewerLoop(loop) &&
		!r.commentOnlyCompletionForProject(projectID, r.effectiveReviewEvents(projectID, loop.MetadataJSON)) &&
		!r.reviewerAutoMergeConfigForProject(projectID).Enabled
}

// Forgejo replaces pending requests with submitted reviews. An outstanding
// request with a newer review ID than the reviewer's last submission is the
// authority for one further pass of the live head.
func forgejoPendingRereview(requested []string, reviews []map[string]any, login, head string) int64 {
	if head == "" || !isCurrentUserRequested(requested, login) {
		return 0
	}
	var requestID, latestID int64
	var latestHead string
	for _, review := range reviews {
		author, _ := review["author"].(map[string]any)
		authorLogin, _ := stringFromAny(author["login"])
		if normalizeLogin(authorLogin) != normalizeLogin(login) {
			continue
		}
		id := readForgejoReviewID(review["id"])
		state, _ := stringFromAny(review["state"])
		if state == "REQUEST_REVIEW" && id > requestID {
			requestID = id
		}
		// Dismissal does not turn a historical submission into a request.
		if (isSubmittedReviewState(state) || state == "DISMISSED") && id > latestID {
			latestID = id
			commit, _ := review["commit"].(map[string]any)
			latestHead, _ = stringFromAny(commit["oid"])
		}
	}
	if latestHead != head || requestID <= latestID {
		return 0
	}
	return requestID
}

func readForgejoReviewID(value any) int64 {
	switch id := value.(type) {
	case int64:
		return id
	case float64:
		n := int64(id)
		if float64(n) == id {
			return n
		}
	case json.Number:
		n, _ := id.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(id, 10, 64)
		return n
	}
	return 0
}

func forgejoRereviewPublicationID(loopID, head string, requestID int64) string {
	return fmt.Sprintf("%s:rereview:%d", agentNativeReviewID(loopID, head), requestID)
}

func forgejoRereviewIDFromQueue(payload *string) int64 {
	return readForgejoReviewID(parseJSONObject(payload)["forgejoRereviewId"])
}

func skipInvalidForgejoRereview(checkpoint reviewerCheckpoint) reviewerCheckpoint {
	checkpoint.SkipKind = "forgejo_rereview_no_longer_current"
	checkpoint.SkipReason = "Forgejo re-review request is no longer current for this head"
	checkpoint.PendingReview = nil
	return checkpoint
}

func (r *Runner) recoverOrValidateForgejoRereview(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint) (reviewerCheckpoint, bool, error) {
	view := ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath}
	detail, err := r.github.ViewPullRequest(ctx, view)
	if err != nil {
		next, skipped, skipErr := r.skipMissingPullRequest(ctx, input, checkpoint, err)
		if skipped || skipErr != nil {
			return next, skipped, skipErr
		}
		return checkpoint, false, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	head := checkpoint.Snapshot.HeadSHA
	if detail.HeadSHA != head || headSHAFromQueuePayload(input.QueueItem.PayloadJSON) != head ||
		normalizePRState(detail.State) != "open" || !r.supportsForgejoRereview(input.Project.ID, input.Loop) {
		return skipInvalidForgejoRereview(checkpoint), true, nil
	}
	if domain.IsAutoLaneHeld(domain.LoopTypeReviewer, detail.Labels) {
		return checkpoint, false, &holdSkipError{summary: fmt.Sprintf("Reviewer stopped because %s#%d is currently held", input.Repo, input.PRNumber)}
	}
	id := reviewPublicationID(input.Loop.ID, checkpoint)
	// Check the marker first. Forgejo consumes the request when it accepts the
	// review, even if the submitting child lost the HTTP response.
	found, err := r.verifyAgentNativeReviewMarker(ctx, input, head, id, cleanReviewAuthorLogin(checkpoint, detail))
	if err != nil {
		return checkpoint, false, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if found.Found {
		checkpoint.PendingReview = &pendingReviewCheckpoint{
			HeadSHA: head, IdempotencyKey: id, Event: reviewEventAgentNative,
			Summary: "Recovered published Forgejo re-review", Outcome: normalizeCommentOnlyOutcome(found.Outcome),
			ContentFingerprint: reviewMarkerFingerprint(found),
		}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, true, nil
	}
	login, err := r.requireCurrentUserLogin(ctx, input.Project.RepoPath)
	if err != nil {
		return checkpoint, false, err
	}
	if forgejoPendingRereview(detail.ReviewRequests, detail.Reviews, login, head) != checkpoint.ForgejoRereviewID {
		return skipInvalidForgejoRereview(checkpoint), true, nil
	}
	return checkpoint, false, nil
}

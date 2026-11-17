package fixer

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
)

// This is the agent's fixed decision, not Forgejo's remote resolution state.
// Reuse the existing per-comment evidence store so later runs retain that
// decision without introducing another ledger or treating a push as authority.
const forgejoFixedUnresolvedStatus = "fixed_unresolved"

var forgejoReviewerMarker = regexp.MustCompile(`<!--\s*looper:review\s+([^>]*)-->`)

func eligibleNativeReviewComment(comment NativeReviewComment, currentUser string) bool {
	state := strings.ToUpper(strings.TrimSpace(comment.ReviewState))
	if state == "PENDING" || state == "DISMISSED" {
		return false
	}
	if isLooperFixerReplyComment(ReviewThreadComment{Body: comment.Body}) || isLooperFixerReplyComment(ReviewThreadComment{Body: comment.ReviewBody}) {
		return false
	}
	if !sameGitHubLogin(comment.Author, currentUser) {
		return true
	}
	// Inline comments normally carry only a generic hidden stamp. The submitted
	// parent review's role marker distinguishes our reviewer from our own chatter.
	if !sameGitHubLogin(comment.ReviewAuthor, currentUser) || (state != "COMMENTED" && state != "CHANGES_REQUESTED" && state != "APPROVED") {
		return false
	}
	for _, match := range forgejoReviewerMarker.FindAllStringSubmatch(comment.ReviewBody, -1) {
		fields := map[string]string{}
		for _, raw := range strings.Fields(match[1]) {
			if key, value, ok := strings.Cut(raw, "="); ok {
				fields[key] = value
			}
		}
		if !strings.HasPrefix(fields["id"], "reviewer:") || strings.TrimPrefix(fields["id"], "reviewer:") == "" || fields["head"] == "" || fields["head"] != comment.ReviewCommitID {
			continue
		}
		switch fields["outcome"] {
		case "blocking", "actionable", "non_blocking":
			return true
		}
	}
	return false
}

func (r *Runner) freshNativeFixMetadata(ctx context.Context, loop *storage.LoopRecord) (*string, error) {
	if loop == nil {
		return nil, nil
	}
	if r.repos != nil && r.repos.Loops != nil && loop.ID != "" {
		current, err := r.repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return nil, err
		}
		if current != nil {
			return current.MetadataJSON, nil
		}
	}
	return loop.MetadataJSON, nil
}

func (r *Runner) suppressFixedForgejoNativeItems(ctx context.Context, project storage.ProjectRecord, repo string, prNumber int64, head string, loop *storage.LoopRecord, items []FixItem) ([]FixItem, error) {
	if !hasForgejoNativeReviewComments(items) {
		return items, nil
	}
	metadata, err := r.freshNativeFixMetadata(ctx, loop)
	if err != nil {
		return nil, err
	}
	store := loadFixEvidenceStoreV2(metadata)
	retained := make([]FixItem, 0, len(items))
	reachable := map[string]bool{}
	for _, item := range items {
		entry, found := findThreadFixEvidence(store, item)
		if item.Source != NativeReviewCommentSource || !found || entry.ResolveState != forgejoFixedUnresolvedStatus || entry.Source != "agent_repair_result" || entry.Explanation == "" || entry.EvidenceHeadSHA == "" || head == "" {
			retained = append(retained, item)
			continue
		}
		ok, checked := reachable[entry.EvidenceHeadSHA]
		if !checked {
			ok = strings.EqualFold(entry.EvidenceHeadSHA, head)
			if !ok {
				comparison, compareErr := r.github.CompareCommits(ctx, CompareCommitsInput{Repo: repo, Base: entry.EvidenceHeadSHA, Head: head, CWD: project.RepoPath})
				if compareErr != nil {
					return nil, compareErr
				}
				ok = comparison.Status == "ahead" || comparison.Status == "identical"
			}
			reachable[entry.EvidenceHeadSHA] = ok
		}
		if !ok {
			retained = append(retained, item)
		}
	}
	return retained, nil
}

func (r *Runner) recordFixedForgejoNativeItem(ctx context.Context, input stepInput, checkpoint fixerCheckpoint, item FixItem, decision replyExplanationEntry) error {
	head := firstNonEmpty(checkpoint.Validation.HeadSHA, resolveCommentsExpectedHeadSHA(checkpoint))
	if head == "" {
		return fmt.Errorf("cannot retain a native repair decision without the inspected commit")
	}
	entry := threadFixEvidence{
		ThreadID: item.ThreadID, ThreadFingerprint: decision.ObservedFingerprint,
		EvidenceHeadSHA: head, ValidationHeadSHA: checkpoint.Validation.HeadSHA,
		CommitSHA:          resolveCommentCommitSHA(checkpoint, nil, false),
		ProducedNewCommits: roundProducedNewCommits(&checkpoint),
		FixItemsHash:       checkpoint.FixItemsHash, Source: "agent_repair_result", RunID: input.Run.ID,
		Explanation: decision.Explanation, ResolveState: forgejoFixedUnresolvedStatus,
	}
	return r.persistFixEvidenceStoreV2(ctx, input.Loop, upsertThreadFixEvidence(nil, entry))
}

// finishForgejoNativeComments keeps the agent's original decision fingerprint
// across refresh/replay. Infrastructure only detects drift; it never invents a
// fixed decision from commits or the presence of push evidence.
func (r *Runner) finishForgejoNativeComments(ctx context.Context, input stepInput, checkpoint *fixerCheckpoint, items []FixItem, liveComments []NativeReviewComment) (finished, missing, drift int, mutationErr error) {
	liveByID := map[int64]NativeReviewComment{}
	for _, live := range liveComments {
		liveByID[live.ProviderCommentID] = live
	}
	decisions := agentResolveRepliesByFixItemID(*checkpoint)
	var resolutionState forge.ProbeState
	var probeErr error
	probed := false
	for _, item := range items {
		decision, hasDecision := decisions[item.ID]
		live, exists := liveByID[item.ProviderCommentID]
		result := checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Action: decision.Action, UpdatedAt: r.nowISO()}
		switch {
		case !exists:
			result.Status = "deleted"
		case live.IsResolved:
			result.Status = "already_resolved"
		case !hasDecision || normalizeNativeRepairAction(decision.Action) == "":
			missing++
			result.Status, result.Message = "skipped_missing_agent_decision", agentMissingThreadDecisionExplanation
		case decision.ObservedFingerprint == "" || decision.ObservedFingerprint != live.ObservedFingerprint:
			drift++
			result.Status, result.Message = "skipped_thread_drift", "Forgejo review comment changed since the fixer inspected it"
		case decision.Action != "fixed":
			result.Status, result.Message = "skipped_noop", decision.Explanation
		default:
			// Missing resolver data prevents a remote mutation; it does not
			// prevent repairing code or acknowledging the agent's fixed result.
			if live.ResolverPresent && !probed {
				resolutionState, probeErr = r.github.ProbeNativeReviewCommentResolution(ctx, ListNativeReviewCommentsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
				probed = true
			}
			unsupported := !live.ResolverPresent || resolutionState == forge.ProbeStateUnsupported
			if !unsupported && (probeErr != nil || resolutionState != forge.ProbeStateSupported) {
				result.Status, result.Message = "failed_mutation_retry", "Could not determine whether Forgejo can close this comment"
				if mutationErr == nil {
					mutationErr = fmt.Errorf("native comment resolution capability unavailable: %s: %v", resolutionState, probeErr)
				}
			} else {
				if !unsupported {
					err := r.github.ResolveNativeReviewComment(ctx, ResolveNativeReviewCommentInput{Repo: input.Repo, PRNumber: input.PRNumber, ProviderCommentID: item.ProviderCommentID, CWD: input.Project.RepoPath})
					if isForgejoNativeResolveUnsupported(err) {
						unsupported = true
					} else if err != nil {
						result.Status, result.Message = "failed_mutation_retry", err.Error()
						if mutationErr == nil {
							mutationErr = err
						}
					}
				}
				if result.Status == "" {
					if unsupported {
						result.Status, result.Message = forgejoFixedUnresolvedStatus, decision.Explanation
					} else {
						result.Status = "resolved"
					}
					finished++
				}
			}
		}
		upsertResolvedComment(&checkpoint.ResolvedComments.Items, result)
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepResolveComments, *checkpoint); err != nil {
			return finished, missing, drift, err
		}
	}
	return finished, missing, drift, mutationErr
}

// Publish before suppressing future discovery. A failed acknowledgement remains
// replayable from the run checkpoint without treating push evidence as a fix.
func (r *Runner) recordPublishedForgejoNativeResults(ctx context.Context, input stepInput, checkpoint fixerCheckpoint) error {
	if checkpoint.ResolvedComments == nil {
		return nil
	}
	decisions := agentResolveRepliesByFixItemID(checkpoint)
	for _, item := range checkpoint.FixItems {
		if item.Source != NativeReviewCommentSource {
			continue
		}
		for _, result := range checkpoint.ResolvedComments.Items {
			if result.FixItemID == item.ID && result.Status == forgejoFixedUnresolvedStatus {
				if err := r.recordFixedForgejoNativeItem(ctx, input, checkpoint, item, decisions[item.ID]); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

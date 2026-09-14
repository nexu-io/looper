package webhookforward

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Forgejo sends exact event types separately from its GitHub-compatible coarse
// event names. These are wakeups: role discovery re-reads the forge and applies
// existing labels, assignees, holds, budgets, and dedupe before enqueuing a run.
func routeForgejoDelivery(eventType string, payload []byte) (routedDelivery, bool, error) {
	var envelope struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest struct {
			Number int64 `json:"number"`
		} `json:"pull_request"`
		Issue struct {
			Number      int64            `json:"number"`
			PullRequest *json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		IsPull  bool   `json:"is_pull"`
		Ref     string `json:"ref"`
		After   string `json:"after"`
		Deleted bool   `json:"deleted"`
		Run     struct {
			EventPayload string `json:"event_payload"`
			Repository   struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"run"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return routedDelivery{}, false, fmt.Errorf("decode Forgejo webhook: %w", err)
	}
	routed := routedDelivery{repo: strings.TrimSpace(envelope.Repository.FullName), action: envelope.Action}
	prLanes := map[Lane]struct{}{LaneReviewer: {}, LaneFixer: {}}
	issueLanes := map[Lane]struct{}{LanePlanner: {}, LaneWorker: {}}
	switch strings.TrimSpace(eventType) {
	case "issues", "issue_assign", "issue_label", "issue_milestone", "issue_comment":
		if envelope.PullRequest.Number > 0 || envelope.IsPull || envelope.Issue.PullRequest != nil {
			number := envelope.PullRequest.Number
			if number == 0 {
				number = envelope.Issue.Number
			}
			routed.objectType, routed.numbers, routed.lanes = "pull_request", []int64{number}, prLanes
		} else {
			if envelope.Issue.Number <= 0 {
				return routedDelivery{}, false, fmt.Errorf("Forgejo issue webhook missing number")
			}
			routed.objectType, routed.lanes, routed.numbers = "issues", issueLanes, []int64{envelope.Issue.Number}
		}
	case "pull_request", "pull_request_assign", "pull_request_label", "pull_request_milestone", "pull_request_sync", "pull_request_review_request", "pull_request_comment", "pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment", "pull_request_approved", "pull_request_rejected":
		number := envelope.PullRequest.Number
		if number == 0 {
			number = envelope.Issue.Number
		}
		if number <= 0 {
			return routedDelivery{}, false, fmt.Errorf("Forgejo PR webhook missing number")
		}
		routed.objectType, routed.numbers, routed.lanes = "pull_request", []int64{number}, prLanes
	case "push":
		if envelope.Deleted || (envelope.After != "" && strings.Trim(envelope.After, "0") == "") {
			return routedDelivery{}, false, nil
		}
		branch, ok := strings.CutPrefix(envelope.Ref, "refs/heads/")
		if !ok || branch == "" {
			return routedDelivery{}, false, nil
		}
		routed.objectType, routed.branch, routed.lanes = "base_branch", branch, map[Lane]struct{}{LaneFixer: {}}
	case "action_run_success", "action_run_failure", "action_run_recover":
		// Forgejo wraps the original trigger as a JSON string. PR workflows
		// can target that PR directly, independently of polling list limits.
		routed.repo = strings.TrimSpace(envelope.Run.Repository.FullName)
		routed.objectType, routed.lanes = "repository", prLanes
		var trigger struct {
			PullRequest struct {
				Number int64 `json:"number"`
			} `json:"pull_request"`
		}
		if json.Unmarshal([]byte(envelope.Run.EventPayload), &trigger) == nil && trigger.PullRequest.Number > 0 {
			routed.objectType, routed.numbers = "pull_request", []int64{trigger.PullRequest.Number}
		}
	default:
		return routedDelivery{}, false, nil
	}
	if routed.repo == "" {
		return routedDelivery{}, false, fmt.Errorf("Forgejo webhook missing repository")
	}
	return routed, true, nil
}

package sweeper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

const phase1BodyCapBytes = 8 * 1024

type FactBundle struct {
	Repo                       string              `json:"repo"`
	TargetType                 string              `json:"target_type"`
	Number                     int64               `json:"number"`
	State                      string              `json:"state"`
	IsDraft                    bool                `json:"is_draft,omitempty"`
	HeadSHA                    string              `json:"head_sha,omitempty"`
	CreatedAt                  string              `json:"created_at,omitempty"`
	UpdatedAt                  string              `json:"updated_at,omitempty"`
	ClosedAt                   string              `json:"closed_at,omitempty"`
	Title                      string              `json:"title,omitempty"`
	Body                       string              `json:"body,omitempty"`
	BodyTruncated              bool                `json:"body_truncated,omitempty"`
	Author                     string              `json:"author,omitempty"`
	AuthorAssociation          string              `json:"author_association,omitempty"`
	Labels                     []string            `json:"labels,omitempty"`
	PolicyLabelsPresent        []string            `json:"policy_labels_present,omitempty"`
	CommentCount               int                 `json:"comment_count,omitempty"`
	Case                       FactBundleCase      `json:"case"`
	PolicySnapshot             any                 `json:"policy_snapshot,omitempty"`
	LastHumanCommentAt         string              `json:"last_human_comment_at,omitempty"`
	HumanCommentCountSinceOpen int                 `json:"human_comment_count_since_open,omitempty"`
	RecentHumanComments        []FactComment       `json:"recent_human_comments,omitempty"`
	WarningComment             *FactWarningComment `json:"warning_comment,omitempty"`
	Timeline                   FactTimeline        `json:"timeline,omitempty"`
	LinkedPRs                  []FactLinkedPR      `json:"linked_prs,omitempty"`
	PRReviewState              *FactPRReviewState  `json:"pr_review_state,omitempty"`
}

type FactComment struct {
	Author       string `json:"author,omitempty"`
	Association  string `json:"association,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	Body         string `json:"body,omitempty"`
	IsMaintainer bool   `json:"is_maintainer,omitempty"`
}

type FactWarningComment struct {
	ID               int64          `json:"id"`
	Body             string         `json:"body,omitempty"`
	CreatedAt        string         `json:"created_at,omitempty"`
	Edited           bool           `json:"edited,omitempty"`
	ReactionsSummary map[string]int `json:"reactions_summary,omitempty"`
}

type FactTimeline struct {
	CrossReferences []map[string]any `json:"cross_references,omitempty"`
	Closures        []map[string]any `json:"closures,omitempty"`
	Duplicates      []map[string]any `json:"duplicates,omitempty"`
}

type FactLinkedPR struct {
	Number         int64  `json:"number"`
	State          string `json:"state,omitempty"`
	Merged         bool   `json:"merged,omitempty"`
	MergedAt       string `json:"merged_at,omitempty"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
}

type FactPRReviewState struct {
	RequestedReviewers  []string          `json:"requested_reviewers,omitempty"`
	LatestReviewPerUser map[string]string `json:"latest_review_per_user,omitempty"`
	LastReviewAt        string            `json:"last_review_at,omitempty"`
}

type FactBundleCase struct {
	CurrentPhase        string `json:"current_phase,omitempty"`
	WarnedAt            string `json:"warned_at,omitempty"`
	CloseDueAt          string `json:"close_due_at,omitempty"`
	WarningMarkerUUID   string `json:"warning_marker_uuid,omitempty"`
	LastHumanActivityAt string `json:"last_human_activity_at,omitempty"`
}

type FingerprintInputs struct {
	State                      string   `json:"state"`
	UpdatedAt                  string   `json:"updated_at"`
	HeadSHA                    string   `json:"head_sha"`
	IsDraft                    bool     `json:"is_draft"`
	PolicyLabelsPresent        []string `json:"policy_labels_present"`
	LastHumanCommentAt         string   `json:"last_human_comment_at"`
	HumanCommentCountSinceOpen int      `json:"human_comment_count_since_open"`
}

type FingerprintRecord struct {
	Algorithm string            `json:"algorithm"`
	Hash      string            `json:"hash"`
	Inputs    FingerprintInputs `json:"inputs"`
}

func NewMarkerUUID() string {
	return eventlog.NewEventID("sweeper")
}

func BuildFingerprint(bundle FactBundle) (string, error) {
	inputs := FingerprintInputs{
		State:                      strings.TrimSpace(bundle.State),
		UpdatedAt:                  strings.TrimSpace(bundle.UpdatedAt),
		HeadSHA:                    strings.TrimSpace(bundle.HeadSHA),
		IsDraft:                    bundle.IsDraft,
		PolicyLabelsPresent:        append([]string(nil), bundle.PolicyLabelsPresent...),
		LastHumanCommentAt:         strings.TrimSpace(bundle.LastHumanCommentAt),
		HumanCommentCountSinceOpen: bundle.HumanCommentCountSinceOpen,
	}
	sort.Strings(inputs.PolicyLabelsPresent)
	inputJSON, err := json.Marshal(inputs)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(inputJSON)
	record := FingerprintRecord{
		Algorithm: "sha256",
		Hash:      hex.EncodeToString(hash[:]),
		Inputs:    inputs,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func TruncateFactBody(body string) (string, bool) {
	body = strings.TrimSpace(body)
	if len(body) <= phase1BodyCapBytes {
		return body, false
	}
	trimmed := strings.TrimSpace(body[:phase1BodyCapBytes])
	if trimmed == "" {
		return trimmed, true
	}
	return trimmed + "\n... [truncated]", true
}

func PolicyLabelsPresent(labels []string, roleCfg config.SweeperRoleConfig) []string {
	candidates := []string{
		roleCfg.Lifecycle.PendingLabel,
		roleCfg.Lifecycle.ClosedLabel,
		roleCfg.Lifecycle.KeepLabel,
		roleCfg.Security.QuarantineLabel,
	}
	candidates = append(candidates, roleCfg.Triggers.ExcludeLabels...)
	candidates = append(candidates, roleCfg.Triggers.LooperInternalLabels...)
	seen := map[string]string{}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		seen[strings.ToLower(candidate)] = candidate
	}
	out := make([]string, 0, len(seen))
	for _, label := range labels {
		if normalized, ok := seen[strings.ToLower(strings.TrimSpace(label))]; ok {
			out = append(out, normalized)
		}
	}
	sort.Strings(out)
	return out
}

func DeriveHumanCommentStats(issueComments []githubinfra.CommentInfo, reviewThreads []githubinfra.ReviewThread, excludedAuthors []string, looperLogin string) (string, int) {
	excluded := map[string]struct{}{}
	for _, author := range excludedAuthors {
		author = strings.ToLower(strings.TrimSpace(author))
		if author != "" {
			excluded[author] = struct{}{}
		}
	}
	if login := strings.ToLower(strings.TrimSpace(looperLogin)); login != "" {
		excluded[login] = struct{}{}
	}
	latest := ""
	count := 0
	consider := func(author, association, createdAt string) {
		author = strings.ToLower(strings.TrimSpace(author))
		association = strings.ToUpper(strings.TrimSpace(association))
		createdAt = strings.TrimSpace(createdAt)
		if author == "" || createdAt == "" {
			return
		}
		if _, blocked := excluded[author]; blocked || association == "BOT" {
			return
		}
		count++
		if latest == "" || createdAt > latest {
			latest = createdAt
		}
	}
	for _, comment := range issueComments {
		consider(comment.Author, comment.AuthorAssociation, comment.CreatedAt)
	}
	for _, thread := range reviewThreads {
		for _, comment := range thread.Comments {
			consider(comment.Author, comment.AuthorAssociation, comment.CreatedAt)
		}
	}
	return latest, count
}

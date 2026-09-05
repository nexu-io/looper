package reviewer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	// metadataLastReviewedSignalFingerprintKey is stored on Reviewer loop metadata.
	metadataLastReviewedSignalFingerprintKey = "lastReviewedSignalFingerprint"
	// metadataLastResolvedReviewThreadIDsKey stores IDs of threads that were
	// resolved in the last persisted review-signal snapshot.
	metadataLastResolvedReviewThreadIDsKey = "lastResolvedReviewThreadIDs"

	threadResolutionMarkerNeedle = "looper:thread-resolution"
	fixerDeclinedMarkerNeedle    = "looper-fixer-reply-declined"
)

var (
	// Exact HTML comment: <!-- looper:thread-resolution thread=... head=... [feedback=...] decision=... -->
	threadResolutionMarkerRE = regexp.MustCompile(`(?is)<!--\s*looper:thread-resolution\s+([^>]*?)-->`)
	// Fixer decline marker. Optional trailing fields (e.g. attempt:post-reject) are ignored.
	fixerDeclinedMarkerRE = regexp.MustCompile(`(?is)<!--\s*looper-fixer-reply-declined\s+thread:(\S+)\s+fingerprint:(\S+)(?:\s+[^>]*)?-->`)
	// Fixer fixed-reply marker. Distinct from decline (`looper-fixer-reply-declined`).
	fixerFixedMarkerRE = regexp.MustCompile(`(?is)<!--\s*looper-fixer-reply\s+thread:(\S+)\s+commit:(\S+)(?:\s+[^>]*)?-->`)
)

// threadResolutionMarkerFields holds parsed audit marker fields.
type threadResolutionMarkerFields struct {
	ThreadID    string
	HeadSHA     string
	Feedback    string
	Decision    string
	HasFeedback bool
}

// ThreadFeedbackFingerprint returns sha256 hex of the canonical Looper-authored
// review-thread feedback set. Reviewer audit replies carrying a validated
// looper:thread-resolution marker from Looper's identity are excluded so
// accept/reject replies do not re-trigger themselves. Spoofed markers from
// untrusted authors remain in the fingerprint. Resolved threads contribute
// only id and resolved state; later comments on those threads are omitted so
// irrelevant edits cannot retrigger same-head discovery or a duplicate
// convergence review. Reopen re-includes comments.
func ThreadFeedbackFingerprint(threads []ReviewThread) string {
	return ThreadFeedbackFingerprintForLogin(threads, "")
}

// ThreadFeedbackFingerprintForLogin is the identity-aware fingerprint. When
// looperLogin is non-empty, only threads/comments authored by that identity
// count as Looper-authored / audit exclusions.
func ThreadFeedbackFingerprintForLogin(threads []ReviewThread, looperLogin string) string {
	canonical := canonicalThreadFeedbackInput(threads, looperLogin)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// ReviewSignalFingerprint returns sha256 hex of headSha + thread feedback.
func ReviewSignalFingerprint(headSHA, threadFeedbackFingerprint string) string {
	payload := strings.TrimSpace(headSHA) + "\x1f" + strings.TrimSpace(threadFeedbackFingerprint)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// ComputeReviewSignalFingerprint is a convenience over the two-step hash.
func ComputeReviewSignalFingerprint(headSHA string, threads []ReviewThread) string {
	return ComputeReviewSignalFingerprintForLogin(headSHA, threads, "")
}

// ComputeReviewSignalFingerprintForLogin includes Looper identity in authorship.
func ComputeReviewSignalFingerprintForLogin(headSHA string, threads []ReviewThread, looperLogin string) string {
	return ReviewSignalFingerprint(headSHA, ThreadFeedbackFingerprintForLogin(threads, looperLogin))
}

func canonicalThreadFeedbackInput(threads []ReviewThread, looperLogin string) string {
	looperThreads := make([]ReviewThread, 0, len(threads))
	for _, thread := range threads {
		if !isLooperAuthoredThreadForLogin(thread, looperLogin) &&
			(thread.IsResolved || !threadHasValidatedFixerDeclineFromAuthor(thread, looperLogin)) {
			continue
		}
		looperThreads = append(looperThreads, thread)
	}
	sort.SliceStable(looperThreads, func(i, j int) bool {
		return looperThreads[i].ID < looperThreads[j].ID
	})
	var b strings.Builder
	for _, thread := range looperThreads {
		appendCanonicalThreadFeedback(&b, thread, looperLogin)
	}
	return b.String()
}

// candidateThreadFeedbackFingerprintForLogin fingerprints one already-selected
// disposition candidate. Unlike ThreadFeedbackFingerprintForLogin it always
// includes the thread, so a Reviewer audit after a third-party Fixer decline
// does not look like external feedback drift (HasUnauditedValidatedFixerDecline
// is false post-accept).
func candidateThreadFeedbackFingerprintForLogin(thread ReviewThread, looperLogin string) string {
	var b strings.Builder
	appendCanonicalThreadFeedback(&b, thread, looperLogin)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func appendCanonicalThreadFeedback(b *strings.Builder, thread ReviewThread, looperLogin string) {
	b.WriteString("thread\x1e")
	b.WriteString(strings.TrimSpace(thread.ID))
	b.WriteByte('\x1f')
	if thread.IsResolved {
		b.WriteString("resolved")
	} else {
		b.WriteString("unresolved")
	}
	b.WriteByte('\n')
	if thread.IsResolved {
		return
	}
	comments := make([]ReviewThreadComment, 0, len(thread.Comments))
	for _, comment := range thread.Comments {
		if isValidatedThreadResolutionAudit(comment, looperLogin, thread.ID) {
			continue
		}
		comments = append(comments, comment)
	}
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].ID != comments[j].ID {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedAt < comments[j].CreatedAt
	})
	for _, comment := range comments {
		b.WriteString("comment\x1e")
		b.WriteString(strings.TrimSpace(comment.ID))
		b.WriteByte('\x1f')
		b.WriteString(normalizeLogin(comment.Author))
		b.WriteByte('\x1f')
		b.WriteString(strings.ToUpper(strings.TrimSpace(comment.AuthorAssociation)))
		b.WriteByte('\x1f')
		b.WriteString(strings.TrimSpace(comment.CreatedAt))
		b.WriteByte('\x1f')
		b.WriteString(strings.TrimSpace(comment.UpdatedAt))
		b.WriteByte('\x1f')
		b.WriteString(normalizedBodyHash(comment.Body))
		b.WriteByte('\x1f')
		b.WriteString(strings.TrimSpace(comment.OriginalCommitOID))
		b.WriteByte('\x1f')
		b.WriteString(strings.TrimSpace(comment.CommitOID))
		b.WriteByte('\n')
	}
}

func normalizedBodyHash(body string) string {
	sum := sha256.Sum256([]byte(normalizeCommentBody(body)))
	return hex.EncodeToString(sum[:])
}

// normalizeCommentBody collapses runs of whitespace and trims edges so trivial
// formatting edits do not thrash the fingerprint while real content changes do.
func normalizeCommentBody(body string) string {
	var b strings.Builder
	b.Grow(len(body))
	prevSpace := false
	for _, r := range body {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// parseThreadResolutionMarker extracts fields from the exact HTML comment form.
// Substring spoofs that are not a well-formed marker return ok=false.
func parseThreadResolutionMarker(body string) (threadResolutionMarkerFields, bool) {
	m := threadResolutionMarkerRE.FindStringSubmatch(body)
	if len(m) < 2 {
		return threadResolutionMarkerFields{}, false
	}
	fields := threadResolutionMarkerFields{}
	for _, part := range strings.Fields(strings.TrimSpace(m[1])) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "thread":
			fields.ThreadID = strings.TrimSpace(value)
		case "head":
			fields.HeadSHA = strings.TrimSpace(value)
		case "feedback":
			fields.Feedback = strings.TrimSpace(value)
			fields.HasFeedback = true
		case "decision":
			fields.Decision = strings.ToLower(strings.TrimSpace(value))
		}
	}
	if fields.ThreadID == "" || fields.HeadSHA == "" || fields.Decision == "" {
		return threadResolutionMarkerFields{}, false
	}
	return fields, true
}

// isThreadResolutionAuditComment reports a well-formed audit marker in the body.
// Callers that authorize actions must also check Looper identity and containing
// thread ID via isValidatedThreadResolutionAudit.
func isThreadResolutionAuditComment(body string) bool {
	_, ok := parseThreadResolutionMarker(body)
	return ok
}

// isValidatedThreadResolutionAudit requires a well-formed marker and Looper
// authorship. When containingThreadID is non-empty, the marker's thread field
// must match it exactly. When looperLogin is empty, only the marker shape
// (and thread match) is checked (unit-test / baseline paths).
func isValidatedThreadResolutionAudit(comment ReviewThreadComment, looperLogin, containingThreadID string) bool {
	fields, ok := parseThreadResolutionMarker(comment.Body)
	if !ok {
		return false
	}
	if want := strings.TrimSpace(containingThreadID); want != "" && fields.ThreadID != want {
		return false
	}
	return isLooperIdentityAuthor(comment.Author, looperLogin)
}

func isLooperIdentityAuthor(author, looperLogin string) bool {
	author = normalizeLogin(author)
	if author == "" {
		return false
	}
	if looperLogin = normalizeLogin(looperLogin); looperLogin != "" {
		return author == looperLogin
	}
	// Without a known login, accept known Looper bot logins used in tests/fixtures.
	return author == "looper-bot" || author == "looper[bot]" || strings.HasPrefix(author, "looper")
}

func hasUnresolvedLooperAuthoredThreads(threads []ReviewThread) bool {
	return hasUnresolvedLooperAuthoredThreadsForLogin(threads, "")
}

func hasUnresolvedLooperAuthoredThreadsForLogin(threads []ReviewThread, looperLogin string) bool {
	for _, thread := range threads {
		if thread.IsResolved || thread.ID == "" || len(thread.Comments) == 0 {
			continue
		}
		if isLooperAuthoredThreadForLogin(thread, looperLogin) {
			return true
		}
	}
	return false
}

// threadResolutionMarker builds the extended audit HTML comment.
// feedback may be empty for legacy callers; new adjudication always sets it.
func threadResolutionMarker(threadID, headSHA, feedbackFingerprint, decision string) string {
	threadID = strings.TrimSpace(threadID)
	headSHA = strings.TrimSpace(headSHA)
	feedbackFingerprint = strings.TrimSpace(feedbackFingerprint)
	decision = strings.ToLower(strings.TrimSpace(decision))
	if feedbackFingerprint == "" {
		return fmt.Sprintf("<!-- looper:thread-resolution thread=%s head=%s decision=%s -->", threadID, headSHA, decision)
	}
	return fmt.Sprintf("<!-- looper:thread-resolution thread=%s head=%s feedback=%s decision=%s -->", threadID, headSHA, feedbackFingerprint, decision)
}

// hasThreadResolutionAuditForSignal reports whether the thread already carries a
// validated Looper audit for the same head + feedback fingerprint + decision.
// Markers without a feedback field are historical only and never suppress a new
// comment state. Spoofed markers from non-Looper authors never match.
func hasThreadResolutionAuditForSignal(thread ReviewThread, threadID, headSHA, feedbackFingerprint, decision string) bool {
	return hasThreadResolutionAuditForSignalForLogin(thread, threadID, headSHA, feedbackFingerprint, decision, "")
}

func hasThreadResolutionAuditForSignalForLogin(thread ReviewThread, threadID, headSHA, feedbackFingerprint, decision, looperLogin string) bool {
	threadID = strings.TrimSpace(threadID)
	headSHA = strings.TrimSpace(headSHA)
	feedbackFingerprint = strings.TrimSpace(feedbackFingerprint)
	decision = strings.ToLower(strings.TrimSpace(decision))
	if threadID == "" || headSHA == "" || feedbackFingerprint == "" || decision == "" {
		return false
	}
	for _, comment := range thread.Comments {
		if !isValidatedThreadResolutionAudit(comment, looperLogin, thread.ID) {
			continue
		}
		fields, ok := parseThreadResolutionMarker(comment.Body)
		if !ok || !fields.HasFeedback {
			continue
		}
		if fields.ThreadID == threadID && fields.HeadSHA == headSHA && fields.Feedback == feedbackFingerprint && fields.Decision == decision {
			return true
		}
	}
	return false
}

// resumeDispositionDecisionFromRemoteAudit returns the already-published
// disposition when a validated Looper audit for this thread, head, and current
// feedback already exists. Spec §8.2: retry observes the remote audit marker
// and must not reclassify after Reviewer's own mutation. A later validated
// Fixer decline after reject_wontfix is new input (§8.4) and must not resume.
// latestValidatedAuditDecision is authority: if the newest complete audit is
// reject_wontfix, a historical accept_wontfix is never resumed (accept→resolve→
// reopen→reject must resume or classify reject). accept_wontfix is not resumed
// when this thread ID is in lastResolvedIDs (already resolved in the last
// persisted snapshot) or when lastReviewedSignalFingerprint equals the
// current-head signal computed with every currently-unresolved thread that has
// a matching accept_wontfix audit forced resolved (two-thread reopen even
// before IDs exist). last != current alone is not reopen — crash-after-accept
// (including a stale last or last==current) must still resume when the thread
// ID is not in lastResolvedIDs. Empty last still resumes (in-run retry).
func resumeDispositionDecisionFromRemoteAudit(thread ReviewThread, headSHA, looperLogin, lastSignal string, allThreads []ReviewThread, lastResolvedIDs []string) (threadResolutionAgentDecision, bool) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" || strings.TrimSpace(thread.ID) == "" {
		return threadResolutionAgentDecision{}, false
	}
	if ForceNeedsHumanAfterSecondDecline(thread, headSHA, looperLogin) {
		return threadResolutionAgentDecision{}, false
	}
	if latestValidatedAuditDecision(thread, looperLogin) != "reject_wontfix" {
		candidateFP := ThreadFeedbackFingerprintForLogin([]ReviewThread{thread}, looperLogin)
		if candidateFP != "" && hasThreadResolutionAuditForSignalForLogin(thread, thread.ID, headSHA, candidateFP, "accept_wontfix", looperLogin) {
			if threadIDInLastResolved(thread.ID, lastResolvedIDs) {
				return threadResolutionAgentDecision{}, false
			}
			last := strings.TrimSpace(lastSignal)
			if last != "" {
				live := allThreads
				if len(live) == 0 {
					live = []ReviewThread{thread}
				}
				if last == reviewSignalWithUnresolvedAcceptsForcedResolved(live, headSHA, looperLogin) {
					return threadResolutionAgentDecision{}, false
				}
			}
			return threadResolutionAgentDecision{
				ThreadID: thread.ID, Decision: "accept_wontfix",
				Evidence: "resume existing accept_wontfix audit", Confidence: "high",
			}, true
		}
	}
	excl := coordinationExcludedThreadFeedbackFingerprint(thread, looperLogin)
	if excl != "" && hasThreadResolutionAuditForSignalForLogin(thread, thread.ID, headSHA, excl, "reject_wontfix", looperLogin) {
		return threadResolutionAgentDecision{
			ThreadID: thread.ID, Decision: "reject_wontfix",
			Evidence: "resume existing reject_wontfix audit", Confidence: "high",
		}, true
	}
	return threadResolutionAgentDecision{}, false
}

func threadIDInLastResolved(threadID string, lastResolvedIDs []string) bool {
	want := strings.TrimSpace(threadID)
	if want == "" {
		return false
	}
	for _, id := range lastResolvedIDs {
		if strings.TrimSpace(id) == want {
			return true
		}
	}
	return false
}

func reviewSignalWithUnresolvedAcceptsForcedResolved(threads []ReviewThread, headSHA, looperLogin string) string {
	copied := make([]ReviewThread, len(threads))
	copy(copied, threads)
	for i := range copied {
		if copied[i].IsResolved {
			continue
		}
		candidateFP := ThreadFeedbackFingerprintForLogin([]ReviewThread{copied[i]}, looperLogin)
		if candidateFP != "" && hasThreadResolutionAuditForSignalForLogin(copied[i], copied[i].ID, headSHA, candidateFP, "accept_wontfix", looperLogin) {
			copied[i].IsResolved = true
		}
	}
	return ComputeReviewSignalFingerprintForLogin(headSHA, copied, looperLogin)
}

func threadHasValidatedFixerDeclineFromAuthor(thread ReviewThread, looperLogin string) bool {
	for _, comment := range thread.Comments {
		if isValidatedFixerDeclinedCommentFromAuthor(comment, looperLogin, thread.ID) {
			return true
		}
	}
	return false
}

// hasUnresolvedAcceptWontfixAudit is true when an unresolved Looper-authored
// thread already carries a validated accept_wontfix audit.
// Spec §8.2: a same-head remote audit is authority after a crash that lost the
// checkpoint cursor; Reviewer must still resolve instead of promoting the
// signal baseline and leaving Fixer permanently withheld.
// Spec §8.4: a later head is new adjudicated input, so an H1 accept must still
// be re-admitted on H2 for re-adjudication rather than disappearing from both
// roles while Fixer withholds the unresolved accept.
func hasUnresolvedAcceptWontfixAudit(thread ReviewThread, headSHA, looperLogin string) bool {
	if thread.IsResolved {
		return false
	}
	if decision, ok := resumeDispositionDecisionFromRemoteAudit(thread, headSHA, looperLogin, "", nil, nil); ok && decision.Decision == "accept_wontfix" {
		return true
	}
	return latestValidatedAuditDecision(thread, looperLogin) == "accept_wontfix"
}

// latestValidatedAuditDecision returns the newest complete Looper audit decision
// on the thread, regardless of the audit's recorded head.
func latestValidatedAuditDecision(thread ReviewThread, looperLogin string) string {
	decision := ""
	for _, comment := range thread.Comments {
		if !isValidatedThreadResolutionAudit(comment, looperLogin, thread.ID) {
			continue
		}
		fields, ok := parseThreadResolutionMarker(comment.Body)
		if !ok || !fields.HasFeedback || fields.ThreadID != thread.ID {
			continue
		}
		decision = fields.Decision
	}
	return decision
}

// parseFixerDeclinedMarker extracts thread/fingerprint from the exact decline marker.
func parseFixerDeclinedMarker(body string) (threadID, fingerprint string, ok bool) {
	m := fixerDeclinedMarkerRE.FindStringSubmatch(body)
	if len(m) < 3 {
		return "", "", false
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), true
}

func isValidatedFixerDeclinedComment(body string) bool {
	_, _, ok := parseFixerDeclinedMarker(body)
	return ok
}

// isValidatedFixerDeclinedCommentFromAuthor requires exact marker + Looper/Fixer identity.
// When containingThreadID is non-empty, the marker's thread field must match it exactly.
func isValidatedFixerDeclinedCommentFromAuthor(comment ReviewThreadComment, looperLogin, containingThreadID string) bool {
	threadID, fingerprint, ok := parseFixerDeclinedMarker(comment.Body)
	if !ok || fingerprint == "" {
		return false
	}
	if want := strings.TrimSpace(containingThreadID); want != "" && threadID != want {
		return false
	}
	return isLooperIdentityAuthor(comment.Author, looperLogin)
}

func parseFixerFixedMarker(body string) (threadID, commitSHA string, ok bool) {
	m := fixerFixedMarkerRE.FindStringSubmatch(body)
	if len(m) < 3 {
		return "", "", false
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), true
}

func isValidatedFixerFixedComment(body string) bool {
	threadID, commitSHA, ok := parseFixerFixedMarker(body)
	return ok && threadID != "" && commitSHA != ""
}

// isValidatedFixerFixedCommentFromAuthor requires exact marker + Looper identity.
// When containingThreadID is non-empty, the marker's thread field must match it exactly.
func isValidatedFixerFixedCommentFromAuthor(comment ReviewThreadComment, looperLogin, containingThreadID string) bool {
	threadID, commitSHA, ok := parseFixerFixedMarker(comment.Body)
	if !ok || threadID == "" || commitSHA == "" {
		return false
	}
	if want := strings.TrimSpace(containingThreadID); want != "" && threadID != want {
		return false
	}
	return isLooperIdentityAuthor(comment.Author, looperLogin)
}

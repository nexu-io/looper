package fixer

import (
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	decisionAcceptWontfix    = "accept_wontfix"
	decisionRejectWontfix    = "reject_wontfix"
	decisionObjectivelyFixed = "objectively_fixed"
	decisionNotFixed         = "not_fixed"
	decisionNeedsHuman       = "needs_human"
)

var (
	fixerLooperDirectiveRE = regexp.MustCompile(`(?is)^\s*/looper\s+(wontfix|reconsider)(?:\s+(.+))?\s*$`)
	fixerWontfixAliasRE    = regexp.MustCompile(`(?is)^\s*(wontfix|won't fix|won` + "\u2019" + `t fix)(?:\s*:\s*(.+))?\s*$`)
	// Exact looper:thread-resolution decision field (not incidental prose).
	fixerThreadResolutionDecisionRE = regexp.MustCompile(`(?is)<!--\s*looper:thread-resolution\b[^>]*\bdecision=(accept_wontfix|reject_wontfix|objectively_fixed|not_fixed|needs_human)\b[^>]*-->`)
	// Exact validated Fixer decline marker (HTML comment form).
	fixerDeclinedMarkerRE = regexp.MustCompile(`(?is)<!--\s*looper-fixer-reply-declined\b[^>]*-->`)
)

// ThreadWithheldFromFixer reports whether an unresolved Looper-authored thread
// must not receive further Fixer edits. Authority is Reviewer audit decisions
// and trusted human / validated Fixer disposition signals — not "any audit
// comment exists".
//
// Rules:
//   - Unaudited trusted human wontfix/reconsider → withheld
//   - Unaudited validated Fixer decline → withheld
//   - Latest Reviewer decision accept_wontfix (or objectively_fixed) while still
//     unresolved → withheld until the thread is actually resolved
//   - Latest Reviewer decision reject_wontfix (or not_fixed) → actionable again
//     on the existing thread (no new thread)
func ThreadWithheldFromFixer(thread ReviewThread, prAuthorLogin, looperLogin string) bool {
	if thread.IsResolved || len(thread.Comments) == 0 {
		return false
	}
	if !isLooperAuthoredFixerThread(thread, looperLogin) {
		return false
	}
	lastDecision, lastDecisionIdx, hasDecision := lastReviewerAuditDecision(thread, looperLogin)
	if hasUnauditedTrustedDispositionAfter(thread, prAuthorLogin, lastDecisionIdx) {
		return true
	}
	if hasUnauditedValidatedFixerDeclineAfter(thread, looperLogin, lastDecisionIdx) {
		return true
	}
	if !hasDecision {
		return false
	}
	switch lastDecision {
	case decisionAcceptWontfix, decisionObjectivelyFixed, decisionNeedsHuman:
		// Accept/resolve path: stay withheld while unresolved (e.g. accept reply
		// succeeded but resolve failed). needs_human also blocks Fixer edits.
		return true
	case decisionRejectWontfix, decisionNotFixed:
		return false
	default:
		return false
	}
}

func isLooperAuthoredFixerThread(thread ReviewThread, looperLogin string) bool {
	if len(thread.Comments) == 0 {
		return false
	}
	root := thread.Comments[0]
	if !hasLooperReviewRootMarker(root.Body) {
		return false
	}
	return isLooperIdentityAuthor(root.Author, looperLogin)
}

func hasLooperReviewRootMarker(body string) bool {
	return strings.Contains(body, "looper:stamp") || strings.Contains(body, "looper:review")
}

// isLooperIdentityAuthor reports whether author is the configured Looper/Fixer
// identity (current login). Empty looperLogin never matches — callers must
// supply the live login; spoofed stamps from other authors do not count.
func isLooperIdentityAuthor(authorLogin, looperLogin string) bool {
	return sameGitHubLogin(authorLogin, looperLogin)
}

func lastReviewerAuditDecision(thread ReviewThread, looperLogin string) (decision string, idx int, ok bool) {
	idx = -1
	for i, comment := range thread.Comments {
		if !isLooperIdentityAuthor(comment.Author, looperLogin) {
			continue
		}
		if d, found := parseThreadResolutionDecision(comment.Body); found {
			decision = d
			idx = i
			ok = true
		}
	}
	return decision, idx, ok
}

func parseThreadResolutionDecision(body string) (string, bool) {
	m := fixerThreadResolutionDecisionRE.FindStringSubmatch(body)
	if len(m) < 2 {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(m[1])), true
}

func isValidatedFixerDeclineComment(comment ReviewThreadComment, looperLogin string) bool {
	if !isLooperIdentityAuthor(comment.Author, looperLogin) {
		return false
	}
	return fixerDeclinedMarkerRE.MatchString(comment.Body)
}

func hasUnauditedTrustedDispositionAfter(thread ReviewThread, prAuthorLogin string, afterIdx int) bool {
	var afterTime time.Time
	if afterIdx >= 0 && afterIdx < len(thread.Comments) {
		afterTime = commentLatestTime(thread.Comments[afterIdx])
	}
	for i := len(thread.Comments) - 1; i >= 0; i-- {
		comment := thread.Comments[i]
		if !parseTrustedWontfixOrReconsider(comment.Body) {
			continue
		}
		if !isTrustedDispositionAuthority(comment.Author, prAuthorLogin, commentAuthorAssociation(comment)) {
			continue
		}
		if afterIdx >= 0 && i <= afterIdx {
			if afterTime.IsZero() || !commentLatestTime(comment).After(afterTime) {
				continue
			}
		}
		return true
	}
	return false
}

func hasUnauditedValidatedFixerDeclineAfter(thread ReviewThread, looperLogin string, afterIdx int) bool {
	for i := len(thread.Comments) - 1; i > afterIdx; i-- {
		if isValidatedFixerDeclineComment(thread.Comments[i], looperLogin) {
			return true
		}
	}
	return false
}

// lastRejectWontfixIndex returns the index of the latest Looper-authored
// reject_wontfix audit, or -1 when none exists.
func lastRejectWontfixIndex(thread ReviewThread, looperLogin string) int {
	idx := -1
	for i, comment := range thread.Comments {
		if !isLooperIdentityAuthor(comment.Author, looperLogin) {
			continue
		}
		if d, ok := parseThreadResolutionDecision(comment.Body); ok && d == decisionRejectWontfix {
			idx = i
		}
	}
	return idx
}

func commentAuthorAssociation(comment ReviewThreadComment) string {
	// Association is optional on the fixer gateway type; when absent, only PR
	// author match grants authority.
	return strings.TrimSpace(comment.AuthorAssociation)
}

func isTrustedDispositionAuthority(authorLogin, prAuthorLogin, authorAssociation string) bool {
	author := strings.ToLower(strings.TrimSpace(authorLogin))
	if author == "" {
		return false
	}
	if strings.HasSuffix(author, "[bot]") || strings.EqualFold(authorAssociation, "BOT") {
		return false
	}
	if pr := strings.ToLower(strings.TrimSpace(prAuthorLogin)); pr != "" && author == pr {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(authorAssociation)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

func parseTrustedWontfixOrReconsider(body string) bool {
	content := stripQuotedAndHTML(body)
	if content == "" {
		return false
	}
	if fixerLooperDirectiveRE.MatchString(content) {
		return true
	}
	return fixerWontfixAliasRE.MatchString(content)
}

func stripQuotedAndHTML(body string) string {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		if strings.Contains(line, "<!--") {
			cleaned := stripHTMLCommentsFixer(line)
			if strings.TrimSpace(cleaned) == "" {
				continue
			}
			kept = append(kept, cleaned)
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func stripHTMLCommentsFixer(s string) string {
	var b strings.Builder
	rest := s
	for {
		start := strings.Index(rest, "<!--")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		end := strings.Index(rest[start:], "-->")
		if end < 0 {
			break
		}
		rest = rest[start+end+3:]
	}
	return b.String()
}

// SuppressWithheldDispositionFixItems drops comment fix items whose threads are
// withheld pending Reviewer adjudication. Non-comment items pass through.
func SuppressWithheldDispositionFixItems(fixItems []FixItem, threads []ReviewThread, prAuthorLogin, looperLogin string) []FixItem {
	if len(fixItems) == 0 || len(threads) == 0 {
		return fixItems
	}
	withheld := map[string]struct{}{}
	for _, thread := range threads {
		if ThreadWithheldFromFixer(thread, prAuthorLogin, looperLogin) {
			withheld[strings.TrimSpace(thread.ID)] = struct{}{}
		}
	}
	if len(withheld) == 0 {
		return fixItems
	}
	out := make([]FixItem, 0, len(fixItems))
	for _, item := range fixItems {
		if item.Type == "comment" {
			if _, ok := withheld[strings.TrimSpace(item.ThreadID)]; ok {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

// declineMarkerForThread chooses the decline HTML marker. After a Reviewer
// reject_wontfix that followed a same-fingerprint decline, use a distinct
// post-reject marker so one new decline is visible without a ledger.
func declineMarkerForThread(thread ReviewThread, threadID, decisionFingerprint, looperLogin string) string {
	base := fixerDeclinedReplyMarker(threadID, decisionFingerprint)
	if base == "" {
		return ""
	}
	lastReject := lastRejectWontfixIndex(thread, looperLogin)
	if lastReject < 0 {
		return base
	}
	for i := 0; i <= lastReject && i < len(thread.Comments); i++ {
		comment := thread.Comments[i]
		if !isValidatedFixerDeclineComment(comment, looperLogin) {
			continue
		}
		if strings.Contains(comment.Body, base) {
			return fixerDeclinedReplyMarkerPostReject(threadID, decisionFingerprint)
		}
	}
	return base
}

// hasValidatedDeclineWithMarkerAfter reports a Looper-authored decline carrying
// marker at an index strictly after afterIdx (-1 = entire thread).
func hasValidatedDeclineWithMarkerAfter(thread ReviewThread, marker, looperLogin string, afterIdx int) bool {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return false
	}
	for i := afterIdx + 1; i < len(thread.Comments); i++ {
		comment := thread.Comments[i]
		if !isValidatedFixerDeclineComment(comment, looperLogin) {
			continue
		}
		if strings.Contains(comment.Body, marker) {
			return true
		}
	}
	return false
}

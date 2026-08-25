package reviewer

import (
	"regexp"
	"strings"
	"unicode"
)

// Disposition kinds recognized inside Looper-authored review threads.
const (
	DispositionWontfix    = "wontfix"
	DispositionReconsider = "reconsider"
)

// TrustedDisposition is a parsed human directive on a Looper-authored thread.
type TrustedDisposition struct {
	Kind   string
	Reason string
}

var (
	// Canonical: /looper wontfix <reason>  and  /looper reconsider <reason>
	looperDirectiveRE = regexp.MustCompile(`(?is)^\s*/looper\s+(wontfix|reconsider)(?:\s+(.+))?\s*$`)

	// Compatibility aliases: entire non-quoted content is one of the phrases,
	// optionally followed by ": <reason>".
	wontfixAliasRE = regexp.MustCompile(`(?is)^\s*(wontfix|won't fix|won` + "\u2019" + `t fix)(?:\s*:\s*(.+))?\s*$`)
)

// stripQuotedMarkdownLines removes block-quoted lines so alias matching applies
// only to the author's own non-quoted content.
func stripQuotedMarkdownLines(body string) string {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		// Drop HTML comment markers (audit/disclosure) from the parse surface.
		if strings.Contains(trimmed, "<!--") {
			// Remove HTML comments inline.
			cleaned := stripHTMLComments(line)
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

func stripHTMLComments(s string) string {
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

// ParseTrustedDispositionDirective parses a single comment body for a trusted
// disposition directive. Incidental prose containing the word "wontfix" is not
// a command. Returns ok=false when no directive is present.
func ParseTrustedDispositionDirective(body string) (TrustedDisposition, bool) {
	content := stripQuotedMarkdownLines(body)
	if content == "" {
		return TrustedDisposition{}, false
	}
	if m := looperDirectiveRE.FindStringSubmatch(content); len(m) >= 2 {
		kind := strings.ToLower(strings.TrimSpace(m[1]))
		reason := ""
		if len(m) > 2 {
			reason = strings.TrimSpace(m[2])
		}
		return TrustedDisposition{Kind: kind, Reason: reason}, true
	}
	if m := wontfixAliasRE.FindStringSubmatch(content); len(m) >= 2 {
		reason := ""
		if len(m) > 2 {
			reason = strings.TrimSpace(m[2])
		}
		return TrustedDisposition{Kind: DispositionWontfix, Reason: reason}, true
	}
	return TrustedDisposition{}, false
}

// IsTrustedDispositionAuthority reports whether the commenter may issue a
// disposition directive. Authority is the PR author or OWNER/MEMBER/COLLABORATOR.
// Bots and external users are not authority. Fixer declined markers are never
// human authority (callers must not pass them here as human directives).
func IsTrustedDispositionAuthority(authorLogin, prAuthorLogin, authorAssociation string) bool {
	author := normalizeLogin(authorLogin)
	if author == "" {
		return false
	}
	if strings.HasSuffix(author, "[bot]") || strings.EqualFold(authorAssociation, "BOT") {
		return false
	}
	if prAuthor := normalizeLogin(prAuthorLogin); prAuthor != "" && author == prAuthor {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(authorAssociation)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

// LatestTrustedDisposition returns the latest valid trusted-human directive on
// a Looper-authored thread after the most recent Reviewer audit comment. When
// the thread is not Looper-authored, ok is false.
func LatestTrustedDisposition(thread ReviewThread, prAuthorLogin string) (TrustedDisposition, ReviewThreadComment, bool) {
	return LatestTrustedDispositionForLogin(thread, prAuthorLogin, "")
}

// LatestTrustedDispositionForLogin is identity-aware for Looper audit markers.
func LatestTrustedDispositionForLogin(thread ReviewThread, prAuthorLogin, looperLogin string) (TrustedDisposition, ReviewThreadComment, bool) {
	if !isLooperAuthoredThreadForLogin(thread, looperLogin) {
		return TrustedDisposition{}, ReviewThreadComment{}, false
	}
	// Walk newest → oldest; first non-audit trusted directive wins.
	lastAuditIdx := -1
	for i, comment := range thread.Comments {
		if isValidatedThreadResolutionAudit(comment, looperLogin) {
			lastAuditIdx = i
		}
	}
	for i := len(thread.Comments) - 1; i > lastAuditIdx; i-- {
		comment := thread.Comments[i]
		if isValidatedThreadResolutionAudit(comment, looperLogin) {
			continue
		}
		if isValidatedFixerDeclinedCommentFromAuthor(comment, looperLogin) || isValidatedFixerFixedCommentFromAuthor(comment, looperLogin) {
			continue
		}
		directive, ok := ParseTrustedDispositionDirective(comment.Body)
		if !ok {
			continue
		}
		if !IsTrustedDispositionAuthority(comment.Author, prAuthorLogin, comment.AuthorAssociation) {
			continue
		}
		return directive, comment, true
	}
	return TrustedDisposition{}, ReviewThreadComment{}, false
}

// HasUnauditedTrustedDisposition reports a new trusted human disposition that
// Reviewer has not yet audited (no matching extended marker after it).
func HasUnauditedTrustedDisposition(thread ReviewThread, prAuthorLogin string) bool {
	return HasUnauditedTrustedDispositionForLogin(thread, prAuthorLogin, "")
}

func HasUnauditedTrustedDispositionForLogin(thread ReviewThread, prAuthorLogin, looperLogin string) bool {
	if thread.IsResolved {
		return false
	}
	directive, _, ok := LatestTrustedDispositionForLogin(thread, prAuthorLogin, looperLogin)
	if !ok {
		return false
	}
	// reconsider cancels accepted disposition; still a changed signal for Reviewer.
	return directive.Kind == DispositionWontfix || directive.Kind == DispositionReconsider
}

// HasUnauditedValidatedFixerDecline reports a validated Fixer decline reply that
// has not been followed by a Reviewer audit on the same thread.
func HasUnauditedValidatedFixerDecline(thread ReviewThread) bool {
	return HasUnauditedValidatedFixerDeclineForLogin(thread, "")
}

func HasUnauditedValidatedFixerDeclineForLogin(thread ReviewThread, looperLogin string) bool {
	if thread.IsResolved || !isLooperAuthoredThreadForLogin(thread, looperLogin) {
		return false
	}
	lastAuditIdx := -1
	lastDeclineIdx := -1
	for i, comment := range thread.Comments {
		if isValidatedThreadResolutionAudit(comment, looperLogin) {
			lastAuditIdx = i
		}
		if isValidatedFixerDeclinedCommentFromAuthor(comment, looperLogin) {
			lastDeclineIdx = i
		}
	}
	return lastDeclineIdx >= 0 && lastDeclineIdx > lastAuditIdx
}

// ThreadHasChangedDispositionSignal is true when a Looper-authored unresolved
// thread carries an unaudited trusted human disposition or validated Fixer decline.
func ThreadHasChangedDispositionSignal(thread ReviewThread, prAuthorLogin string) bool {
	return ThreadHasChangedDispositionSignalForLogin(thread, prAuthorLogin, "")
}

func ThreadHasChangedDispositionSignalForLogin(thread ReviewThread, prAuthorLogin, looperLogin string) bool {
	if thread.IsResolved || !isLooperAuthoredThreadForLogin(thread, looperLogin) {
		return false
	}
	return HasUnauditedTrustedDispositionForLogin(thread, prAuthorLogin, looperLogin) || HasUnauditedValidatedFixerDeclineForLogin(thread, looperLogin)
}

// ThreadsHaveChangedDispositionSignal reports whether any thread qualifies.
func ThreadsHaveChangedDispositionSignal(threads []ReviewThread, prAuthorLogin string) bool {
	return ThreadsHaveChangedDispositionSignalForLogin(threads, prAuthorLogin, "")
}

func ThreadsHaveChangedDispositionSignalForLogin(threads []ReviewThread, prAuthorLogin, looperLogin string) bool {
	for _, thread := range threads {
		if ThreadHasChangedDispositionSignalForLogin(thread, prAuthorLogin, looperLogin) {
			return true
		}
	}
	return false
}

// ForceNeedsHumanAfterSecondDecline reports §8.4 one-rejection quota: after
// Reviewer reject_wontfix on an input, a later validated Fixer decline with
// unchanged head + same canonical human/original-finding feedback forces
// needs_human without another classifier argument. Changed head or changed
// trusted human directive is new adjudicated input (quota resets).
func ForceNeedsHumanAfterSecondDecline(thread ReviewThread, headSHA, looperLogin string) bool {
	if thread.IsResolved || !isLooperAuthoredThreadForLogin(thread, looperLogin) {
		return false
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return false
	}
	// Find last reject_wontfix audit for this head, then a later validated decline.
	lastRejectIdx := -1
	rejectFeedback := ""
	for i, comment := range thread.Comments {
		if !isValidatedThreadResolutionAudit(comment, looperLogin) {
			continue
		}
		fields, ok := parseThreadResolutionMarker(comment.Body)
		if !ok {
			continue
		}
		if fields.Decision != "reject_wontfix" {
			continue
		}
		if fields.HeadSHA != "" && fields.HeadSHA != headSHA {
			continue
		}
		lastRejectIdx = i
		rejectFeedback = fields.Feedback
	}
	if lastRejectIdx < 0 {
		return false
	}
	foundDecline := false
	for i := lastRejectIdx + 1; i < len(thread.Comments); i++ {
		comment := thread.Comments[i]
		// Coordination replies (non-decline audits) do not count.
		if isValidatedThreadResolutionAudit(comment, looperLogin) {
			continue
		}
		if isValidatedFixerDeclinedCommentFromAuthor(comment, looperLogin) {
			foundDecline = true
			break
		}
	}
	if !foundDecline {
		return false
	}
	// Unchanged-input only: coordination-excluded feedback must match the
	// reject marker's feedback fingerprint (audits + fixer declines excluded).
	if rejectFeedback == "" {
		return false
	}
	live := coordinationExcludedThreadFeedbackFingerprint(thread, looperLogin)
	return live != "" && live == rejectFeedback
}

// coordinationExcludedThreadFeedbackFingerprint hashes human/original-finding
// feedback on a Looper-authored thread after excluding validated Reviewer audits
// and validated Fixer decline/fixed coordination replies.
func coordinationExcludedThreadFeedbackFingerprint(thread ReviewThread, looperLogin string) string {
	if !isLooperAuthoredThreadForLogin(thread, looperLogin) {
		return ""
	}
	filtered := ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved}
	for _, comment := range thread.Comments {
		if isValidatedThreadResolutionAudit(comment, looperLogin) {
			continue
		}
		if isValidatedFixerDeclinedCommentFromAuthor(comment, looperLogin) || isValidatedFixerFixedCommentFromAuthor(comment, looperLogin) {
			continue
		}
		filtered.Comments = append(filtered.Comments, comment)
	}
	return ThreadFeedbackFingerprintForLogin([]ReviewThread{filtered}, looperLogin)
}

// LastReviewerDispositionDecision returns the latest validated Reviewer
// disposition decision on the thread for the given head, if any.
func LastReviewerDispositionDecision(thread ReviewThread, headSHA, looperLogin string) (decision string, ok bool) {
	headSHA = strings.TrimSpace(headSHA)
	for i := len(thread.Comments) - 1; i >= 0; i-- {
		comment := thread.Comments[i]
		if !isValidatedThreadResolutionAudit(comment, looperLogin) {
			continue
		}
		fields, parsed := parseThreadResolutionMarker(comment.Body)
		if !parsed {
			continue
		}
		if headSHA != "" && fields.HeadSHA != "" && fields.HeadSHA != headSHA {
			continue
		}
		switch fields.Decision {
		case "accept_wontfix", "reject_wontfix", "needs_human":
			return fields.Decision, true
		}
	}
	return "", false
}

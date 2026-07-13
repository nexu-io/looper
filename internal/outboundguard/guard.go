// Package outboundguard is a best-effort brake on obvious secret-shaped content
// in agent publications (review bodies, comments, PR text).
//
// It is intentionally incomplete: prefer low false positives over exhaustive
// DLP. Short or ambiguous names (DB_PASS, PGPASSWORD, ACCESS_KEY, …) may slip
// through; structural signals (credential URLs, PEM blocks, env dumps, high
// entropy) catch the common paste mistakes without a growing synonym list.
package outboundguard

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
)

const (
	environmentAssignmentLimit = 5
	highEntropyThreshold       = 4.25

	// RecoveryGuidance is appended to rejection errors so in-session agents can
	// rewrite and resubmit without Looper echoing the rejected secret content.
	RecoveryGuidance = "rewrite the rejected field in plain review prose without secret-shaped env assignments (NAME=value), credential-bearing URLs, multi-line environment dumps, or high-entropy tokens; resubmit through the same tool in this session; do not exit solely because of this rejection; never paste the rejected value into logs, prompts, or comments"
)

var (
	environmentAssignmentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// user:password@ or password-only :password@ in URL userinfo.
	credentialURLRE = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/@\s:]*:[^/@\s]+@`)
	// High-confidence query credential params only (not short aliases like pass/pwd).
	credentialQueryRE      = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s]*[?&](?:[^=&\s]*_)?(?:password|secret|client[_-]?secret|api[_-]?key|access[_-]?token|private[_-]?key)=[^\s&"'<>]+`)
	privateKeyRE           = regexp.MustCompile(`(?i)-----BEGIN(?: [A-Z0-9]+)? PRIVATE KEY-----`)
	highEntropyCandidateRE = regexp.MustCompile(`[A-Za-z0-9_+/=-]{24,}`)
	gitObjectIDRE          = regexp.MustCompile(`(?i)^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
	uuidRE                 = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// sensitiveEnvNameKeywords are long, high-confidence segments only. Matched as
// whole underscore/hyphen-delimited segments so PASSWORDLESS / TOKENIZATION /
// SECRETS_MANAGER are not rejected. Short ambiguous words (pass, pwd) are
// deliberately omitted.
var sensitiveEnvNameKeywords = []string{
	"password",
	"secret",
	"api_key",
	"apikey",
}

type Field struct {
	Name string
	Text string
}

// Rejection is returned when outbound content fails the safety gate.
// Error text includes the field name and detection category only — never the
// rejected value — plus recovery guidance for same-session rewrite/resubmit.
type Rejection struct {
	Field  string
	Reason string
}

func (e *Rejection) Error() string {
	if e == nil {
		return "outbound content safety gate rejected content"
	}
	return fmt.Sprintf("outbound content safety gate rejected %s: %s; %s", e.Field, e.Reason, RecoveryGuidance)
}

// IsRejection reports whether err is or wraps a content-safety Rejection.
func IsRejection(err error) bool {
	var rejected *Rejection
	return errors.As(err, &rejected)
}

func Validate(fields ...Field) error {
	for _, field := range fields {
		if reason := unsafeText(field.Text); reason != "" {
			return &Rejection{Field: field.Name, Reason: reason}
		}
	}
	return nil
}

func unsafeText(text string) string {
	if credentialURLRE.MatchString(text) || credentialQueryRE.MatchString(text) {
		return "contains a credential-bearing connection URL"
	}
	if privateKeyRE.MatchString(text) {
		return "contains a private key block"
	}
	environmentAssignments := 0
	for _, rawLine := range strings.Split(text, "\n") {
		line := stripShellAssignmentDecorators(stripShellLinePrefix(rawLine))
		if isSensitiveAssignment(line) {
			return "contains a credential-shaped environment assignment"
		}
		if environmentAssignmentRE.MatchString(line) {
			environmentAssignments++
		}
	}
	if environmentAssignments >= environmentAssignmentLimit {
		return "contains an environment-dump-shaped block"
	}
	for _, token := range highEntropyCandidateRE.FindAllString(text, -1) {
		if gitObjectIDRE.MatchString(token) || uuidRE.MatchString(token) {
			continue
		}
		if characterClassCount(token) >= 3 && shannonEntropy(token) >= highEntropyThreshold {
			return "contains a high-entropy credential-shaped token"
		}
	}
	return ""
}

// isSensitiveAssignment reports high-confidence env-style credential lines such
// as PASSWORD=..., export OPENAI_API_KEY=..., or SERVICE_TOKEN=....
// Requires shell/env form NAME=value with no spaces around '='.
//
// Boolean-looking values are treated as configuration (has_password_field=true).
func isSensitiveAssignment(line string) bool {
	line = strings.TrimSpace(line)
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return false
	}
	name := line[:eq]
	if !isEnvVarName(name) {
		return false
	}
	if !isSensitiveEnvName(name) {
		return false
	}
	return !looksLikeBooleanConfigValue(line[eq+1:])
}

func stripShellAssignmentDecorators(line string) string {
	line = strings.TrimSpace(line)
	if next, ok := stripLeadingKeyword(line, "export"); ok {
		line = next
	}
	if next, ok := stripLeadingKeywordWithFlags(line, "declare"); ok {
		return next
	}
	if next, ok := stripLeadingKeywordWithFlags(line, "typeset"); ok {
		return next
	}
	return line
}

func stripLeadingKeyword(line, keyword string) (string, bool) {
	if len(line) < len(keyword)+1 || !strings.EqualFold(line[:len(keyword)], keyword) || !isASCIISpace(line[len(keyword)]) {
		return line, false
	}
	return strings.TrimSpace(line[len(keyword):]), true
}

func stripLeadingKeywordWithFlags(line, keyword string) (string, bool) {
	rest, ok := stripLeadingKeyword(line, keyword)
	if !ok {
		return line, false
	}
	strippedFlags := false
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" || rest[0] != '-' {
			break
		}
		end := 1
		for end < len(rest) && !isASCIISpace(rest[end]) {
			if !isShellFlagChar(rest[end]) {
				return line, false
			}
			end++
		}
		if end == 1 {
			return line, false
		}
		rest = strings.TrimSpace(rest[end:])
		strippedFlags = true
	}
	if !strippedFlags || rest == "" {
		return line, false
	}
	return rest, true
}

func isShellFlagChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isEnvVarName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && r != '-' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func isSensitiveEnvName(name string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(name, "-", "_"))
	for _, keyword := range sensitiveEnvNameKeywords {
		if hasDelimitedKeyword(normalized, keyword) {
			return true
		}
	}
	// token: full name or trailing segment only (SERVICE_TOKEN, MY_TOKEN).
	// Not leading TOKEN_* (TOKEN_COUNT) and not middle (refresh_token_ttl).
	if normalized == "token" || strings.HasSuffix(normalized, "_token") {
		return true
	}
	return false
}

func looksLikeBooleanConfigValue(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if q := value[0]; (q == '"' || q == '\'') && value[len(value)-1] == q {
			value = value[1 : len(value)-1]
		}
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "false", "yes", "no", "on", "off", "null", "none", "nil":
		return true
	default:
		return false
	}
}

func hasDelimitedKeyword(name, keyword string) bool {
	if keyword == "" {
		return false
	}
	for start := 0; start <= len(name); {
		idx := strings.Index(name[start:], keyword)
		if idx < 0 {
			return false
		}
		idx += start
		beforeOK := idx == 0 || name[idx-1] == '_'
		after := idx + len(keyword)
		afterOK := after == len(name) || name[after] == '_'
		if beforeOK && afterOK {
			return true
		}
		start = idx + 1
	}
	return false
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t'
}

func stripShellLinePrefix(line string) string {
	line = strings.TrimSpace(line)
	for {
		if !strings.HasPrefix(line, "+") {
			break
		}
		if len(line) == 1 {
			return ""
		}
		next := line[1]
		if next == '+' {
			line = line[1:]
			continue
		}
		if next == ' ' || next == '\t' {
			line = strings.TrimSpace(line[1:])
			continue
		}
		break
	}
	if len(line) >= 2 {
		switch line[0] {
		case '$', '#', '%':
			if line[1] == ' ' || line[1] == '\t' {
				line = strings.TrimSpace(line[1:])
			}
		}
	}
	return line
}

func characterClassCount(value string) int {
	classes := 0
	var lower, upper, digit, symbol bool
	for _, r := range value {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		default:
			symbol = true
		}
	}
	for _, present := range []bool{lower, upper, digit, symbol} {
		if present {
			classes++
		}
	}
	return classes
}

func shannonEntropy(value string) float64 {
	if len(value) == 0 {
		return 0
	}
	counts := make(map[byte]int)
	for i := 0; i < len(value); i++ {
		counts[value[i]]++
	}
	length := float64(len(value))
	entropy := 0.0
	for _, count := range counts {
		probability := float64(count) / length
		entropy -= probability * math.Log2(probability)
	}
	return entropy
}

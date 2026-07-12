package outboundguard

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
)

const (
	environmentAssignmentLimit = 5
	highEntropyThreshold       = 4.25
)

var (
	environmentAssignmentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	sensitiveAssignmentRE   = regexp.MustCompile(`(?i)^(?:export[ \t]+)?(?:[A-Za-z_][A-Za-z0-9_]*)?(?:api[_-]?key|token|secret|password|credential)[A-Za-z0-9_]*[ \t]*=`)
	credentialURLRE         = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/@\s:]+:[^/@\s]+@`)
	highEntropyCandidateRE  = regexp.MustCompile(`[A-Za-z0-9_+/=-]{24,}`)
	gitObjectIDRE           = regexp.MustCompile(`(?i)^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
	uuidRE                  = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

type Field struct {
	Name string
	Text string
}

func Validate(fields ...Field) error {
	for _, field := range fields {
		if reason := unsafeText(field.Text); reason != "" {
			return fmt.Errorf("outbound content safety gate rejected %s: %s", field.Name, reason)
		}
	}
	return nil
}

func unsafeText(text string) string {
	if credentialURLRE.MatchString(text) {
		return "contains a credential-bearing connection URL"
	}
	environmentAssignments := 0
	for _, rawLine := range strings.Split(text, "\n") {
		line := stripShellLinePrefix(rawLine)
		if sensitiveAssignmentRE.MatchString(line) {
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

// stripShellLinePrefix removes common shell prompt and xtrace prefixes so
// assignment detection works on terminal output copied into review text.
// Examples: "$ SERVICE_TOKEN=...", "+ export TOKEN=...", "++ FOO=bar".
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

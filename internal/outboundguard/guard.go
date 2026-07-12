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
	environmentAssignments := 0
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
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

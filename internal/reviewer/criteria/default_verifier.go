package criteria

import "strings"

type DefaultVerifier struct{}

func NewDefaultVerifier() Verifier { return DefaultVerifier{} }

func (DefaultVerifier) VerifyCriterion(criterion AcceptanceCriterion, diff PRDiff) (CriterionAssessment, error) {
	added := collectAddedLines(diff)
	criterionText := strings.ToLower(strings.TrimSpace(string(criterion)))
	tokens := criterionTokens(criterionText)
	for _, line := range added {
		lineLower := strings.ToLower(strings.TrimSpace(line.text))
		if lineLower == "" {
			continue
		}
		if strings.Contains(lineLower, criterionText) || tokenOverlap(tokens, criterionTokens(lineLower)) {
			return CriterionAssessment{Verdict: VerdictPass, Justification: "diff contains matching implementation evidence", Evidence: []Evidence{{FilePath: lineFilePath(line), StartLine: lineStart(line), EndLine: lineEnd(line)}}}, nil
		}
	}
	return CriterionAssessment{Verdict: VerdictUnverifiable, Justification: "diff does not contain deterministic evidence matching this criterion"}, nil
}

type addedLine struct {
	filePath string
	line     int
	text     string
}

func lineFilePath(line addedLine) string { return line.filePath }
func lineStart(line addedLine) int       { return line.line }
func lineEnd(line addedLine) int         { return line.line }

func collectAddedLines(diff PRDiff) []addedLine {
	lines := []addedLine{}
	for _, file := range diff.Files {
		lineNo := 0
		for _, raw := range strings.Split(file.Patch, "\n") {
			if strings.HasPrefix(raw, "@@") {
				lineNo = hunkStart(raw)
				continue
			}
			if strings.HasPrefix(raw, "+") && !strings.HasPrefix(raw, "+++") {
				lines = append(lines, addedLine{filePath: file.Path, line: max(lineNo, 1), text: raw[1:]})
				lineNo++
				continue
			}
			if strings.HasPrefix(raw, "-") && !strings.HasPrefix(raw, "---") {
				continue
			}
			if raw != "" {
				lineNo++
			}
		}
	}
	return lines
}

func hunkStart(raw string) int {
	parts := strings.Split(raw, " ")
	for _, part := range parts {
		if !strings.HasPrefix(part, "+") {
			continue
		}
		part = strings.TrimPrefix(part, "+")
		part = strings.TrimSuffix(part, "@@")
		if index := strings.Index(part, ","); index >= 0 {
			part = part[:index]
		}
		part = strings.TrimSpace(part)
		if part == "" {
			return 0
		}
		value := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				return value
			}
			value = value*10 + int(r-'0')
		}
		return value
	}
	return 0
}

func criterionTokens(value string) []string {
	words := strings.FieldsFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	tokens := make([]string, 0, len(words))
	for _, word := range words {
		if len(word) >= 4 {
			tokens = append(tokens, word)
		}
	}
	return tokens
}

func tokenOverlap(a []string, b []string) bool {
	if len(a) < 2 || len(b) == 0 {
		return false
	}
	set := map[string]struct{}{}
	for _, token := range a {
		set[token] = struct{}{}
	}
	matches := 0
	for _, token := range b {
		if _, ok := set[token]; ok {
			matches++
			delete(set, token)
		}
	}
	return matches >= min(2, len(a))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

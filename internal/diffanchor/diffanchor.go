package diffanchor

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	SideRight = "RIGHT"
	SideLeft  = "LEFT"
)

var (
	topLevelFileLocationRe    = regexp.MustCompile(`(?m)(^|[\s(])((?:[\w.-]+/)+[\w.-]+|[\w./-]+\.[A-Za-z0-9]+|(?:Dockerfile|Makefile|Containerfile|Jenkinsfile|Procfile|Rakefile|Gemfile|Vagrantfile|Brewfile|Justfile|Taskfile|Tiltfile|Earthfile|BUILD|WORKSPACE|LICENSE|NOTICE|README|CHANGELOG|AUTHORS|CODEOWNERS))(?::\d+(?:-\d+)?)?\b`)
	topLevelHeadingLocationRe = regexp.MustCompile(`(?m)^#{1,6}\s+\S+`)
	topLevelLineLocationRe    = regexp.MustCompile(`(?i)\b(?:lines?\s+\d+(?:\s*[-–]\s*\d+)?|L\d+(?:\s*[-–]\s*L?\d+)?)\b`)
	topLevelNamedLocationRe   = regexp.MustCompile(`(?i)\b(?:section|symbol|function|method|type|struct|package)\s+([` + "`" + `"']?[\w./:#-]+)`)
)

type Anchor struct {
	Path      string
	Line      int64
	Side      string
	StartLine int64
	StartSide string
}

type Range struct {
	Path    string
	Side    string
	Start   int64
	End     int64
	Excerpt string
	Heading string
}

type Index struct {
	Ranges []Range
}

type ValidationResult struct {
	Valid          bool
	Reason         string
	LocationText   string
	QualityFlagged bool
}

var hunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func Parse(diff string) Index {
	var idx Index
	var path string
	var oldLine, newLine int64
	var rightHeading, leftHeading string
	inHunk := false
	var openRight, openLeft *Range
	flush := func() {
		if openRight != nil {
			idx.Ranges = append(idx.Ranges, *openRight)
			openRight = nil
		}
		if openLeft != nil {
			idx.Ranges = append(idx.Ranges, *openLeft)
			openLeft = nil
		}
	}
	add := func(side string, line int64, text string, heading string) {
		if path == "" || line <= 0 {
			return
		}
		text = strings.TrimSpace(text)
		if len(text) > 96 {
			text = text[:93] + "..."
		}
		if side == SideRight {
			if openRight == nil || openRight.Path != path || openRight.Side != side || openRight.End+1 != line {
				if openRight != nil {
					idx.Ranges = append(idx.Ranges, *openRight)
				}
				openRight = &Range{Path: path, Side: side, Start: line, Heading: heading}
			}
			openRight.End = line
			if openRight.Excerpt == "" && text != "" {
				openRight.Excerpt = text
			}
			return
		}
		if openLeft == nil || openLeft.Path != path || openLeft.Side != side || openLeft.End+1 != line {
			if openLeft != nil {
				idx.Ranges = append(idx.Ranges, *openLeft)
			}
			openLeft = &Range{Path: path, Side: side, Start: line, Heading: heading}
		}
		openLeft.End = line
		if openLeft.Excerpt == "" && text != "" {
			openLeft.Excerpt = text
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			path = gitDiffPath(line)
			rightHeading, leftHeading = "", ""
			inHunk = false
			continue
		}
		if !inHunk && strings.HasPrefix(line, "+++ ") {
			if parsed := fileHeaderPath(line[4:]); parsed != "" {
				path = parsed
			}
			continue
		}
		if m := hunkRE.FindStringSubmatch(line); m != nil {
			flush()
			oldLine = parseInt64(m[1])
			newLine = parseInt64(m[3])
			inHunk = true
			continue
		}
		if path == "" || line == "" || (!inHunk && strings.HasPrefix(line, "--- ")) || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "new file mode") || strings.HasPrefix(line, "deleted file mode") {
			continue
		}
		switch line[0] {
		case ' ':
			content := line[1:]
			if h := headingFor(path, content); h != "" {
				rightHeading, leftHeading = h, h
			}
			add(SideRight, newLine, content, rightHeading)
			oldLine++
			newLine++
		case '+':
			content := line[1:]
			if h := headingFor(path, content); h != "" {
				rightHeading = h
			}
			add(SideRight, newLine, content, rightHeading)
			newLine++
		case '-':
			content := line[1:]
			if h := headingFor(path, content); h != "" {
				leftHeading = h
			}
			add(SideLeft, oldLine, content, leftHeading)
			oldLine++
		}
	}
	flush()
	sort.SliceStable(idx.Ranges, func(i, j int) bool {
		if idx.Ranges[i].Path != idx.Ranges[j].Path {
			return idx.Ranges[i].Path < idx.Ranges[j].Path
		}
		if idx.Ranges[i].Side != idx.Ranges[j].Side {
			return idx.Ranges[i].Side < idx.Ranges[j].Side
		}
		return idx.Ranges[i].Start < idx.Ranges[j].Start
	})
	return idx
}

func (idx Index) FormatPromptSection(limit int) string {
	if len(idx.Ranges) == 0 {
		return "ANCHORABLE DIFF LOCATIONS\nNo anchorable diff locations were parsed from the PR diff."
	}
	if limit <= 0 || limit > len(idx.Ranges) {
		limit = len(idx.Ranges)
	}
	lines := []string{"ANCHORABLE DIFF LOCATIONS", "Use these path/side/line ranges for inline review comments; downgrade anything outside the full PR diff's anchorable locations to a top-level comment with explicit location context."}
	for i := 0; i < limit; i++ {
		r := idx.Ranges[i]
		lineRange := fmt.Sprintf("%d", r.Start)
		if r.End != r.Start {
			lineRange = fmt.Sprintf("%d-%d", r.Start, r.End)
		}
		parts := []string{fmt.Sprintf("- %s %s lines %s", r.Path, r.Side, lineRange)}
		if r.Heading != "" {
			parts = append(parts, "heading: "+r.Heading)
		}
		if r.Excerpt != "" {
			parts = append(parts, "excerpt: "+r.Excerpt)
		}
		lines = append(lines, strings.Join(parts, " | "))
	}
	if limit < len(idx.Ranges) {
		lines = append(lines, fmt.Sprintf("- ... %d additional anchorable ranges omitted for brevity; the full PR diff remains authoritative for anchor validation", len(idx.Ranges)-limit))
	}
	return strings.Join(lines, "\n")
}

func (idx Index) Validate(anchor Anchor) ValidationResult {
	anchor.Side = normalizeSide(anchor.Side)
	anchor.StartSide = normalizeSide(anchor.StartSide)
	if anchor.Path == "" || anchor.Line <= 0 || anchor.Side == "" {
		return ValidationResult{Valid: false, Reason: "inline anchor is missing path, line, or side", LocationText: fallbackLocation(anchor), QualityFlagged: anchor.Path == "" && anchor.Line <= 0}
	}
	startLine := anchor.StartLine
	startSide := anchor.StartSide
	if startLine == 0 {
		startLine = anchor.Line
		startSide = anchor.Side
	}
	if startSide == "" {
		startSide = anchor.Side
	}
	if startSide != anchor.Side {
		return ValidationResult{Valid: false, Reason: "multiline anchors spanning LEFT and RIGHT sides are not supported", LocationText: fallbackLocation(anchor)}
	}
	if startLine > anchor.Line {
		return ValidationResult{Valid: false, Reason: "start_line must be less than or equal to line", LocationText: fallbackLocation(anchor)}
	}
	if idx.contains(anchor.Path, anchor.Side, startLine, anchor.Line) {
		return ValidationResult{Valid: true}
	}
	return ValidationResult{Valid: false, Reason: "inline anchor is outside the PR diff anchorable ranges", LocationText: fallbackLocation(anchor)}
}

func (idx Index) contains(path, side string, start, end int64) bool {
	for _, r := range idx.Ranges {
		if r.Path == path && r.Side == side && r.Start <= start && r.End >= end {
			return true
		}
	}
	return false
}

func ValidateTopLevelLocation(body string) ValidationResult {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || !hasExactTopLevelLocation(trimmed) {
		return ValidationResult{Valid: false, Reason: "top-level comment lacks exact file, section, symbol, or behavior location context", QualityFlagged: true}
	}
	return ValidationResult{Valid: true}
}

func hasExactTopLevelLocation(body string) bool {
	if topLevelFileLocationRe.MatchString(body) || topLevelHeadingLocationRe.MatchString(body) || topLevelLineLocationRe.MatchString(body) {
		return true
	}
	for _, match := range topLevelNamedLocationRe.FindAllStringSubmatch(body, -1) {
		if len(match) > 1 && isSpecificLocationName(match[1]) {
			return true
		}
	}
	return false
}

func isSpecificLocationName(name string) bool {
	name = strings.Trim(strings.TrimSpace(name), "`\"'.,:;()[]{}")
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	if genericTopLevelLocationName(lower) {
		return false
	}
	return regexp.MustCompile(`[A-Za-z0-9_]`).MatchString(name)
}

func genericTopLevelLocationName(name string) bool {
	switch name {
	case "a", "an", "the", "this", "that", "these", "those", "it", "its", "here", "there", "above", "below", "line", "lines", "section", "symbol", "function", "method", "type", "struct", "package", "code", "area", "part", "spot", "place", "needs", "need", "should", "work", "broken", "is", "are":
		return true
	default:
		return false
	}
}

func DowngradeBody(body string, anchor Anchor, reason string) string {
	location := fallbackLocation(anchor)
	if location == "" {
		location = "Location: unavailable; follow-up quality gate required."
	}
	parts := []string{location}
	if trimmed := strings.TrimSpace(body); trimmed != "" {
		parts = append(parts, trimmed)
	}
	parts = append(parts, "Downgraded from inline review comment: "+reason)
	return strings.Join(parts, "\n\n")
}

func fallbackLocation(anchor Anchor) string {
	parts := []string{}
	if anchor.Path != "" {
		parts = append(parts, anchor.Path)
	}
	if anchor.Side != "" && anchor.Line > 0 {
		if anchor.StartLine > 0 && anchor.StartLine != anchor.Line {
			parts = append(parts, fmt.Sprintf("%s lines %d-%d", normalizeSide(anchor.Side), anchor.StartLine, anchor.Line))
		} else {
			parts = append(parts, fmt.Sprintf("%s line %d", normalizeSide(anchor.Side), anchor.Line))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Location: " + strings.Join(parts, " ")
}

func gitDiffPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if rest == line {
		return ""
	}
	if strings.HasPrefix(rest, "\"") {
		parts := gitPathTokens(rest)
		if len(parts) >= 2 {
			return strings.TrimPrefix(parts[1], "b/")
		}
	}
	for searchFrom := 0; ; {
		idx := strings.Index(rest[searchFrom:], " b/")
		if idx < 0 {
			break
		}
		idx += searchFrom
		left := strings.TrimPrefix(rest[:idx], "a/")
		right := rest[idx+3:]
		if left == right {
			return unquoteGitPath(right)
		}
		searchFrom = idx + len(" b/")
	}
	if idx := strings.Index(rest, " b/"); idx >= 0 {
		return unquoteGitPath(rest[idx+3:])
	}
	return ""
}

func fileHeaderPath(path string) string {
	path = strings.TrimSuffix(path, "\r")
	if strings.HasPrefix(path, "\"") {
		parts := gitPathTokens(path)
		if len(parts) > 0 {
			path = parts[0]
		}
	} else if idx := strings.IndexByte(path, '\t'); idx >= 0 {
		path = path[:idx]
	} else {
		path = unquoteGitPath(path)
	}
	if path == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(path, "b/")
}

func gitPathTokens(s string) []string {
	var tokens []string
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		if s[0] != '"' {
			idx := strings.IndexByte(s, ' ')
			if idx < 0 {
				tokens = append(tokens, s)
				break
			}
			tokens = append(tokens, s[:idx])
			s = s[idx+1:]
			continue
		}
		end := 1
		escaped := false
		for end < len(s) {
			c := s[end]
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				end++
				break
			}
			end++
		}
		tokens = append(tokens, unquoteGitPath(s[:end]))
		s = s[end:]
	}
	return tokens
}

func unquoteGitPath(path string) string {
	if len(path) < 2 || path[0] != '"' || path[len(path)-1] != '"' {
		return path
	}
	unquoted, err := strconv.Unquote(path)
	if err != nil {
		return path
	}
	return unquoted
}

func parseInt64(value string) int64 {
	n, _ := strconv.ParseInt(value, 10, 64)
	return n
}

func headingFor(path, line string) string {
	if !isMarkdownLike(path) {
		return ""
	}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return trimmed
	}
	return ""
}

func isMarkdownLike(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".md" || ext == ".mdx" || ext == ".markdown" {
		return true
	}
	path = strings.ToLower(path)
	return strings.Contains(path, "/docs/") || strings.Contains(path, "/spec") || strings.HasPrefix(path, "docs/") || strings.HasPrefix(path, "spec")
}

func normalizeSide(side string) string {
	side = strings.ToUpper(strings.TrimSpace(side))
	if side == SideLeft || side == SideRight {
		return side
	}
	return ""
}

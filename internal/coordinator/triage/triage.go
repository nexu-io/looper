package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const jsISOStringLayout = "2006-01-02T15:04:05.000Z"

var tokenPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_./-]{2,}`)
var fencedJSONPattern = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")

type Disposition string

const (
	DispositionValid      Disposition = "valid"
	DispositionOutOfScope Disposition = "out-of-scope"
	DispositionUnclear    Disposition = "unclear"
)

type Comment struct {
	ID                int64
	Author            string
	AuthorAssociation string
	Body              string
	CreatedAt         string
	UpdatedAt         string
}

type TimelineEvent struct {
	Event     string
	CreatedAt string
	Label     string
}

type Issue struct {
	Number    int64
	Title     string
	Body      string
	URL       string
	Author    string
	CreatedAt string
	UpdatedAt string
	Labels    []string
	Comments  []Comment
	Timeline  []TimelineEvent
}

type RepoContext struct {
	Repo             string
	WorkingDirectory string
	Paths            []string
	Symbols          []string
}

type Config struct {
	TriagedLabel          string
	MaxIssueAgeDays       int
	MaxPerTick            int
	OutOfScopeLabel       string
	UnclearLabel          string
	ReTriageOnAuthorReply bool
}

type Input struct {
	Issue       Issue
	RepoContext RepoContext
	Config      Config
	Now         time.Time
}

type Request struct {
	Prompt           string
	WorkingDirectory string
}

type LLM interface {
	Complete(context.Context, Request) (string, error)
}

type Decision struct {
	NoOp               bool
	Disposition        Disposition
	ClearLabelPatterns []string
	RemoveLabels       []string
	ApplyLabels        []string
	CommentBody        string
	MarkTriaged        bool
}

type llmOutput struct {
	Disposition string `json:"disposition"`
	Comment     string `json:"comment"`
	Labels      struct {
		Kind       []string `json:"kind"`
		Area       []string `json:"area"`
		Complexity []string `json:"complexity"`
		Dispatch   []string `json:"dispatch"`
	} `json:"labels"`
}

func Decide(ctx context.Context, llm LLM, input Input) Decision {
	if llm == nil {
		return NoOpDecision()
	}
	raw, err := llm.Complete(ctx, Request{Prompt: BuildPrompt(input), WorkingDirectory: input.RepoContext.WorkingDirectory})
	if err != nil {
		return NoOpDecision()
	}
	decision, err := parseDecision(raw, input.Config)
	if err != nil {
		return NoOpDecision()
	}
	return decision
}

func NoOpDecision() Decision {
	return Decision{NoOp: true}
}

func ReTriageDecision(cfg Config) Decision {
	return Decision{Disposition: DispositionUnclear, RemoveLabels: []string{cfg.UnclearLabel, cfg.TriagedLabel}}
}

func ShouldTriage(issue Issue, cfg Config, now time.Time) bool {
	if hasLabel(issue.Labels, cfg.TriagedLabel) {
		return false
	}
	createdAt, ok := parseTime(issue.CreatedAt)
	if !ok {
		return false
	}
	return now.UTC().Sub(createdAt) <= time.Duration(cfg.MaxIssueAgeDays)*24*time.Hour
}

func ShouldReTriage(issue Issue, cfg Config, _ time.Time) bool {
	if !cfg.ReTriageOnAuthorReply || !hasLabel(issue.Labels, cfg.UnclearLabel) {
		return false
	}
	needsInfoAt, ok := needsInfoAppliedAt(issue, cfg.UnclearLabel)
	if !ok {
		return false
	}
	for _, comment := range issue.Comments {
		when, parsed := parseTime(comment.CreatedAt)
		if parsed && comment.Author == issue.Author && !when.Before(needsInfoAt) {
			return true
		}
	}
	return false
}

func LimitPerTick[T any](items []T, max int) []T {
	if max <= 0 || len(items) <= max {
		return append([]T(nil), items...)
	}
	return append([]T(nil), items[:max]...)
}

func BuildPrompt(input Input) string {
	var b strings.Builder
	b.WriteString("You are Looper Coordinator triage. Return strict JSON only.\n")
	b.WriteString("Allowed dispositions: valid, out-of-scope, unclear.\n")
	b.WriteString("Allowed kind labels: ")
	b.WriteString(strings.Join(AllowedKinds(), ", "))
	b.WriteString("\nAllowed area labels: ")
	b.WriteString(strings.Join(AllowedAreas(), ", "))
	b.WriteString("\nAllowed complexity labels: ")
	b.WriteString(strings.Join(AllowedComplexities(), ", "))
	b.WriteString("\nAllowed dispatch labels: ")
	b.WriteString(strings.Join(AllowedDispatches(), ", "))
	b.WriteString("\nOutput schema:\n")
	b.WriteString(`{"disposition":"valid|out-of-scope|unclear","comment":"string","labels":{"kind":["kind/..."],"area":["area/..."],"complexity":["complexity/..."],"dispatch":["dispatch/..."]}}`)
	b.WriteString("\n\nComment style:\n")
	b.WriteString("- Write the comment in a warm maintainer voice.\n")
	b.WriteString("- Briefly acknowledge the reporter and reference concrete details from the issue.\n")
	b.WriteString("- For valid issues, name the selected label/status path and give one clear next step.\n")
	b.WriteString("- For unclear issues, ask for the smallest missing detail needed to proceed.\n")
	b.WriteString("- For out-of-scope issues, explain the boundary respectfully.\n")
	b.WriteString("- Do not write generic bot boilerplate or repeat the entire issue.")
	b.WriteString("\n\nIssue:\n")
	if author := strings.TrimSpace(input.Issue.Author); author != "" {
		b.WriteString("Reporter: @")
		b.WriteString(strings.TrimPrefix(author, "@"))
		b.WriteString("\n")
	}
	b.WriteString(input.Issue.Title)
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(input.Issue.Body))
	if len(input.RepoContext.Paths) > 0 {
		b.WriteString("\n\nRelevant paths:\n- ")
		b.WriteString(strings.Join(input.RepoContext.Paths, "\n- "))
	}
	if len(input.RepoContext.Symbols) > 0 {
		b.WriteString("\n\nRelevant symbols:\n- ")
		b.WriteString(strings.Join(input.RepoContext.Symbols, "\n- "))
	}
	return b.String()
}

func SearchTokens(issue Issue) []string {
	text := issue.Title + "\n" + issue.Body
	matches := tokenPattern.FindAllString(text, -1)
	seen := map[string]struct{}{}
	tokens := make([]string, 0, len(matches))
	for _, match := range matches {
		match = strings.ToLower(strings.Trim(match, " ./-"))
		if len(match) < 3 {
			continue
		}
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		tokens = append(tokens, match)
		if len(tokens) == 12 {
			break
		}
	}
	return tokens
}

func AllowedKinds() []string {
	return []string{"kind/bug", "kind/feature", "kind/docs", "kind/refactor"}
}

func AllowedAreas() []string {
	return []string{"area/api", "area/config", "area/coordinator", "area/docs", "area/github", "area/runtime", "area/testing", "area/planner", "area/worker", "area/reviewer", "area/sweeper"}
}

func AllowedComplexities() []string {
	return []string{"complexity/s", "complexity/m", "complexity/l"}
}

func AllowedDispatches() []string {
	return []string{"dispatch/plan", "dispatch/implement"}
}

func parseDecision(raw string, cfg Config) (Decision, error) {
	var output llmOutput
	if err := json.Unmarshal([]byte(extractJSON(raw)), &output); err != nil {
		return Decision{}, err
	}
	comment := strings.TrimSpace(output.Comment)
	if comment == "" {
		return Decision{}, fmt.Errorf("comment is required")
	}
	clear := []string{"kind/*", "area/*", "complexity/*", "dispatch/*", cfg.OutOfScopeLabel, cfg.UnclearLabel}
	switch Disposition(strings.TrimSpace(output.Disposition)) {
	case DispositionValid:
		if len(output.Labels.Kind) == 0 && len(output.Labels.Area) == 0 && len(output.Labels.Complexity) == 0 {
			return Decision{}, fmt.Errorf("valid disposition requires at least one classification label")
		}
		kind, err := requireAllowedOrDefault(output.Labels.Kind, AllowedKinds(), "kind/feature")
		if err != nil {
			return Decision{}, err
		}
		area, err := requireAllowedOrDefault(output.Labels.Area, AllowedAreas(), defaultAreaForKind(kind))
		if err != nil {
			return Decision{}, err
		}
		complexity, err := requireAllowedOrDefault(output.Labels.Complexity, AllowedComplexities(), "complexity/m")
		if err != nil {
			return Decision{}, err
		}
		dispatch, err := requireAllowedOrDefault(output.Labels.Dispatch, AllowedDispatches(), "dispatch/plan")
		if err != nil {
			return Decision{}, err
		}
		return Decision{Disposition: DispositionValid, ClearLabelPatterns: clear, ApplyLabels: []string{kind, area, complexity, dispatch, cfg.TriagedLabel}, CommentBody: comment, MarkTriaged: true}, nil
	case DispositionOutOfScope:
		return Decision{Disposition: DispositionOutOfScope, ClearLabelPatterns: clear, ApplyLabels: []string{cfg.OutOfScopeLabel, cfg.TriagedLabel}, CommentBody: comment, MarkTriaged: true}, nil
	case DispositionUnclear:
		return Decision{Disposition: DispositionUnclear, ClearLabelPatterns: clear, ApplyLabels: []string{cfg.UnclearLabel, cfg.TriagedLabel}, CommentBody: comment, MarkTriaged: true}, nil
	default:
		return Decision{}, fmt.Errorf("unknown disposition")
	}
}

func requireAllowedOrDefault(values []string, allowed []string, fallback string) (string, error) {
	if len(values) == 0 {
		values = []string{fallback}
	}
	allowedSet := map[string]struct{}{}
	for _, item := range allowed {
		allowedSet[item] = struct{}{}
	}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if _, ok := allowedSet[value]; ok {
			return value, nil
		}
	}
	return "", fmt.Errorf("no allowed value")
}

func defaultAreaForKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case "kind/docs":
		return "area/docs"
	case "kind/feature":
		return "area/planner"
	case "kind/bug":
		return "area/runtime"
	default:
		return "area/runtime"
	}
}

func extractJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if matches := fencedJSONPattern.FindStringSubmatch(trimmed); len(matches) == 2 {
		trimmed = strings.TrimSpace(matches[1])
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		return strings.TrimSpace(trimmed[start : end+1])
	}
	return trimmed
}

func requireExactlyOne(values []string, allowed []string) (string, error) {
	if len(values) != 1 {
		return "", fmt.Errorf("expected exactly one value")
	}
	value := strings.TrimSpace(values[0])
	allowedSet := map[string]struct{}{}
	for _, item := range allowed {
		allowedSet[item] = struct{}{}
	}
	if _, ok := allowedSet[value]; !ok {
		return "", fmt.Errorf("unknown value %q", value)
	}
	return value, nil
}

func needsInfoAppliedAt(issue Issue, unclearLabel string) (time.Time, bool) {
	for index := len(issue.Timeline) - 1; index >= 0; index-- {
		event := issue.Timeline[index]
		if event.Event != "labeled" || event.Label != unclearLabel {
			continue
		}
		return parseTime(event.CreatedAt)
	}
	return time.Time{}, false
}

func parseTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, jsISOStringLayout} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func AllowedLabelUniverse() []string {
	labels := append([]string{}, AllowedKinds()...)
	labels = append(labels, AllowedAreas()...)
	labels = append(labels, AllowedComplexities()...)
	labels = append(labels, AllowedDispatches()...)
	sort.Strings(labels)
	return labels
}

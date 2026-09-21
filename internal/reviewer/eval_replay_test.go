package reviewer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/specpr"
)

// Prompt-construction replay for the reviewer eval kit. This is not model-quality
// CI: a clean labels.json (expectedFindings empty) is "no findings", not an
// execution failure. Crashes, timeouts, and missing completion markers would be
// execution failures and must be counted separately when a model is later run.
//
// Live model replay is gated on LOOPER_EVAL_AGENT=1 and is not implemented here.
// Unset env must not start an agent or invent token counts.

type evalIndexFile struct {
	Version int              `json:"version"`
	Samples []evalIndexEntry `json:"samples"`
}

type evalIndexEntry struct {
	ID         string   `json:"id"`
	Categories []string `json:"categories"`
}

type evalSampleMeta struct {
	ID                   string              `json:"id"`
	SourceRepo           string              `json:"sourceRepo"`
	PRNumber             int64               `json:"prNumber"`
	BaseSHA              string              `json:"baseSHA"`
	HeadSHA              string              `json:"headSHA"`
	MergeBase            string              `json:"mergeBase"`
	PassKind             string              `json:"passKind"`
	Scope                string              `json:"scope"`
	ReviewKind           string              `json:"reviewKind"`
	LastPublishedHeadSha string              `json:"lastPublishedHeadSha"`
	FixturePath          string              `json:"fixturePath"`
	RequiredHistory      evalRequiredHistory `json:"requiredHistory"`
}

type evalRequiredHistory struct {
	Notes                    string             `json:"notes"`
	UnresolvedMustFixThreads []evalFrozenThread `json:"unresolvedMustFixThreads"`
	FixerNotes               string             `json:"fixerNotes"`
}

type evalFrozenThread struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Body string `json:"body"`
}

type evalLabelsFile struct {
	ExpectedFindings []evalFinding `json:"expectedFindings"`
}

type evalFinding struct {
	ID               string `json:"id"`
	Path             string `json:"path"`
	Line             int    `json:"line"`
	Title            string `json:"title"`
	Body             string `json:"body"`
	TriggerCondition string `json:"triggerCondition"`
	CorrectnessBasis string `json:"correctnessBasis"`
	RootCauseKey     string `json:"rootCauseKey"`
}

func evalRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		index := filepath.Join(dir, "evals", "reviewer", "samples", "index.json")
		if _, err := os.Stat(index); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find evals/reviewer/samples/index.json walking up from this test file")
		}
		dir = parent
	}
}

func loadEvalIndex(t *testing.T) (string, evalIndexFile) {
	t.Helper()
	root := evalRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "evals", "reviewer", "samples", "index.json"))
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	var index evalIndexFile
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("parse index.json: %v", err)
	}
	return root, index
}

func loadEvalSample(t *testing.T, root, id string) (evalSampleMeta, evalLabelsFile, []byte) {
	t.Helper()
	dir := filepath.Join(root, "evals", "reviewer", "samples", id)
	metaRaw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatalf("sample %s meta.json: %v", id, err)
	}
	labelRaw, err := os.ReadFile(filepath.Join(dir, "labels.json"))
	if err != nil {
		t.Fatalf("sample %s labels.json: %v", id, err)
	}
	var meta evalSampleMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("sample %s meta.json parse: %v", id, err)
	}
	labels, err := parseEvalLabels(labelRaw)
	if err != nil {
		t.Fatalf("sample %s labels.json: %v", id, err)
	}
	return meta, labels, labelRaw
}

func parseEvalLabels(labelRaw []byte) (evalLabelsFile, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(labelRaw, &raw); err != nil {
		return evalLabelsFile{}, err
	}
	findingsRaw, ok := raw["expectedFindings"]
	if !ok {
		return evalLabelsFile{}, fmt.Errorf("missing expectedFindings")
	}
	if strings.TrimSpace(string(findingsRaw)) == "null" {
		return evalLabelsFile{}, fmt.Errorf("expectedFindings is null; use [] for a clean label")
	}
	var labels evalLabelsFile
	if err := json.Unmarshal(labelRaw, &labels); err != nil {
		return evalLabelsFile{}, err
	}
	for i, finding := range labels.ExpectedFindings {
		if strings.TrimSpace(finding.RootCauseKey) == "" {
			return evalLabelsFile{}, fmt.Errorf("expectedFindings[%d] missing rootCauseKey", i)
		}
		if strings.TrimSpace(finding.Path) == "" {
			return evalLabelsFile{}, fmt.Errorf("expectedFindings[%d] missing path", i)
		}
		if finding.Line <= 0 {
			return evalLabelsFile{}, fmt.Errorf("expectedFindings[%d] missing line", i)
		}
	}
	return labels, nil
}

func TestEvalSampleIndexValid(t *testing.T) {
	root, index := loadEvalIndex(t)
	n := len(index.Samples)
	if n < 8 || n > 12 {
		t.Fatalf("index must list 8–12 samples, got %d", n)
	}

	seen := map[string]bool{}
	categories := map[string]bool{}
	repairCount := 0
	for _, entry := range index.Samples {
		if strings.TrimSpace(entry.ID) == "" {
			t.Fatal("index sample missing id")
		}
		if seen[entry.ID] {
			t.Fatalf("duplicate sample id %q", entry.ID)
		}
		seen[entry.ID] = true
		for _, cat := range entry.Categories {
			categories[cat] = true
		}

		meta, _, _ := loadEvalSample(t, root, entry.ID)
		if meta.ID != entry.ID {
			t.Fatalf("sample %s meta.id %q does not match index", entry.ID, meta.ID)
		}
		if strings.TrimSpace(meta.HeadSHA) == "" {
			t.Fatalf("sample %s missing headSHA", entry.ID)
		}
		if meta.PassKind != "first_pass" && meta.PassKind != "repair_frontier" {
			t.Fatalf("sample %s passKind %q", entry.ID, meta.PassKind)
		}
		if meta.ReviewKind != "implementation" && meta.ReviewKind != "spec" {
			t.Fatalf("sample %s reviewKind %q", entry.ID, meta.ReviewKind)
		}
		if meta.Scope == "" {
			meta.Scope = "changed_ranges"
		}
		if meta.Scope != "changed_ranges" && meta.Scope != "full_pr" && meta.Scope != "changed_files" {
			t.Fatalf("sample %s scope %q", entry.ID, meta.Scope)
		}
		if meta.PassKind == "repair_frontier" {
			repairCount++
			if strings.TrimSpace(meta.LastPublishedHeadSha) == "" || meta.LastPublishedHeadSha == meta.HeadSHA {
				t.Fatalf("repair sample %s needs lastPublishedHeadSha distinct from headSHA", entry.ID)
			}
			if strings.TrimSpace(meta.RequiredHistory.Notes) == "" {
				t.Fatalf("repair sample %s needs non-empty requiredHistory.notes", entry.ID)
			}
		}
		if fp := strings.TrimSpace(meta.FixturePath); fp != "" {
			assertEvalFixtureProposedDiff(t, root, entry.ID, fp)
		}
	}

	required := []string{
		"concurrency_resource",
		"cross_module_contract",
		"large_diff",
		"test_only",
		"docs_only",
		"repair_frontier",
		"spec_review",
		"implementation_review",
		"p0_p1_regression",
	}
	for _, cat := range required {
		if !categories[cat] {
			t.Errorf("required category %q missing from index", cat)
		}
	}
	if repairCount < 1 {
		t.Fatal("index needs at least one repair_frontier sample")
	}
}

func TestEvalLabelsRejectMalformed(t *testing.T) {
	t.Parallel()
	validFinding := `{
		"id": "F1",
		"path": "pkg/file.go",
		"line": 12,
		"title": "title",
		"body": "body",
		"rootCauseKey": "shared-root"
	}`
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "clean_empty_array", raw: `{"expectedFindings":[]}`},
		{name: "valid_finding", raw: `{"expectedFindings":[` + validFinding + `]}`},
		{name: "missing_key", raw: `{}`, wantErr: "missing expectedFindings"},
		{name: "misspelled_key", raw: `{"expected_findings":[]}`, wantErr: "missing expectedFindings"},
		{name: "null_findings", raw: `{"expectedFindings":null}`, wantErr: "expectedFindings is null"},
		{name: "missing_root_cause", raw: `{"expectedFindings":[{"id":"F1","path":"pkg/file.go","line":12,"rootCauseKey":""}]}`, wantErr: "missing rootCauseKey"},
		{name: "missing_path", raw: `{"expectedFindings":[{"id":"F1","path":"","line":12,"rootCauseKey":"shared-root"}]}`, wantErr: "missing path"},
		{name: "missing_line", raw: `{"expectedFindings":[{"id":"F1","path":"pkg/file.go","rootCauseKey":"shared-root"}]}`, wantErr: "missing line"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseEvalLabels([]byte(tc.raw))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parseEvalLabels() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parseEvalLabels() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func assertEvalFixtureProposedDiff(t *testing.T, root, id, fixturePath string) {
	t.Helper()
	dir := filepath.Join(root, fixturePath)
	diffPath := filepath.Join(dir, "proposed.diff")
	absDiff, err := filepath.Abs(diffPath)
	if err != nil {
		t.Fatalf("sample %s proposed.diff abs: %v", id, err)
	}
	numstat := exec.Command("git", "apply", "--numstat", absDiff)
	if out, err := numstat.CombinedOutput(); err != nil {
		t.Fatalf("sample %s proposed.diff is not a valid unified diff: %v\n%s", id, err, out)
	}

	tmp := t.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("sample %s fixture dir: %v", id, err)
	}
	copied := 0
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "proposed.diff" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("sample %s read %s: %v", id, entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(tmp, entry.Name()), data, 0o644); err != nil {
			t.Fatalf("sample %s copy %s: %v", id, entry.Name(), err)
		}
		copied++
	}
	if copied == 0 {
		t.Fatalf("sample %s fixture has no after-image files to apply against", id)
	}
	reverse := exec.Command("git", "apply", "--reverse", absDiff)
	reverse.Dir = tmp
	if out, err := reverse.CombinedOutput(); err != nil {
		t.Fatalf("sample %s proposed.diff after-image does not match fixture files: %v\n%s", id, err, out)
	}
}

func TestEvalReplayPromptConstruction(t *testing.T) {
	root, index := loadEvalIndex(t)

	var firstPass, repair *evalIndexEntry
	for i := range index.Samples {
		entry := index.Samples[i]
		meta, _, _ := loadEvalSample(t, root, entry.ID)
		switch meta.PassKind {
		case "first_pass":
			if firstPass == nil {
				firstPass = &index.Samples[i]
			}
		case "repair_frontier":
			if repair == nil {
				repair = &index.Samples[i]
			}
		}
	}
	if firstPass == nil || repair == nil {
		t.Fatal("index must include at least one first_pass and one repair_frontier sample")
	}

	t.Run("first_pass", func(t *testing.T) {
		meta, labels, labelRaw := loadEvalSample(t, root, firstPass.ID)
		prompt := replayEvalPrompt(t, meta)
		if !strings.Contains(prompt, "Review pass contract: complete one full review pass") {
			t.Fatalf("first-pass prompt missing full-pass contract:\n%s", prompt)
		}
		if strings.Contains(prompt, "Repair frontier contract (later pass)") {
			t.Fatalf("first-pass prompt leaked repair-frontier contract:\n%s", prompt)
		}
		assertLabelsStayOutOfPrompt(t, prompt, labels, labelRaw)
	})

	t.Run("repair_frontier", func(t *testing.T) {
		meta, labels, labelRaw := loadEvalSample(t, root, repair.ID)
		prompt := replayEvalPrompt(t, meta)
		if !strings.Contains(prompt, "Repair frontier contract (later pass)") {
			t.Fatalf("repair-frontier prompt missing later-pass contract:\n%s", prompt)
		}
		if !strings.Contains(prompt, "Last reviewed head SHA") {
			t.Fatalf("repair-frontier prompt missing last reviewed head SHA:\n%s", prompt)
		}
		if strings.Contains(prompt, "Review pass contract: complete one full review pass") &&
			!strings.Contains(prompt, "Review pass contract (later pass / repair frontier)") {
			t.Fatalf("repair-frontier prompt used first-pass contract:\n%s", prompt)
		}
		assertLabelsStayOutOfPrompt(t, prompt, labels, labelRaw)
	})

	t.Run("frozen_sample_context", func(t *testing.T) {
		for _, entry := range index.Samples {
			meta, labels, labelRaw := loadEvalSample(t, root, entry.ID)
			prompt := replayEvalPrompt(t, meta)
			assertEvalFrozenSampleContext(t, meta, prompt)
			assertLabelsStayOutOfPrompt(t, prompt, labels, labelRaw)
		}
	})
}

func replayEvalPrompt(t *testing.T, meta evalSampleMeta) string {
	t.Helper()
	prNumber := meta.PRNumber
	if prNumber <= 0 {
		prNumber = 1
	}
	repo := strings.TrimSpace(meta.SourceRepo)
	if repo == "" {
		repo = "nexu-io/looper"
	}
	scope := config.ReviewerScopeChangedRanges
	switch meta.Scope {
	case "full_pr":
		scope = config.ReviewerScopeFullPR
	case "changed_files":
		scope = config.ReviewerScopeChangedFiles
	}
	detail := &checkpointDetail{
		HeadSHA: meta.HeadSHA,
		BaseSHA: meta.BaseSHA,
		State:   "OPEN",
	}
	if meta.ReviewKind == "spec" {
		detail.Labels = []string{specpr.ReviewingLabel}
	}
	prompt, _ := buildReviewPromptWithInstructions(
		"eval-project",
		config.Config{},
		repo,
		prNumber,
		reviewerCheckpoint{
			Detail:   detail,
			Snapshot: &checkpointSnapshot{HeadSHA: meta.HeadSHA, BaseSHA: meta.BaseSHA},
		},
		"eval-run",
		"reviewer:eval:"+meta.HeadSHA,
		config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment},
		false,
		true,
		"",
		scope,
		config.DefaultDisclosureConfig(),
		"opencode",
		"",
		"/opt/looper/bin/looper",
		false,
		false,
		meta.LastPublishedHeadSha,
	)
	return insertBeforeCompletionInstruction(applyEvalOfflinePromptContract(t, prompt), evalFrozenHistoryPrompt(meta.RequiredHistory))
}

func assertEvalFrozenSampleContext(t *testing.T, meta evalSampleMeta, prompt string) {
	t.Helper()
	if base := strings.TrimSpace(meta.BaseSHA); base != "" && !strings.Contains(prompt, base) {
		t.Fatalf("sample %s prompt missing frozen baseSHA %s", meta.ID, base)
	}
	switch meta.ReviewKind {
	case "spec":
		if !strings.Contains(prompt, "This is a spec review") {
			t.Fatalf("sample %s prompt missing spec review phase:\n%s", meta.ID, prompt)
		}
		if strings.Contains(prompt, "This is an implementation review") {
			t.Fatalf("sample %s prompt used implementation review phase:\n%s", meta.ID, prompt)
		}
	case "implementation":
		if !strings.Contains(prompt, "This is an implementation review") {
			t.Fatalf("sample %s prompt missing implementation review phase:\n%s", meta.ID, prompt)
		}
		if strings.Contains(prompt, "This is a spec review") {
			t.Fatalf("sample %s prompt used spec review phase:\n%s", meta.ID, prompt)
		}
	}
	history := meta.RequiredHistory
	if notes := strings.TrimSpace(history.Notes); notes != "" && !strings.Contains(prompt, notes) {
		t.Fatalf("sample %s prompt missing requiredHistory.notes", meta.ID)
	}
	if fixer := strings.TrimSpace(history.FixerNotes); fixer != "" && !strings.Contains(prompt, fixer) {
		t.Fatalf("sample %s prompt missing requiredHistory.fixerNotes", meta.ID)
	}
	for _, thread := range history.UnresolvedMustFixThreads {
		if id := strings.TrimSpace(thread.ID); id != "" && !strings.Contains(prompt, id) {
			t.Fatalf("sample %s prompt missing frozen thread %s", meta.ID, id)
		}
		if body := strings.TrimSpace(thread.Body); body != "" && !strings.Contains(prompt, body) {
			t.Fatalf("sample %s prompt missing frozen thread body for %s", meta.ID, thread.ID)
		}
	}
	assertEvalPromptOmitsLiveGitHub(t, meta, prompt)
}

func evalFrozenHistoryPrompt(history evalRequiredHistory) string {
	notes := strings.TrimSpace(history.Notes)
	fixer := strings.TrimSpace(history.FixerNotes)
	if notes == "" && fixer == "" && len(history.UnresolvedMustFixThreads) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Frozen sample review history. Do not fetch live review threads; use this recorded evidence instead.")
	if notes != "" {
		b.WriteString("\n\n")
		b.WriteString(notes)
	}
	if len(history.UnresolvedMustFixThreads) > 0 {
		b.WriteString("\n\nUnresolved must_fix threads:")
		for _, thread := range history.UnresolvedMustFixThreads {
			b.WriteString("\n- id=")
			b.WriteString(thread.ID)
			if path := strings.TrimSpace(thread.Path); path != "" {
				b.WriteString(" path=")
				b.WriteString(path)
			}
			if body := strings.TrimSpace(thread.Body); body != "" {
				b.WriteString("\n  ")
				b.WriteString(body)
			}
		}
	}
	if fixer != "" {
		b.WriteString("\n\nFixer notes:\n")
		b.WriteString(fixer)
	}
	return b.String()
}

func evalOfflineFetchContract() string {
	return strings.Join([]string{
		"Offline eval sample contract: this prompt is a frozen fixture replay. Use only the minimal PR seed, requiredHistory, local checkout, and committed fixture diff in this prompt.",
		"Do not query live GitHub metadata, checks, review threads, or comments for this sample.",
		"Do not validate live head/base SHAs, PR state, or draft status, and do not fail fast on PR drift. These samples include historical OPEN PRs, synthetic PR numbers, and nonexistent SHAs; live validation would introduce mutable evidence or terminate with pr_drift instead of reviewing the frozen sample.",
		"Inspect changes with local Git using the fixed base_sha and head_sha from the seed, or the committed fixture patch when a fixturePath is provided. Never treat a missing local object as permission to fetch the live PR.",
	}, "\n")
}

func applyEvalOfflinePromptContract(t *testing.T, prompt string) string {
	t.Helper()
	live := reviewerAgentSideGitHubFetchContract()
	if !strings.Contains(prompt, live) {
		t.Fatal("eval replay prompt missing production GitHub fetch contract to replace")
	}
	prompt = strings.Replace(prompt, live, evalOfflineFetchContract(), 1)
	replacements := [][2]string{
		{
			"Idempotency requirement: before posting anything, use `gh api` to list existing PR reviews for this PR. Only treat an existing marker as satisfying this run when the review body contains the exact idempotency id and expected head SHA, and the review state matches the required outcome-specific policy for this run. If such a matching review already exists, do not post another review. Instead, rely on Looper to validate that marker after the agent exits and to reconcile clean-signal reactions/spec label transitions as needed. If the marker exists but the outcome/review-state combination does not satisfy this run, ignore it and publish the correct review for this run instead.",
			"Idempotency requirement: do not list live PR reviews or inspect live review markers. Frozen eval samples are offline fixtures.",
		},
		{
			"Before posting, use `gh` to confirm the PR is still open and the head SHA still matches the expected head SHA. If it changed, do not post a review and exit non-zero with the exact message `PR head changed before publish`.",
			"Do not confirm live PR state or head SHA before posting. Frozen eval samples include historical OPEN PRs and synthetic SHAs; do not exit for PR drift.",
		},
		{
			"Before posting, confirm the current GitHub user is still requested for review. If not requested, do not post a review; exit non-zero with the exact message `review request removed before publish`.",
			"Do not confirm a live GitHub review request. Frozen eval samples are not live review assignments.",
		},
	}
	for _, pair := range replacements {
		if !strings.Contains(prompt, pair[0]) {
			t.Fatalf("eval replay prompt missing live GitHub instruction to replace: %q", pair[0])
		}
		prompt = strings.Replace(prompt, pair[0], pair[1], 1)
	}
	return prompt
}

func assertEvalPromptOmitsLiveGitHub(t *testing.T, meta evalSampleMeta, prompt string) {
	t.Helper()
	if !strings.Contains(prompt, "Offline eval sample contract") {
		t.Fatalf("sample %s prompt missing offline eval contract", meta.ID)
	}
	if strings.Contains(prompt, reviewerAgentSideGitHubFetchContract()) {
		t.Fatalf("sample %s prompt retained production GitHub fetch contract", meta.ID)
	}
	for _, forbidden := range []string{
		"gh pr view <pr-url>",
		"gh pr checks <pr-url>",
		"gh api repos/{owner}/{repo}/pulls/{number}/comments --paginate",
		"Fail fast on drift",
		"Do not proceed on stale PR data",
		"use `gh api` to list existing PR reviews",
		"use `gh` to confirm the PR is still open",
		"review request removed before publish",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("sample %s prompt still contains live GitHub instruction %q", meta.ID, forbidden)
		}
	}
}

func insertBeforeCompletionInstruction(prompt, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return prompt
	}
	const marker = "When finished, print exactly one final line to stdout in this format:"
	if idx := strings.LastIndex(prompt, marker); idx >= 0 {
		return strings.TrimRight(prompt[:idx], "\n") + "\n\n" + extra + "\n\n" + prompt[idx:]
	}
	return prompt + "\n\n" + extra
}

func assertLabelsStayOutOfPrompt(t *testing.T, prompt string, labels evalLabelsFile, labelRaw []byte) {
	t.Helper()
	for _, finding := range labels.ExpectedFindings {
		for _, needle := range []string{finding.Title, finding.Body, finding.TriggerCondition, finding.CorrectnessBasis, finding.RootCauseKey} {
			needle = strings.TrimSpace(needle)
			if len(needle) < 12 {
				continue
			}
			if strings.Contains(prompt, needle) {
				t.Fatalf("labels.json leaked into prompt: %q", needle)
			}
		}
	}
	var tree any
	if err := json.Unmarshal(labelRaw, &tree); err != nil {
		t.Fatalf("labels.json reparse: %v", err)
	}
	var needles []string
	collectJSONStrings(tree, &needles)
	for _, needle := range needles {
		if strings.Contains(prompt, needle) {
			t.Fatalf("labels.json contents leaked into prompt: %q", needle)
		}
	}
}

func collectJSONStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		if len(strings.TrimSpace(t)) >= 16 {
			*out = append(*out, t)
		}
	case []any:
		for _, item := range t {
			collectJSONStrings(item, out)
		}
	case map[string]any:
		for _, item := range t {
			collectJSONStrings(item, out)
		}
	}
}

package reviewer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
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
	LastPublishedHeadSha string              `json:"lastPublishedHeadSha"`
	FixturePath          string              `json:"fixturePath"`
	RequiredHistory      evalRequiredHistory `json:"requiredHistory"`
}

type evalRequiredHistory struct {
	Notes                    string `json:"notes"`
	UnresolvedMustFixThreads []any  `json:"unresolvedMustFixThreads"`
	FixerNotes               string `json:"fixerNotes"`
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
	var labels evalLabelsFile
	if err := json.Unmarshal(labelRaw, &labels); err != nil {
		t.Fatalf("sample %s labels.json parse: %v", id, err)
	}
	return meta, labels, labelRaw
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
	prompt, _ := buildReviewPromptWithInstructions(
		"eval-project",
		config.Config{},
		repo,
		prNumber,
		reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: meta.HeadSHA}},
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
	return prompt
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

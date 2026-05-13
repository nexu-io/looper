package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	envFakeGHMode        = "LOOPER_E2E_FAKE_GH_MODE"
	envFakeGHArtifactDir = "LOOPER_E2E_FAKE_GH_ARTIFACT_DIR"
	envFakeGHSchemaPath  = "LOOPER_E2E_FAKE_GH_SCHEMA_PATH"
	envFakeGHStatePath   = "LOOPER_E2E_FAKE_GH_STATE_PATH"
	envFakeGHRecordPath  = "LOOPER_E2E_FAKE_GH_RECORD_PATH"
	envFakeGHGitPath     = "LOOPER_E2E_FAKE_GH_GIT_PATH"
)

type GHSchema struct {
	JSONFieldAllowlist map[string][]string `json:"jsonFieldAllowlist"`
}

type FakeGH struct {
	Path          string
	Mode          string
	GitPath       string
	ArtifactDir   string
	SchemaPath    string
	StatePath     string
	RecordPath    string
	InvocationLog string
}

type GHThreadComment struct {
	ID                string `json:"id"`
	Body              string `json:"body,omitempty"`
	Author            string `json:"author,omitempty"`
	CreatedAt         string `json:"createdAt,omitempty"`
	UpdatedAt         string `json:"updatedAt,omitempty"`
	Path              string `json:"path,omitempty"`
	Line              int64  `json:"line,omitempty"`
	OriginalCommitOID string `json:"originalCommitOid,omitempty"`
	CommitOID         string `json:"commitOid,omitempty"`
	URL               string `json:"url,omitempty"`
}

type GHThread struct {
	ID         string            `json:"id"`
	IsResolved bool              `json:"isResolved,omitempty"`
	Path       string            `json:"path,omitempty"`
	Line       int64             `json:"line,omitempty"`
	Comments   []GHThreadComment `json:"comments,omitempty"`
}

type GHPullRequest struct {
	Number            int64            `json:"number"`
	Repo              string           `json:"repo,omitempty"`
	Title             string           `json:"title,omitempty"`
	Body              string           `json:"body,omitempty"`
	URL               string           `json:"url,omitempty"`
	State             string           `json:"state,omitempty"`
	UpdatedAt         string           `json:"updatedAt,omitempty"`
	IsDraft           bool             `json:"isDraft,omitempty"`
	ReviewDecision    string           `json:"reviewDecision,omitempty"`
	Labels            []string         `json:"labels,omitempty"`
	HeadRefName       string           `json:"headRefName,omitempty"`
	BaseRefName       string           `json:"baseRefName,omitempty"`
	HeadRef           string           `json:"headRef,omitempty"`
	BaseRef           string           `json:"baseRef,omitempty"`
	HeadSHA           string           `json:"headSha,omitempty"`
	BaseSHA           string           `json:"baseSha,omitempty"`
	GitDir            string           `json:"gitDir,omitempty"`
	Author            string           `json:"author,omitempty"`
	AuthorAssociation string           `json:"authorAssociation,omitempty"`
	ReviewRequests    []string         `json:"reviewRequests,omitempty"`
	IssueComments     []map[string]any `json:"issueComments,omitempty"`
	Reviews           []map[string]any `json:"reviews,omitempty"`
	StatusCheckRollup []map[string]any `json:"statusCheckRollup,omitempty"`
	MergeStateStatus  string           `json:"mergeStateStatus,omitempty"`
	Threads           []GHThread       `json:"threads,omitempty"`
}

type GHState struct {
	Commands         map[string]any           `json:"commands,omitempty"`
	Routes           map[string]any           `json:"routes,omitempty"`
	GraphQL          map[string]any           `json:"graphql,omitempty"`
	CurrentUserLogin string                   `json:"currentUserLogin,omitempty"`
	PullRequests     map[string]GHPullRequest `json:"pullRequests,omitempty"`
}

func NewFakeGH(tb testing.TB, bins BuiltBinaries, schema GHSchema) FakeGH {
	tb.Helper()
	root := filepath.Join(artifactTempDir(tb, "fake-gh"), "fake-gh")
	if err := os.MkdirAll(root, 0o755); err != nil {
		tb.Fatalf("mkdir fake gh root: %v", err)
	}
	schemaPath := filepath.Join(root, "schema.json")
	payload, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		tb.Fatalf("marshal fake gh schema: %v", err)
	}
	if err := os.WriteFile(schemaPath, payload, 0o644); err != nil {
		tb.Fatalf("write fake gh schema: %v", err)
	}
	return FakeGH{Path: bins.FakeGHPath, Mode: "strict", GitPath: "git", ArtifactDir: root, SchemaPath: schemaPath, StatePath: filepath.Join(root, "state.json"), RecordPath: filepath.Join(root, "record.jsonl"), InvocationLog: filepath.Join(root, "invocations.jsonl")}
}

func (f FakeGH) EnvMap() map[string]string {
	mode := f.Mode
	if mode == "" {
		mode = "strict"
	}
	return map[string]string{
		envFakeGHMode:        mode,
		envFakeGHGitPath:     firstNonEmpty(f.GitPath, "git"),
		envFakeGHArtifactDir: f.ArtifactDir,
		envFakeGHSchemaPath:  f.SchemaPath,
		envFakeGHStatePath:   f.StatePath,
		envFakeGHRecordPath:  f.RecordPath,
	}
}

func (f FakeGH) WriteState(tb testing.TB, state GHState) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(f.StatePath), 0o755); err != nil {
		tb.Fatalf("mkdir fake gh state dir: %v", err)
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		tb.Fatalf("marshal fake gh state: %v", err)
	}
	if err := os.WriteFile(f.StatePath, payload, 0o644); err != nil {
		tb.Fatalf("write fake gh state: %v", err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

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
)

type GHSchema struct {
	JSONFieldAllowlist map[string][]string `json:"jsonFieldAllowlist"`
}

type FakeGH struct {
	Path          string
	Mode          string
	ArtifactDir   string
	SchemaPath    string
	StatePath     string
	RecordPath    string
	InvocationLog string
}

func NewFakeGH(tb testing.TB, bins BuiltBinaries, schema GHSchema) FakeGH {
	tb.Helper()
	root := filepath.Join(tb.TempDir(), "fake-gh")
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
	return FakeGH{
		Path:          bins.FakeGHPath,
		Mode:          "strict",
		ArtifactDir:   root,
		SchemaPath:    schemaPath,
		StatePath:     filepath.Join(root, "state.json"),
		RecordPath:    filepath.Join(root, "record.jsonl"),
		InvocationLog: filepath.Join(root, "invocations.jsonl"),
	}
}

func (f FakeGH) EnvMap() map[string]string {
	mode := f.Mode
	if mode == "" {
		mode = "strict"
	}
	return map[string]string{
		envFakeGHMode:        mode,
		envFakeGHArtifactDir: f.ArtifactDir,
		envFakeGHSchemaPath:  f.SchemaPath,
		envFakeGHStatePath:   f.StatePath,
		envFakeGHRecordPath:  f.RecordPath,
	}
}

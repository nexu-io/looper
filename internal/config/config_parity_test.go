package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type configParityFixture struct {
	Description string `json:"description"`
	Input       struct {
		OptionConfigPath  string            `json:"optionConfigPath"`
		DefaultConfigPath string            `json:"defaultConfigPath"`
		Args              []string          `json:"argv"`
		Env               map[string]string `json:"env"`
		Files             map[string]any    `json:"files"`
	} `json:"input"`
	Expected struct {
		Config   map[string]any `json:"config"`
		Metadata map[string]any `json:"metadata"`
	} `json:"expected"`
}

func TestLoadFileMatchesFrozenParityFixtures(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join("testdata", "parity"))
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := filepath.Join("testdata", "parity", entry.Name())
		fixture := readConfigParityFixture(t, path)

		t.Run(strings.TrimSuffix(entry.Name(), ".json"), func(t *testing.T) {
			rootDir := t.TempDir()
			writeParityFiles(t, rootDir, homeDir, fixture.Input.Files)

			loaded, err := LoadFile(LoadFileOptions{
				CWD:               rootDir,
				ConfigPath:        resolveParityString(fixture.Input.OptionConfigPath, rootDir, homeDir),
				DefaultConfigPath: resolveParityString(fixture.Input.DefaultConfigPath, rootDir, homeDir),
				Args:              resolveParityStrings(fixture.Input.Args, rootDir, homeDir),
				LookupEnv:         mapEnvLookup(resolveParityStringMap(fixture.Input.Env, rootDir, homeDir)),
			})
			if err != nil {
				t.Fatalf("LoadFile() error = %v", err)
			}

			actual := parityPayloadFromLoadedFileConfig(t, loaded)
			expected := map[string]any{
				"config":   resolveParityValue(fixture.Expected.Config, rootDir, homeDir),
				"metadata": resolveParityValue(fixture.Expected.Metadata, rootDir, homeDir),
			}

			if !reflect.DeepEqual(actual, expected) {
				actualJSON := mustMarshalJSON(t, actual)
				expectedJSON := mustMarshalJSON(t, expected)
				t.Fatalf("parity mismatch\nactual:\n%s\nexpected:\n%s", actualJSON, expectedJSON)
			}
		})
	}
}

func readConfigParityFixture(t *testing.T, path string) configParityFixture {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}

	var fixture configParityFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", path, err)
	}

	return fixture
}

func writeParityFiles(t *testing.T, rootDir string, homeDir string, files map[string]any) {
	t.Helper()

	for relativePath, contents := range files {
		targetPath := resolveParityString(relativePath, rootDir, homeDir)
		if !filepath.IsAbs(targetPath) {
			targetPath = filepath.Join(rootDir, targetPath)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%q) error = %v", filepath.Dir(targetPath), err)
		}

		resolvedContents := resolveParityValue(contents, rootDir, homeDir)
		raw, err := json.MarshalIndent(resolvedContents, "", "  ")
		if err != nil {
			t.Fatalf("json.MarshalIndent(%q) error = %v", targetPath, err)
		}

		if err := os.WriteFile(targetPath, raw, 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", targetPath, err)
		}
	}
}

func parityPayloadFromLoadedFileConfig(t *testing.T, loaded LoadedFileConfig) map[string]any {
	t.Helper()

	configValue := toJSONValue(t, loaded.Config).(map[string]any)
	if daemonValue, ok := configValue["daemon"].(map[string]any); ok {
		delete(daemonValue, "shutdownTimeoutMs")
	}

	return map[string]any{
		"config": configValue,
		"metadata": map[string]any{
			"configPath":        loaded.Metadata.ConfigPath,
			"configFilePresent": loaded.Metadata.ConfigFilePresent,
			"toolDetection":     toJSONValue(t, loaded.Metadata.ToolDetection),
		},
	}
}

func toJSONValue(t *testing.T, value any) any {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	return decoded
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()

	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}

	return string(raw)
}

func resolveParityStrings(values []string, rootDir string, homeDir string) []string {
	if len(values) == 0 {
		return nil
	}

	resolved := make([]string, len(values))
	for index, value := range values {
		resolved[index] = resolveParityString(value, rootDir, homeDir)
	}

	return resolved
}

func resolveParityStringMap(values map[string]string, rootDir string, homeDir string) map[string]string {
	if len(values) == 0 {
		return nil
	}

	resolved := make(map[string]string, len(values))
	for key, value := range values {
		resolved[key] = resolveParityString(value, rootDir, homeDir)
	}

	return resolved
}

func resolveParityValue(value any, rootDir string, homeDir string) any {
	switch typed := value.(type) {
	case string:
		return resolveParityString(typed, rootDir, homeDir)
	case []any:
		resolved := make([]any, len(typed))
		for index, item := range typed {
			resolved[index] = resolveParityValue(item, rootDir, homeDir)
		}

		return resolved
	case map[string]any:
		resolved := make(map[string]any, len(typed))
		for key, item := range typed {
			resolved[key] = resolveParityValue(item, rootDir, homeDir)
		}

		return resolved
	default:
		return value
	}
}

func resolveParityString(value string, rootDir string, homeDir string) string {
	value = strings.ReplaceAll(value, "__TMP__", rootDir)
	value = strings.ReplaceAll(value, "__HOME__", homeDir)
	return value
}

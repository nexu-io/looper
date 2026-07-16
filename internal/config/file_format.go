package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

func ReadPartialConfigFile(path string) (PartialConfig, bool, error) {
	return readConfigFile(path)
}

// DecodePartialConfigFile decodes exactly the supplied bytes using the normal
// config-file rules. Callers that already captured a file revision use this to
// avoid a second, independently racing read.
func DecodePartialConfigFile(path string, raw []byte) (PartialConfig, error) {
	if err := validateConfigFileSuffix(path); err != nil {
		return PartialConfig{}, err
	}
	return decodeConfigFile(path, raw)
}

// MarshalPatchedConfigFile writes the patched typed sections back into the
// original top-level document. Unknown top-level extension sections are kept;
// the selected format is still normalized by MarshalConfigFile, so comments
// and lexical ordering are not preserved.
func MarshalPatchedConfigFile(path string, original []byte, originalPresent bool, patched PartialConfig, paths []string) ([]byte, error) {
	root := map[string]any{}
	if originalPresent {
		decoded, err := decodeGenericConfigDocument(path, original)
		if err != nil {
			return nil, err
		}
		root = decoded
	}

	patchedRoot, err := partialConfigJSONMap(patched)
	if err != nil {
		return nil, err
	}
	sections := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		section := strings.SplitN(path, ".", 2)[0]
		if section != "" {
			sections[section] = struct{}{}
		}
	}
	for section := range sections {
		deleteConfigDocumentKeyFold(root, section)
		if value, ok := patchedRoot[section]; ok {
			// Known config sections came through a JSON-number-preserving typed
			// patch. Normalize only those replacement values for TOML/YAML. Leave
			// every untouched extension value in its decoder-native type so dates,
			// times, uint64 values, and other format-specific scalars survive.
			root[section] = normalizeJSONNumbers(value)
		}
	}
	// Canonical dashboard edits also retire any deprecated file-layer aliases
	// that could otherwise continue to win after an unset. Only the mapped leaf
	// is removed; unrelated legacy fields and extension sections are preserved.
	for _, path := range paths {
		for _, alias := range configPatchLegacyAliases(path) {
			unsetConfigDocumentPathFold(root, strings.Split(alias, "."))
		}
	}
	return marshalConfigFileValue(path, root)
}

func configPatchLegacyAliases(path string) []string {
	switch path {
	case "agent.timeouts.plannerMaxRuntimeSeconds":
		return []string{"agent.timeouts.plannerSeconds"}
	case "agent.timeouts.workerMaxRuntimeSeconds":
		return []string{"agent.timeouts.workerSeconds"}
	case "agent.timeouts.reviewerMaxRuntimeSeconds":
		return []string{"agent.timeouts.reviewerSeconds"}
	case "agent.timeouts.fixerMaxRuntimeSeconds":
		return []string{"agent.timeouts.fixerSeconds"}
	case "roles.fixer.triggers.authorFilter":
		return []string{"defaults.fixAllPullRequests"}
	}

	const behavior = "roles.reviewer.behavior."
	if strings.HasPrefix(path, behavior) {
		suffix := strings.TrimPrefix(path, behavior)
		aliases := []string{"reviewer." + suffix}
		if path == behavior+"reviewEvents.clean" {
			aliases = append(aliases, "defaults.allowAutoApprove")
		}
		if path == behavior+"detectDuplicateFindings" {
			aliases = append(aliases,
				"reviewer.dedupeFindings",
				"roles.reviewer.behavior.dedupeFindings",
			)
		}
		return aliases
	}

	const discovery = "roles.reviewer.discovery."
	if strings.HasPrefix(path, discovery) {
		return []string{"roles.reviewer." + strings.TrimPrefix(path, discovery)}
	}
	return nil
}

func unsetConfigDocumentPathFold(root map[string]any, segments []string) {
	current := root
	for index, segment := range segments {
		key := ""
		for candidate := range current {
			if strings.EqualFold(candidate, segment) {
				key = candidate
				break
			}
		}
		if key == "" {
			return
		}
		if index == len(segments)-1 {
			delete(current, key)
			return
		}
		next, ok := current[key].(map[string]any)
		if !ok {
			return
		}
		current = next
	}
}

func decodeGenericConfigDocument(path string, raw []byte) (map[string]any, error) {
	if err := validateConfigFileSuffix(path); err != nil {
		return nil, err
	}
	decoded := map[string]any{}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		if err := rejectDuplicateJSONNames(raw); err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, err
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("trailing JSON value")
		}
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
	case ".toml":
		if err := toml.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
	}
	if decoded == nil {
		decoded = map[string]any{}
	}
	return decoded, nil
}

func rejectDuplicateJSONNames(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("trailing JSON token %v", token)
	}
	return nil
}

// ValidateUniqueJSONNames rejects ambiguous JSON objects before a last-key-wins
// decoder can hide duplicate request or config fields.
func ValidateUniqueJSONNames(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	for {
		err := consumeUniqueJSONValue(decoder)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func consumeUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, structured := token.(json.Delim)
	if !structured {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("invalid JSON object key")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON object name %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func deleteConfigDocumentKeyFold(root map[string]any, canonical string) {
	for key := range root {
		if strings.EqualFold(key, canonical) {
			delete(root, key)
		}
	}
}

func MarshalConfigFile(path string, value any) ([]byte, error) {
	normalized, err := normalizeConfigEncodingValue(value)
	if err != nil {
		return nil, err
	}
	return marshalConfigFileValue(path, normalized)
}

func marshalConfigFileValue(path string, value any) ([]byte, error) {
	if err := validateConfigFileSuffix(path); err != nil {
		return nil, err
	}

	var (
		raw []byte
		err error
	)
	if strings.EqualFold(filepath.Ext(path), ".json") {
		raw, err = json.MarshalIndent(value, "", "  ")
	} else if strings.EqualFold(filepath.Ext(path), ".yaml") || strings.EqualFold(filepath.Ext(path), ".yml") {
		raw, err = yaml.Marshal(value)
	} else {
		raw, err = toml.Marshal(value)
	}
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	return raw, nil
}

func normalizeConfigEncodingValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("normalize config for encoding: %w", err)
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return nil, fmt.Errorf("normalize config for encoding: %w", err)
	}
	return normalizeJSONNumbers(normalized), nil
}

func normalizeJSONNumbers(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			typed[key] = normalizeJSONNumbers(child)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = normalizeJSONNumbers(child)
		}
		return typed
	case json.Number:
		if integer, err := strconv.ParseInt(typed.String(), 10, 64); err == nil {
			return integer
		}
		if floating, err := strconv.ParseFloat(typed.String(), 64); err == nil {
			return floating
		}
		return typed.String()
	default:
		return value
	}
}

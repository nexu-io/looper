package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

func ReadPartialConfigFile(path string) (PartialConfig, bool, error) {
	return readConfigFile(path)
}

func MarshalConfigFile(path string, value any) ([]byte, error) {
	if err := validateConfigFileSuffix(path); err != nil {
		return nil, err
	}

	normalized, err := normalizeConfigEncodingValue(value)
	if err != nil {
		return nil, err
	}

	var (
		raw []byte
	)
	if strings.EqualFold(filepath.Ext(path), ".json") {
		raw, err = json.MarshalIndent(normalized, "", "  ")
	} else if strings.EqualFold(filepath.Ext(path), ".yaml") || strings.EqualFold(filepath.Ext(path), ".yml") {
		raw, err = yaml.Marshal(normalized)
	} else {
		raw, err = toml.Marshal(normalized)
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

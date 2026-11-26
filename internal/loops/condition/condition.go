package condition

import (
	"encoding/json"
	"fmt"
	"strings"
)

const metadataKey = "blockedCondition"

type Kind string

const (
	ProductSpec    Kind = "product_spec"
	DiskRecovered  Kind = "disk_recovered"
	CISettled      Kind = "ci_settled"
	ReviewUpdated  Kind = "review_updated"
	HumanAnswered  Kind = "human_answered"
	InfraRecovered Kind = "infra_recovered"
)

type Record struct {
	Kind        Kind   `json:"kind"`
	Since       string `json:"since,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

func (r Record) Valid() bool {
	switch r.Kind {
	case ProductSpec, DiskRecovered, CISettled, ReviewUpdated, HumanAnswered, InfraRecovered:
		return true
	default:
		return false
	}
}

func Read(metadataJSON *string) (Record, bool) {
	metadata, err := parseMetadata(metadataJSON)
	if err != nil {
		return Record{}, false
	}
	raw, ok := metadata[metadataKey]
	if !ok {
		return Record{}, false
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return Record{}, false
	}
	var record Record
	if json.Unmarshal(payload, &record) != nil || !record.Valid() {
		return Record{}, false
	}
	return record, true
}

func Set(metadataJSON *string, record Record) (string, error) {
	if !record.Valid() {
		return "", fmt.Errorf("unknown blocked condition: %q", record.Kind)
	}
	metadata, err := parseMetadata(metadataJSON)
	if err != nil {
		return "", err
	}
	metadata[metadataKey] = record
	payload, err := json.Marshal(metadata)
	return string(payload), err
}

func Clear(metadataJSON *string) (string, error) {
	metadata, err := parseMetadata(metadataJSON)
	if err != nil {
		return "", err
	}
	delete(metadata, metadataKey)
	payload, err := json.Marshal(metadata)
	return string(payload), err
}

func parseMetadata(metadataJSON *string) (map[string]any, error) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return map[string]any{}, nil
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return nil, err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	return metadata, nil
}

package loops

import (
	"encoding/json"
	"strings"
)

// ProductSpecConfirmation records an explicit Plane reply from the configured
// product owner. It lets Looper preserve provenance when the owner replies with
// inline text or an external document that Looper then associates on their behalf.
type ProductSpecConfirmation struct {
	URL          string `json:"url"`
	PlaneActorID string `json:"planeActorId"`
	ConfirmedAt  string `json:"confirmedAt"`
}

func ReadProductSpecConfirmation(metadataJSON *string) (ProductSpecConfirmation, bool) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ProductSpecConfirmation{}, false
	}
	var envelope struct {
		Confirmation *ProductSpecConfirmation `json:"productSpecConfirmation"`
	}
	if json.Unmarshal([]byte(*metadataJSON), &envelope) != nil || envelope.Confirmation == nil {
		return ProductSpecConfirmation{}, false
	}
	confirmation := *envelope.Confirmation
	confirmation.URL = strings.TrimSpace(confirmation.URL)
	confirmation.PlaneActorID = strings.TrimSpace(confirmation.PlaneActorID)
	confirmation.ConfirmedAt = strings.TrimSpace(confirmation.ConfirmedAt)
	if confirmation.URL == "" || confirmation.PlaneActorID == "" {
		return ProductSpecConfirmation{}, false
	}
	return confirmation, true
}

func WriteProductSpecConfirmation(metadataJSON *string, confirmation ProductSpecConfirmation) (string, error) {
	object := map[string]any{}
	if metadataJSON != nil && strings.TrimSpace(*metadataJSON) != "" {
		if err := json.Unmarshal([]byte(*metadataJSON), &object); err != nil {
			return "", err
		}
	}
	object["productSpecConfirmation"] = confirmation
	encoded, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func ProductSpecConfirmedBy(metadataJSON *string, specURL, planeActorID string) bool {
	confirmation, ok := ReadProductSpecConfirmation(metadataJSON)
	return ok && confirmation.URL == strings.TrimSpace(specURL) && confirmation.PlaneActorID == strings.TrimSpace(planeActorID)
}

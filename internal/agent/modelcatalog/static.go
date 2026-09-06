package modelcatalog

import (
	_ "embed"
	"encoding/json"

	"github.com/nexu-io/looper/internal/config"
)

//go:embed static_catalog.json
var staticCatalogJSON []byte

type staticEntry struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func loadStaticCatalog() map[config.AgentVendor][]Model {
	var raw map[string][]staticEntry
	if err := json.Unmarshal(staticCatalogJSON, &raw); err != nil {
		panic("modelcatalog: invalid static_catalog.json: " + err.Error())
	}
	out := make(map[config.AgentVendor][]Model, len(raw))
	for vendor, entries := range raw {
		models := make([]Model, 0, len(entries))
		for _, e := range entries {
			if e.ID == "" {
				continue
			}
			label := e.Label
			if label == "" {
				label = e.ID
			}
			models = append(models, Model{ID: e.ID, Label: label, Source: SourceStatic})
		}
		out[config.AgentVendor(vendor)] = models
	}
	return out
}

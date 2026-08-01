package modelcatalog

import "sort"

// mergeModels unions static then probe by id.
// Probe label wins on conflict only when the probe entry has a non-empty label;
// bare probe ids keep the static label when one exists.
// Ordering: static recommendations first (stable input order), then probe-only
// ids sorted alphabetically. Empty labels default to id for API consumers.
func mergeModels(staticModels, probeModels []Model) []Model {
	byID := make(map[string]Model, len(staticModels)+len(probeModels))
	staticOrder := make([]string, 0, len(staticModels))
	fromStatic := make(map[string]struct{}, len(staticModels))

	for _, m := range staticModels {
		id := m.ID
		if id == "" {
			continue
		}
		if _, ok := fromStatic[id]; ok {
			continue
		}
		fromStatic[id] = struct{}{}
		staticOrder = append(staticOrder, id)
		cp := m
		if cp.Source == "" {
			cp.Source = SourceStatic
		}
		if cp.Label == "" {
			cp.Label = id
		}
		byID[id] = cp
	}

	probeOnly := make([]string, 0)
	seenProbe := make(map[string]struct{})
	for _, m := range probeModels {
		id := m.ID
		if id == "" {
			continue
		}
		if _, ok := seenProbe[id]; ok {
			// Keep first probe occurrence.
			continue
		}
		seenProbe[id] = struct{}{}
		probeLabel := m.Label // empty means "no label from probe"
		if _, ok := fromStatic[id]; ok {
			existing := byID[id]
			if probeLabel != "" {
				existing.Label = probeLabel
			}
			existing.Source = SourceMerged
			if existing.Label == "" {
				existing.Label = id
			}
			byID[id] = existing
			continue
		}
		label := probeLabel
		if label == "" {
			label = id
		}
		probeOnly = append(probeOnly, id)
		byID[id] = Model{ID: id, Label: label, Source: SourceProbe}
	}

	sort.Strings(probeOnly)

	out := make([]Model, 0, len(staticOrder)+len(probeOnly))
	for _, id := range staticOrder {
		out = append(out, byID[id])
	}
	for _, id := range probeOnly {
		out = append(out, byID[id])
	}
	return out
}

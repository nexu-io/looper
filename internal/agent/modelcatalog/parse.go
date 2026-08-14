package modelcatalog

import (
	"encoding/json"
	"strings"
)

func parseOpenCodeModels(stdout []byte) []Model {
	// OpenCode prints one bare id per line (provider/model). Reject multiword
	// diagnostic lines such as "Not logged in".
	return parseStrictIDLines(stdout)
}

func parseCursorModels(stdout []byte) []Model {
	return parseTableOrIDLines(stdout)
}

func parseGrokModels(stdout []byte) []Model {
	return parseTableOrIDLines(stdout)
}

// parsePiModels parses `pi --list-models` multi-column table output:
//
//	provider               model                                               context  ...
//	openai                 gpt-4o                                              128K     ...
//
// Model IDs are provider/model when both columns are present (pi accepts that form).
func parsePiModels(stdout []byte) []Model {
	lines := strings.Split(string(stdout), "\n")
	out := make([]Model, 0, len(lines))
	seen := make(map[string]struct{})
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || shouldSkipDecorLine(line) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Skip header row (provider model context ...).
		if strings.EqualFold(fields[0], "provider") && strings.EqualFold(fields[1], "model") {
			continue
		}
		provider := fields[0]
		model := fields[1]
		// Pi table tokens are lowercase ids; reject diagnostic sentences like "Not logged in".
		if !looksLikePiTableToken(provider) || !looksLikePiTableToken(model) {
			continue
		}
		id := provider + "/" + model
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, Model{ID: id, Source: SourceProbe})
	}
	return out
}

// looksLikePiTableToken accepts provider/model column values from pi --list-models
// (lowercase alphanumerics plus - _ . /). Uppercase English words are rejected.
func looksLikePiTableToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		r := s[i]
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/':
		default:
			return false
		}
	}
	r0 := s[0]
	return (r0 >= 'a' && r0 <= 'z') || (r0 >= '0' && r0 <= '9')
}

type ompModelsPayload struct {
	Models []ompModelEntry `json:"models"`
}

type ompModelEntry struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Selector string `json:"selector"`
	Name     string `json:"name"`
}

// parseOmpModels parses `omp models --json` output. Prefer selector as Model.ID;
// fallback provider/id or bare id. Use name as Label when present.
func parseOmpModels(stdout []byte) ([]Model, error) {
	trimmed := bytesTrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var payload ompModelsPayload
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		var arr []ompModelEntry
		if err2 := json.Unmarshal(trimmed, &arr); err2 != nil {
			return nil, err
		}
		payload.Models = arr
	}
	out := make([]Model, 0, len(payload.Models))
	seen := make(map[string]struct{})
	for _, e := range payload.Models {
		id := strings.TrimSpace(e.Selector)
		if id == "" {
			provider := strings.TrimSpace(e.Provider)
			rawID := strings.TrimSpace(e.ID)
			switch {
			case provider != "" && rawID != "":
				id = provider + "/" + rawID
			case rawID != "":
				id = rawID
			default:
				continue
			}
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		label := strings.TrimSpace(e.Name)
		out = append(out, Model{ID: id, Label: label, Source: SourceProbe})
	}
	return out, nil
}

// parseLineModels is the shared text parser used by cursor/grok-style output.
// Kept as an alias for tests and call sites that want the table-tolerant path.
func parseLineModels(stdout []byte) []Model {
	return parseTableOrIDLines(stdout)
}

// parseStrictIDLines accepts only a single model-id token per non-empty line.
func parseStrictIDLines(stdout []byte) []Model {
	lines := strings.Split(string(stdout), "\n")
	out := make([]Model, 0, len(lines))
	seen := make(map[string]struct{})
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || shouldSkipDecorLine(line) {
			continue
		}
		// Multiword lines are diagnostics/warnings, not model ids.
		if strings.ContainsAny(line, " \t") {
			continue
		}
		id := strings.TrimRight(line, ",;:")
		if id == "" || !looksLikeModelID(id) {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		// Leave Label empty when probe has no separate label so merge can keep
		// a better static label (probe label wins only when present).
		out = append(out, Model{ID: id, Source: SourceProbe})
	}
	return out
}

// parseTableOrIDLines accepts bare ids or recognized table rows:
// "id - label", "id — label", "id\tlabel". Multiword lines without those
// separators (e.g. "Not logged in") are rejected.
func parseTableOrIDLines(stdout []byte) []Model {
	lines := strings.Split(string(stdout), "\n")
	out := make([]Model, 0, len(lines))
	seen := make(map[string]struct{})
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || shouldSkipDecorLine(line) {
			continue
		}
		id, label, ok := parseModelLine(line)
		if !ok {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, Model{ID: id, Label: label, Source: SourceProbe})
	}
	return out
}

func shouldSkipDecorLine(line string) bool {
	lower := strings.ToLower(line)
	if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "─") || strings.HasPrefix(line, "━") {
		return true
	}
	if lower == "models" || lower == "model" || strings.HasPrefix(lower, "available model") {
		return true
	}
	return false
}

func parseModelLine(line string) (id, label string, ok bool) {
	// Prefer explicit table separators over whitespace splitting so diagnostic
	// sentences are not partially accepted as model ids.
	for _, sep := range []string{" - ", " — ", "\t"} {
		if i := strings.Index(line, sep); i > 0 {
			id = strings.TrimSpace(line[:i])
			label = strings.TrimSpace(line[i+len(sep):])
			id = strings.TrimRight(id, ",;:")
			if id == "" || !looksLikeModelID(id) {
				return "", "", false
			}
			return id, label, true
		}
	}
	fields := strings.Fields(line)
	if len(fields) != 1 {
		// Multiword without recognized separator → diagnostic, not a model.
		return "", "", false
	}
	id = strings.TrimRight(fields[0], ",;:")
	if id == "" || !looksLikeModelID(id) {
		return "", "", false
	}
	// Bare id: no separate label from probe.
	return id, "", true
}

func looksLikeModelID(id string) bool {
	if id == "" {
		return false
	}
	// Reject pure decoration / table borders.
	if strings.Trim(id, "-─━=|*+") == "" {
		return false
	}
	// Must start with alphanumeric.
	r := id[0]
	if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
		return true
	}
	return false
}

type codexModelsPayload struct {
	Models []codexModelEntry `json:"models"`
}

type codexModelEntry struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Model      string `json:"model"`
	Visibility string `json:"visibility"`
	// Some builds nest identity differently.
	Label string `json:"label"`
}

func parseCodexModels(stdout []byte) ([]Model, error) {
	trimmed := bytesTrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var payload codexModelsPayload
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		// Some builds may print a bare array.
		var arr []codexModelEntry
		if err2 := json.Unmarshal(trimmed, &arr); err2 != nil {
			return nil, err
		}
		payload.Models = arr
	}

	hasVisibility := false
	for _, e := range payload.Models {
		if strings.TrimSpace(e.Visibility) != "" {
			hasVisibility = true
			break
		}
	}

	out := make([]Model, 0, len(payload.Models))
	seen := make(map[string]struct{})
	for _, e := range payload.Models {
		if hasVisibility {
			vis := strings.ToLower(strings.TrimSpace(e.Visibility))
			// Prefer visibility=list when the field exists; still include
			// experimental entries that are list-visible.
			if vis != "" && vis != "list" {
				continue
			}
		}
		id := firstNonEmpty(e.ID, e.Slug, e.Model)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		// Do not fall back to id: empty means "no label from probe" so merge
		// can preserve a better static label.
		label := firstNonEmpty(e.Label, e.Name)
		out = append(out, Model{ID: id, Label: label, Source: SourceProbe})
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

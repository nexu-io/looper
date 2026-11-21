package cliapp

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/storage"
	"github.com/spf13/cobra"
)

var planeTaskKey = regexp.MustCompile(`^[A-Z0-9_]+(?:-[A-Z0-9_]+)*-[1-9][0-9]*$`)
var planeTaskUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

const collabExportCoverage = "Local snapshot only. Other devices, removed history and unrecorded events may be missing. Raw session logs, prompts, and message bodies stay on the device."

func (r *commandRuntime) collabExport(cmd *cobra.Command, args []string) error {
	ref := strings.TrimSpace(args[0])
	task := collabTaskRef{value: strings.ToUpper(ref)}
	refField := "taskKey"
	if !planeTaskKey.MatchString(task.value) || planeTaskUUID.MatchString(task.value) {
		task = parseCollabTaskURL(ref)
		if task.value == "" {
			return fmt.Errorf("provide a Plane task key (for example OPEND-123) or its full task URL")
		}
		if planeTaskUUID.MatchString(task.value) {
			refField = "taskId"
		}
	}
	output := getStringFlag(cmd, "output")
	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("--output is required")
	}
	_, dir, err := r.collabConfig(cmd)
	if err != nil {
		return err
	}
	db, err := openCollabReadOnly(dir)
	if err != nil {
		return fmt.Errorf("open local collaboration history: %w", err)
	}
	defer db.Close()
	ctx := cmd.Context()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	loops, matchedURL, err := collabExportLoops(ctx, tx, task)
	if err != nil {
		return err
	}
	capturedAt := time.Now().UTC().Format(time.RFC3339)
	repos := storage.NewRepositories(tx)
	reports := make([]map[string]any, 0, len(loops))
	diagnostics := []map[string]any{}
	for _, id := range loops {
		loop, err := repos.Loops.GetByID(ctx, id)
		if err != nil {
			return err
		}
		report, err := collabTraceForLoop(ctx, repos, loopSelectorResult{Loop: *loop}, true)
		if err != nil {
			return err
		}
		report["dispatchId"] = strings.TrimPrefix(id, "loop_plane_")
		reports = append(reports, report)
		diagnostics = append(diagnostics, collabDiagnostics(report)...)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	trace := map[string]any{"schemaVersion": 1, "scope": "local", "capturedAt": capturedAt,
		refField: task.value, "taskUrl": matchedURL, "dispatches": reports, "coverage": collabExportCoverage}
	if err := writeCollabArchive(ctx, output, collabExportSummary(task.value, matchedURL, capturedAt, reports), trace, diagnostics); err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"path": output, refField: task.value, "localDispatchCount": len(reports), "scope": "local"})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Exported %s (%d local dispatches) to %s\n", task.value, len(reports), output)
	return err
}

func collabExportLoops(ctx context.Context, tx *sql.Tx, task collabTaskRef) ([]string, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, CASE WHEN json_valid(metadata_json)
		THEN json_extract(metadata_json, '$.worker.issueUrl') END
		FROM loops WHERE type='worker' AND id GLOB 'loop_plane_?*' ORDER BY seq`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var loops []string
	var matched collabTaskRef
	for rows.Next() {
		var id string
		var rawURL sql.NullString
		if err := rows.Scan(&id, &rawURL); err != nil {
			return nil, "", err
		}
		candidate := parseCollabTaskURL(rawURL.String)
		if candidate.value != task.value || (task.scope != "" && task.scope != candidate.scope) {
			continue
		}
		if matched.scope != "" && matched.scope != candidate.scope {
			return nil, "", fmt.Errorf("%s matches multiple Plane task URLs; rerun with the exact task URL", task.value)
		}
		matched = candidate
		loops = append(loops, id)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(loops) == 0 {
		return nil, "", fmt.Errorf("no matching Plane dispatch for %s in this collaboration state directory", task.value)
	}
	return loops, matched.url, nil
}

type collabTaskRef struct {
	value string
	scope string
	url   string
}

func parseCollabTaskURL(rawURL string) collabTaskRef {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return collabTaskRef{}
	}
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	last := len(parts) - 1
	if last < 3 {
		return collabTaskRef{}
	}
	value := parts[last]
	var scopeEnd int
	switch {
	case parts[last-1] == "browse" && planeTaskKey.MatchString(strings.ToUpper(value)):
		scopeEnd = last - 1
		parts[last] = strings.ToUpper(value)
	case parts[last-1] == "issues" && planeTaskUUID.MatchString(value):
		// Older dispatches store a UUID link instead of the human-readable key.
		switch {
		case parts[last-2] == "browse":
			scopeEnd = last - 2
		case last >= 5 && parts[last-3] == "projects" && planeTaskUUID.MatchString(parts[last-2]):
			scopeEnd = last - 3
		}
		parts[last] = strings.ToLower(value)
	}
	if scopeEnd < 2 || parts[scopeEnd-1] == "" {
		return collabTaskRef{}
	}
	normalized := url.URL{Scheme: u.Scheme, Host: strings.ToLower(u.Host), Path: strings.Join(parts[:scopeEnd], "/")}
	scope := normalized.String()
	normalized.Path = strings.Join(parts, "/")
	return collabTaskRef{value: parts[last], scope: scope, url: normalized.String()}
}

func writeCollabArchive(ctx context.Context, output, summary string, trace map[string]any, diagnostics []map[string]any) error {
	tmp, err := os.CreateTemp(filepath.Dir(output), ".looper-export-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	archive := zip.NewWriter(tmp)
	summaryEntry, err := archive.Create("summary.md")
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(summaryEntry, summary); err != nil {
		return err
	}
	for _, entry := range []struct {
		name  string
		value any
	}{
		{"trace.json", trace},
		{"diagnostics.json", map[string]any{"errors": diagnostics}},
	} {
		writer, err := archive.Create(entry.name)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(entry.value); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Publish a complete file atomically without replacing an existing archive.
	if err := os.Link(tmp.Name(), output); err != nil {
		return fmt.Errorf("create export %q: %w", output, err)
	}
	return nil
}

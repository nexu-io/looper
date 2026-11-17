package telemetry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
)

type Result struct {
	Uploaded  int `json:"uploaded"`
	Unchanged int `json:"unchanged"`
}

// Sync retries the same persisted revision until acknowledged. A failed upload
// never changes the task or causes its agent to run again.
func Sync(ctx context.Context, db *sql.DB, cfg config.Config, force bool) (Result, error) {
	result := Result{}
	loops, err := storage.NewRepositories(db).Loops.List(ctx)
	if err != nil {
		return result, err
	}
	var failures []error
	for _, provider := range cfg.Providers {
		s := provider.StrictDispatch
		if provider.Kind != config.ProviderKindPlane || s == nil || !s.Enabled || (!force && !s.TelemetryEnabled) {
			continue
		}
		if provider.Workspace == nil || provider.ProjectID == nil {
			return result, fmt.Errorf("telemetry provider %s has no Plane project", provider.ID)
		}
		credentials, err := planestrict.LoadCredentials(s.BindingID, s.KeyRevision, s.NodeID, s.PrivateKeyFile)
		if err != nil {
			return result, fmt.Errorf("telemetry credentials for %s: %w", provider.ID, err)
		}
		client, err := planestrict.NewClient(s.BaseURL, *provider.Workspace, *provider.ProjectID, credentials)
		if err != nil {
			return result, err
		}
		destination := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join([]string{s.BaseURL, *provider.Workspace, *provider.ProjectID, s.BindingID}, "\n"))))
		projects := map[string]bool{}
		for _, project := range cfg.Projects {
			if project.Provider == provider.ID {
				projects[project.ID] = true
			}
		}
		for _, loop := range loops {
			if !projects[loop.ProjectID] {
				continue
			}
			payload, revision, err := pending(ctx, db, destination, loop.ID)
			if err != nil {
				failures = append(failures, fmt.Errorf("task %s: %w", loop.ID, err))
				continue
			}
			if payload == nil {
				result.Unchanged++
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			ack, err := client.UploadTelemetry(requestCtx, payload)
			cancel()
			if err == nil && (ack.TaskID != loop.ID || ack.Revision != revision || ack.StoredRevision < revision) {
				err = fmt.Errorf("invalid telemetry acknowledgement")
			}
			if err == nil {
				_, err = db.ExecContext(ctx, `UPDATE telemetry_checkpoints SET acknowledged=1,revision=? WHERE destination=? AND loop_id=? AND revision=?`, ack.StoredRevision, destination, loop.ID, revision)
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("task %s upload: %w", loop.ID, err))
			} else {
				result.Uploaded++
			}
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
		}
	}
	return result, errors.Join(failures...)
}

func pending(ctx context.Context, db *sql.DB, destination, loopID string) (json.RawMessage, int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO telemetry_identity(id,database_id) VALUES(1,?)`, eventlog.NewEventID("db")); err != nil {
		return nil, 0, err
	}
	var databaseID string
	if err = tx.QueryRowContext(ctx, `SELECT database_id FROM telemetry_identity WHERE id=1`).Scan(&databaseID); err != nil {
		return nil, 0, err
	}
	var revision int64
	var fingerprint, payload string
	var acknowledged bool
	err = tx.QueryRowContext(ctx, `SELECT revision,fingerprint,payload,acknowledged FROM telemetry_checkpoints WHERE destination=? AND loop_id=?`, destination, loopID).Scan(&revision, &fingerprint, &payload, &acknowledged)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}
	if err == nil && !acknowledged {
		return json.RawMessage(payload), revision, tx.Commit()
	}
	repos := storage.NewRepositories(tx)
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return nil, 0, err
	}
	if loop == nil {
		return nil, 0, nil
	}
	task, err := snapshot(ctx, repos, *loop)
	if err != nil || task == nil {
		return nil, 0, err
	}
	raw, err := json.Marshal(task)
	if err != nil {
		return nil, 0, err
	}
	nextFingerprint := fmt.Sprintf("%x", sha256.Sum256(raw))
	if fingerprint == nextFingerprint {
		return nil, 0, tx.Commit()
	}
	revision++
	task["revision"] = revision
	raw, err = json.Marshal(map[string]any{"schemaVersion": 1, "databaseId": databaseID, "task": task})
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > 1<<20 {
		return nil, 0, fmt.Errorf("telemetry snapshot exceeds 1 MiB; retained locally")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO telemetry_checkpoints(destination,loop_id,revision,fingerprint,payload,acknowledged) VALUES(?,?,?,?,?,0)
		ON CONFLICT(destination,loop_id) DO UPDATE SET revision=excluded.revision,fingerprint=excluded.fingerprint,payload=excluded.payload,acknowledged=0`, destination, loopID, revision, nextFingerprint, string(raw))
	if err != nil {
		return nil, 0, err
	}
	return raw, revision, tx.Commit()
}

func Maintain(ctx context.Context, db *sql.DB, cfg config.Config, warn func(error)) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if _, err := Sync(ctx, db, cfg, false); err != nil && ctx.Err() == nil {
			warn(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

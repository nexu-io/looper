package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/storage"
)

type OpenSQLiteCoordinatorFunc func(context.Context, string, storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error)

type SyncConfiguredProjectsFunc func(context.Context, *storage.Repositories, config.Config, time.Time) error

type Options struct {
	Config                 config.Config
	Logger                 bootstrap.Logger
	Now                    func() time.Time
	OpenSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	SyncConfiguredProjects SyncConfiguredProjectsFunc
}

type Services struct {
	Coordinator  *storage.SQLiteCoordinator
	Repositories *storage.Repositories
}

type Runtime struct {
	config config.Config
	logger bootstrap.Logger
	now    func() time.Time

	openSQLiteCoordinator  OpenSQLiteCoordinatorFunc
	syncConfiguredProjects SyncConfiguredProjectsFunc

	mu           sync.RWMutex
	startedAt    *time.Time
	stopped      bool
	services     Services
	startErr     error
	startOnce    sync.Once
	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

func New(options Options) *Runtime {
	now := options.Now
	if now == nil {
		now = time.Now
	}

	openSQLiteCoordinator := options.OpenSQLiteCoordinator
	if openSQLiteCoordinator == nil {
		openSQLiteCoordinator = storage.OpenSQLiteCoordinator
	}

	syncConfiguredProjects := options.SyncConfiguredProjects
	if syncConfiguredProjects == nil {
		syncConfiguredProjects = defaultSyncConfiguredProjects
	}

	return &Runtime{
		config:                 options.Config,
		logger:                 options.Logger,
		now:                    now,
		openSQLiteCoordinator:  openSQLiteCoordinator,
		syncConfiguredProjects: syncConfiguredProjects,
		shutdownCh:             make(chan struct{}),
	}
}

func Start(ctx context.Context, deps bootstrap.RuntimeDependencies) (bootstrap.Runtime, error) {
	rt := New(Options{
		Config: deps.Config,
		Logger: deps.Logger,
	})
	if err := rt.Start(ctx); err != nil {
		return nil, err
	}

	return rt, nil
}

func (r *Runtime) Start(ctx context.Context) error {
	r.startOnce.Do(func() {
		r.startErr = r.start(ctx)
	})

	return r.startErr
}

func (r *Runtime) Stop(reason string) {
	r.shutdownOnce.Do(func() {
		if r.logger != nil {
			r.logger.Info("looperd runtime stopping", map[string]any{"reason": reason})
		}

		r.mu.Lock()
		r.stopped = true
		coordinator := r.services.Coordinator
		r.services = Services{}
		r.mu.Unlock()

		if coordinator != nil {
			if err := coordinator.Close(); err != nil && r.logger != nil {
				r.logger.Warn("looperd runtime close failed", map[string]any{"error": err.Error()})
			}
		}

		close(r.shutdownCh)

		if r.logger != nil {
			r.logger.Info("looperd runtime stopped", map[string]any{"reason": reason})
		}
	})
}

func (r *Runtime) WaitForShutdown() {
	<-r.shutdownCh
}

func (r *Runtime) Services() Services {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.services
}

func (r *Runtime) StartedAt() (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.startedAt == nil {
		return time.Time{}, false
	}

	return *r.startedAt, true
}

func (r *Runtime) Config() config.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.config
}

func (r *Runtime) start(ctx context.Context) error {
	r.mu.RLock()
	if r.stopped {
		r.mu.RUnlock()
		return fmt.Errorf("runtime already stopped")
	}
	r.mu.RUnlock()

	backupDir := ""
	if r.config.Storage.BackupDir != nil {
		backupDir = *r.config.Storage.BackupDir
	}

	coordinator, err := r.openSQLiteCoordinator(ctx, r.config.Storage.DBPath, storage.SQLiteCoordinatorOptions{
		BackupDir: backupDir,
		Now:       r.now,
	})
	if err != nil {
		return err
	}

	started := false
	defer func() {
		if !started {
			_ = coordinator.Close()
		}
	}()

	if r.config.Package.AutoMigrateOnStartup {
		_, err = coordinator.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{
			RequireBackup: r.config.Package.RequireBackupBeforeMigrate,
		})
		if err != nil {
			return err
		}
	}

	repositories := storage.NewRepositories(coordinator.DB())
	startedAt := r.now().UTC()
	if err := r.syncConfiguredProjects(ctx, repositories, r.config, startedAt); err != nil {
		return err
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return fmt.Errorf("runtime already stopped")
	}
	r.startedAt = &startedAt
	r.services = Services{
		Coordinator:  coordinator,
		Repositories: repositories,
	}
	r.mu.Unlock()

	started = true

	if r.logger != nil {
		r.logger.Info("looperd runtime assembled", map[string]any{
			"dbPath":         r.config.Storage.DBPath,
			"projectCount":   len(r.config.Projects),
			"autoMigrate":    r.config.Package.AutoMigrateOnStartup,
			"backupRequired": r.config.Package.RequireBackupBeforeMigrate,
		})
	}

	return nil
}

func defaultSyncConfiguredProjects(ctx context.Context, repositories *storage.Repositories, cfg config.Config, now time.Time) error {
	if repositories == nil || repositories.Projects == nil {
		return fmt.Errorf("projects repository is not configured")
	}

	nowISO := formatJavaScriptISOString(now)

	for _, project := range cfg.Projects {
		existing, err := repositories.Projects.GetByID(ctx, project.ID)
		if err != nil {
			return err
		}

		metadataJSONValue, err := buildProjectMetadataJSON(existing, project)
		if err != nil {
			return fmt.Errorf("build project metadata for %s: %w", project.ID, err)
		}

		baseBranch := cfg.Defaults.BaseBranch
		if project.BaseBranch != nil {
			baseBranch = *project.BaseBranch
		}

		createdAt := nowISO
		if existing != nil {
			createdAt = existing.CreatedAt
		}

		record := storage.ProjectRecord{
			ID:           project.ID,
			Name:         project.Name,
			RepoPath:     project.RepoPath,
			BaseBranch:   &baseBranch,
			Archived:     false,
			MetadataJSON: &metadataJSONValue,
			CreatedAt:    createdAt,
			UpdatedAt:    nowISO,
		}
		if err := repositories.Projects.Upsert(ctx, record); err != nil {
			return err
		}
	}

	return nil
}

func buildProjectMetadataJSON(existing *storage.ProjectRecord, project config.ProjectRefConfig) (string, error) {
	extras := make([]orderedJSONEntry, 0)
	repoRaw := []byte("null")

	if existing != nil {
		existingMetadata, err := parseProjectMetadata(existing.MetadataJSON)
		if err != nil {
			return "", err
		}
		for _, entry := range existingMetadata {
			switch entry.Key {
			case "repo":
				if existing.RepoPath == project.RepoPath {
					var existingRepo string
					if err := json.Unmarshal(entry.Raw, &existingRepo); err == nil && existingRepo != "" {
						repoRaw, err = json.Marshal(existingRepo)
						if err != nil {
							return "", err
						}
					}
				}
			case "worktreeRoot", "source":
				continue
			default:
				extras = append(extras, entry)
			}
		}
	}

	worktreeRootRaw := []byte("null")
	if project.WorktreeRoot != nil {
		encodedWorktreeRoot, err := json.Marshal(*project.WorktreeRoot)
		if err != nil {
			return "", err
		}
		worktreeRootRaw = encodedWorktreeRoot
	}

	entries := append(extras,
		orderedJSONEntry{Key: "repo", Raw: repoRaw},
		orderedJSONEntry{Key: "worktreeRoot", Raw: worktreeRootRaw},
		orderedJSONEntry{Key: "source", Raw: json.RawMessage(`"config"`)},
	)

	return marshalOrderedJSONObject(entries)
}

type orderedJSONEntry struct {
	Key string
	Raw json.RawMessage
}

func parseProjectMetadata(raw *string) ([]orderedJSONEntry, error) {
	if raw == nil || *raw == "" {
		return []orderedJSONEntry{}, nil
	}

	decoder := json.NewDecoder(strings.NewReader(*raw))
	startToken, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	startDelimiter, ok := startToken.(json.Delim)
	if !ok || startDelimiter != '{' {
		return nil, fmt.Errorf("project metadata must be a JSON object")
	}

	entries := make([]orderedJSONEntry, 0)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}

		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("project metadata key must be a string")
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}

		entries = append(entries, orderedJSONEntry{Key: key, Raw: value})
	}

	endToken, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	endDelimiter, ok := endToken.(json.Delim)
	if !ok || endDelimiter != '}' {
		return nil, fmt.Errorf("project metadata must end with a JSON object delimiter")
	}

	return entries, nil
}

func formatJavaScriptISOString(value time.Time) string {
	value = value.UTC()
	return fmt.Sprintf("%s.%03dZ", value.Format("2006-01-02T15:04:05"), value.Nanosecond()/int(time.Millisecond))
}

func marshalOrderedJSONObject(entries []orderedJSONEntry) (string, error) {
	buffer := &bytes.Buffer{}
	buffer.WriteByte('{')

	for index, entry := range entries {
		if index > 0 {
			buffer.WriteByte(',')
		}

		keyJSON, err := json.Marshal(entry.Key)
		if err != nil {
			return "", err
		}
		buffer.Write(keyJSON)
		buffer.WriteByte(':')
		buffer.Write(entry.Raw)
	}

	buffer.WriteByte('}')
	return buffer.String(), nil
}

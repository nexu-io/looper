package projects

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// ConfigSource supplies a coherent runtime configuration snapshot. Consumers
// capture one snapshot at the start of an operation and retain it for that
// operation's lifetime.
type ConfigSource interface {
	Snapshot() config.Config
}

// Catalog owns the immutable runtime view of Projects. The non-project portion
// of the configuration is frozen at construction; Publish atomically replaces
// only the already-materialized Projects view.
//
// Catalog keeps no caller-owned slices, maps, or pointers. Snapshot likewise
// returns a detached copy so a consumer cannot mutate the published view.
type Catalog struct {
	global  config.Config
	current atomic.Pointer[config.Config]
}

// NewCatalog creates a Catalog from the normalized global configuration. Its
// initial snapshot includes global.Projects so callers can construct and read a
// Catalog before the first database materialization is published.
func NewCatalog(global config.Config) *Catalog {
	frozen := cloneCatalogConfig(global)
	catalog := &Catalog{global: frozen}
	initial := cloneCatalogConfig(frozen)
	catalog.current.Store(&initial)
	return catalog
}

// Publish atomically installs a prevalidated, materialized Project view. It is
// intentionally infallible: validation belongs to MaterializeCatalog before a
// database mutation commits, while publication after commit is only a swap.
func (c *Catalog) Publish(projects []config.ProjectRefConfig) {
	if c == nil {
		return
	}
	next := cloneCatalogConfig(c.global)
	next.Projects = cloneCatalogProjects(projects)
	c.current.Store(&next)
}

// Snapshot returns a coherent, detached copy of the currently published full
// configuration.
func (c *Catalog) Snapshot() config.Config {
	if c == nil {
		return config.Config{}
	}
	current := c.current.Load()
	if current == nil {
		return config.Config{}
	}
	return cloneCatalogConfig(*current)
}

func cloneCatalogProjects(projects []config.ProjectRefConfig) []config.ProjectRefConfig {
	cloned := cloneCatalogConfig(config.Config{Projects: projects})
	return cloned.Projects
}

// Config is composed exclusively of JSON configuration values. JSON
// round-tripping gives the Catalog a single complete deep-copy boundary,
// including nested role overrides and map[string]any agent parameters. A
// failure indicates a programming error: normalized Config must remain JSON
// representable.
func cloneCatalogConfig(source config.Config) config.Config {
	encoded, err := json.Marshal(source)
	if err != nil {
		panic(fmt.Sprintf("clone project catalog config: %v", err))
	}
	var cloned config.Config
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		panic(fmt.Sprintf("clone project catalog config: %v", err))
	}
	return cloned
}

// MaterializeCatalog builds the immutable runtime Project view from active
// SQLite records. global supplies Providers and global defaults, but its
// Projects list is import input and is not consulted here.
func MaterializeCatalog(global config.Config, records []storage.ProjectRecord) ([]config.ProjectRefConfig, error) {
	projects := make([]config.ProjectRefConfig, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record.Archived {
			continue
		}
		if _, ok := seen[record.ID]; ok {
			return nil, fmt.Errorf("duplicate active project id %q", record.ID)
		}
		seen[record.ID] = struct{}{}

		metadata := parseMetadata(record.MetadataJSON)
		project := config.ProjectRefConfig{
			ID:         record.ID,
			Name:       record.Name,
			RepoPath:   record.RepoPath,
			BaseBranch: cloneStringPointer(record.BaseBranch),
			Provider:   metadataString(metadata, "provider"),
			Repo:       metadataString(metadata, "repo"),
		}
		if project.Provider != "" && !configuredProviderExists(global, project.Provider) {
			return nil, fmt.Errorf("project %q references unknown provider %q", project.ID, project.Provider)
		}
		if worktreeRoot := metadataString(metadata, "worktreeRoot"); worktreeRoot != "" {
			project.WorktreeRoot = &worktreeRoot
		}
		project.Path = metadataString(metadata, "path")
		if err := decodeMetadataValue(metadata, "network", &project.Network); err != nil {
			return nil, fmt.Errorf("decode project %q network policy: %w", project.ID, err)
		}
		if err := decodeMetadataValue(metadata, "webhook", &project.Webhook); err != nil {
			return nil, fmt.Errorf("decode project %q webhook policy: %w", project.ID, err)
		}
		if err := decodeMetadataValue(metadata, "roles", &project.Roles); err != nil {
			return nil, fmt.Errorf("decode project %q role policy: %w", project.ID, err)
		}
		projects = append(projects, project)
	}
	return projects, nil
}

func configuredProviderExists(cfg config.Config, providerID string) bool {
	for _, provider := range cfg.Providers {
		if provider.ID == providerID {
			return true
		}
	}
	return false
}

func decodeMetadataValue(metadata map[string]any, key string, target any) error {
	value, ok := metadata[key]
	if !ok || value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

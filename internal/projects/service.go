package projects

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/storage"
)

const legacyProjectIDPrefix = "legacy-id-"

var nonProjectIDPattern = regexp.MustCompile(`[^a-z0-9]+`)

type DetectRepoFunc func(context.Context, string) (string, error)

type Service struct {
	DB         *sql.DB
	Repos      *storage.Repositories
	Logger     bootstrap.Logger
	Now        func() time.Time
	DetectRepo DetectRepoFunc
}

type AddInput struct {
	ID           string
	Name         string
	RepoPath     string
	BaseBranch   string
	IDSource     string
	WorktreeRoot *string
	Repo         *string
}

type AddResult struct {
	Project                storage.ProjectRecord
	Repo                   *string
	DiscoveredPullRequests int
	DiscoveredWorktrees    int
	Warnings               []string
}

type ProjectIDCollisionError struct{ ProjectID string }

func (e ProjectIDCollisionError) Error() string {
	return fmt.Sprintf("Derived project id collides with an existing explicit project: %s", e.ProjectID)
}

func (s *Service) AddProject(ctx context.Context, input AddInput) (AddResult, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return AddResult{}, fmt.Errorf("projects repository is not configured")
	}

	existing, err := s.Repos.Projects.GetByID(ctx, input.ID)
	if err != nil {
		return AddResult{}, err
	}
	if existing != nil && input.IDSource != "derived" {
		return AddResult{}, ProjectIDCollisionError{ProjectID: input.ID}
	}
	projectID := input.ID
	if existing == nil {
		projectID = normalizeProjectID(input)
	}
	if existing == nil && projectID != input.ID {
		normalizedExisting, err := s.Repos.Projects.GetByID(ctx, projectID)
		if err != nil {
			return AddResult{}, err
		}
		if normalizedExisting != nil {
			metadata := parseMetadata(normalizedExisting.MetadataJSON)
			if normalized, _ := metadata["normalizedDerivedId"].(bool); !normalized {
				return AddResult{}, ProjectIDCollisionError{ProjectID: projectID}
			}
			existing = normalizedExisting
		}
	}

	if existing == nil {
		if err := assertValidProjectID(projectID); err != nil {
			return AddResult{}, err
		}
	}

	repo := input.Repo
	warnings := []string{}
	if repo == nil && s.DetectRepo != nil {
		detected, detectErr := s.DetectRepo(ctx, input.RepoPath)
		if detectErr != nil {
			warnings = append(warnings, fmt.Sprintf("Could not detect GitHub repo: %s", detectErr.Error()))
		} else if detected != "" {
			repo = &detected
		}
	}

	nowISO := currentISO(s.Now)
	metadata := parseMetadata(nil)
	if existing != nil {
		metadata = parseMetadata(existing.MetadataJSON)
	}
	derivedProjectID := deriveProjectIDFromRepoPath(input.RepoPath)
	normalizedDerivedID := false
	if normalized, _ := metadata["normalizedDerivedId"].(bool); normalized {
		normalizedDerivedID = true
	}
	if input.IDSource == "derived" && strings.HasPrefix(derivedProjectID, legacyProjectIDPrefix) && input.ID == normalizeDerivedProjectID(derivedProjectID) {
		normalizedDerivedID = true
	}
	metadata["repo"] = nil
	if repo != nil {
		metadata["repo"] = *repo
	}
	if input.WorktreeRoot != nil {
		metadata["worktreeRoot"] = *input.WorktreeRoot
	} else if _, ok := metadata["worktreeRoot"]; !ok {
		metadata["worktreeRoot"] = nil
	}
	if normalizedDerivedID {
		metadata["normalizedDerivedId"] = true
	}
	if existing != nil {
		if _, ok := metadata["source"]; !ok {
			metadata["source"] = "api"
		}
	} else {
		metadata["source"] = "api"
	}
	metadataJSON, err := buildAddProjectMetadataJSON(metadata)
	if err != nil {
		return AddResult{}, fmt.Errorf("marshal project metadata: %w", err)
	}

	record := storage.ProjectRecord{
		ID:           projectID,
		Name:         input.Name,
		RepoPath:     input.RepoPath,
		BaseBranch:   stringPointer(input.BaseBranch),
		Archived:     false,
		MetadataJSON: stringPointer(metadataJSON),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}
	if existing != nil {
		record.CreatedAt = existing.CreatedAt
	}
	if err := s.Repos.Projects.Upsert(ctx, record); err != nil {
		return AddResult{}, err
	}

	return AddResult{Project: record, Repo: repo, Warnings: warnings}, nil
}

func (s *Service) Get(ctx context.Context, id string) (*storage.ProjectRecord, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return nil, fmt.Errorf("projects repository is not configured")
	}
	return s.Repos.Projects.GetByID(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]storage.ProjectRecord, error) {
	if s.Repos == nil || s.Repos.Projects == nil {
		return nil, fmt.Errorf("projects repository is not configured")
	}
	return s.Repos.Projects.List(ctx)
}

func (s *Service) SyncConfigured(ctx context.Context, cfg config.Config, now time.Time) error {
	if s.Repos == nil || s.Repos.Projects == nil {
		return fmt.Errorf("projects repository is not configured")
	}

	nowISO := currentISO(func() time.Time { return now })
	for _, project := range cfg.Projects {
		existing, err := s.Repos.Projects.GetByID(ctx, project.ID)
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
		if err := s.Repos.Projects.Upsert(ctx, record); err != nil {
			return err
		}
	}

	return nil
}

func normalizeProjectID(input AddInput) string {
	if input.IDSource != "derived" {
		return input.ID
	}
	if input.ID != deriveProjectIDFromRepoPath(input.RepoPath) {
		return input.ID
	}
	if !strings.HasPrefix(input.ID, legacyProjectIDPrefix) {
		return input.ID
	}
	return normalizeDerivedProjectID(input.ID)
}

func normalizeDerivedProjectID(projectID string) string {
	if !strings.HasPrefix(projectID, legacyProjectIDPrefix) {
		return projectID
	}
	return "project_" + projectID
}

func deriveProjectIDFromRepoPath(repoPath string) string {
	segments := strings.FieldsFunc(repoPath, func(r rune) bool { return r == '/' || r == '\\' })
	lastSegment := "project"
	if len(segments) > 0 {
		lastSegment = segments[len(segments)-1]
	}
	normalized := strings.Trim(nonProjectIDPattern.ReplaceAllString(strings.ToLower(lastSegment), "-"), "-")
	if normalized == "" {
		return "project"
	}
	return normalized
}

func assertValidProjectID(projectID string) error {
	if projectID == "" || projectID == "." || projectID == ".." || strings.HasPrefix(projectID, legacyProjectIDPrefix) || containsProjectPathSeparator(projectID) || filepath.IsAbs(projectID) || isWindowsAbsolute(projectID) {
		return fmt.Errorf("invalid project id %q: must not contain path separators, dot segments, be an absolute path, or start with legacy-id-", projectID)
	}
	return nil
}

func containsProjectPathSeparator(projectID string) bool {
	return strings.Contains(projectID, "/") || strings.Contains(projectID, `\\`)
}

func isWindowsAbsolute(projectID string) bool {
	if len(projectID) >= 3 {
		drive := projectID[0]
		sep := projectID[2]
		if ((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) && projectID[1] == ':' && (sep == '/' || sep == '\\') {
			return true
		}
	}
	if len(projectID) >= 2 && strings.HasPrefix(projectID, `\\`) {
		return true
	}
	return false
}

func parseMetadata(metadataJSON *string) map[string]any {
	if metadataJSON == nil || *metadataJSON == "" {
		return map[string]any{}
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil || metadata == nil {
		return map[string]any{}
	}
	return metadata
}

func buildProjectMetadataJSON(existing *storage.ProjectRecord, project config.ProjectRefConfig) (string, error) {
	extras := map[string]json.RawMessage{}
	repoRaw := json.RawMessage("null")

	if existing != nil {
		existingMetadata := parseMetadata(existing.MetadataJSON)
		for key, value := range existingMetadata {
			switch key {
			case "repo":
				if existing.RepoPath == project.RepoPath {
					if repo, ok := value.(string); ok && repo != "" {
						encoded, err := json.Marshal(repo)
						if err != nil {
							return "", err
						}
						repoRaw = encoded
					}
				}
			case "worktreeRoot", "source":
				continue
			default:
				encoded, err := json.Marshal(value)
				if err != nil {
					return "", err
				}
				extras[key] = encoded
			}
		}
	}

	entries := make([]orderedJSONEntry, 0, len(extras)+3)
	extraKeys := make([]string, 0, len(extras))
	for key := range extras {
		extraKeys = append(extraKeys, key)
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		entries = append(entries, orderedJSONEntry{Key: key, Raw: extras[key]})
	}
	entries = append(entries, orderedJSONEntry{Key: "repo", Raw: repoRaw})
	if project.WorktreeRoot != nil {
		encoded, err := json.Marshal(*project.WorktreeRoot)
		if err != nil {
			return "", err
		}
		entries = append(entries, orderedJSONEntry{Key: "worktreeRoot", Raw: encoded})
	} else {
		entries = append(entries, orderedJSONEntry{Key: "worktreeRoot", Raw: json.RawMessage("null")})
	}
	entries = append(entries, orderedJSONEntry{Key: "source", Raw: json.RawMessage(`"config"`)})

	return marshalOrderedJSONObject(entries)
}

func buildAddProjectMetadataJSON(metadata map[string]any) (string, error) {
	entries := make([]orderedJSONEntry, 0, len(metadata))
	extraKeys := make([]string, 0, len(metadata))
	for key := range metadata {
		switch key {
		case "normalizedDerivedId", "repo", "worktreeRoot", "source":
			continue
		default:
			extraKeys = append(extraKeys, key)
		}
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		encoded, err := json.Marshal(metadata[key])
		if err != nil {
			return "", err
		}
		entries = append(entries, orderedJSONEntry{Key: key, Raw: encoded})
	}
	if value, ok := metadata["normalizedDerivedId"]; ok {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		entries = append(entries, orderedJSONEntry{Key: "normalizedDerivedId", Raw: encoded})
	}
	repoEncoded, err := json.Marshal(metadata["repo"])
	if err != nil {
		return "", err
	}
	entries = append(entries, orderedJSONEntry{Key: "repo", Raw: repoEncoded})
	worktreeRootEncoded, err := json.Marshal(metadata["worktreeRoot"])
	if err != nil {
		return "", err
	}
	entries = append(entries, orderedJSONEntry{Key: "worktreeRoot", Raw: worktreeRootEncoded})
	sourceEncoded, err := json.Marshal(metadata["source"])
	if err != nil {
		return "", err
	}
	entries = append(entries, orderedJSONEntry{Key: "source", Raw: sourceEncoded})
	return marshalOrderedJSONObject(entries)
}

type orderedJSONEntry struct {
	Key string
	Raw json.RawMessage
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

func currentISO(now func() time.Time) string {
	if now == nil {
		now = time.Now
	}
	return eventlog.FormatJavaScriptISOString(now())
}

func stringPointer(value string) *string {
	return &value
}

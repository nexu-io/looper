package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

// ConfigReloadStatus is transient diagnostic state. The config file overlaid by
// the daemon's startup environment and CLI flags remains the authority for
// global policy; this status only explains whether that authority was applied.
type ConfigReloadStatus struct {
	ConfigPath    string
	Format        string
	FilePresent   bool
	Revision      string
	LastAttemptAt *time.Time
	LastAppliedAt *time.Time
	LastError     string
	RejectedPaths []string
	FieldSources  map[string]config.ValueSource
}

// ConfigReloadError reports a rejected candidate without replacing the
// last-known-good runtime snapshot.
type ConfigReloadError struct {
	Kind  string
	Paths []string
	Err   error
}

type ConfigPatch struct {
	Revision string
	Set      map[string]json.RawMessage
	Unset    []string
}

type ConfigPatchError struct {
	Kind    string
	Message string
	Paths   []string
	Err     error
}

func (e *ConfigPatchError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "configuration update failed"
}

func (e *ConfigPatchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ConfigReloadError) Error() string {
	if e == nil {
		return ""
	}
	// Unstructured parser/decoder errors can quote rejected YAML/TOML scalar
	// values. Keep the public error safe; callers that need classification can
	// inspect Kind and use errors.Unwrap without putting the raw text in logs.
	if e.Kind == "invalid" && e.Err != nil && len(e.Paths) == 0 {
		return "configuration reload rejected: config file could not be decoded or validated"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if len(e.Paths) > 0 {
		return fmt.Sprintf("configuration changes require a daemon restart: %s", strings.Join(e.Paths, ", "))
	}
	return "configuration reload failed"
}

func (e *ConfigReloadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (r *Runtime) startConfigReloadLoop() {
	if r == nil || r.reloadConfig == nil {
		return
	}
	r.configReloadMu.Lock()
	if r.configReloadStop != nil {
		r.configReloadMu.Unlock()
		return
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	r.configReloadStop = stopCh
	r.configReloadDone = doneCh
	r.configReloadMu.Unlock()

	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(r.configReloadInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				_ = r.ReloadConfig(context.Background())
			}
		}
	}()
}

func (r *Runtime) stopConfigReloadLoop() {
	if r == nil {
		return
	}
	r.configReloadMu.Lock()
	stopCh := r.configReloadStop
	doneCh := r.configReloadDone
	r.configReloadStop = nil
	r.configReloadDone = nil
	r.configReloadMu.Unlock()
	if stopCh == nil {
		return
	}
	close(stopCh)
	if doneCh != nil {
		timeout := r.shutdownTimeout
		if timeout <= 0 {
			timeout = time.Second
		}
		select {
		case <-doneCh:
		case <-time.After(timeout):
			if r.logger != nil {
				r.logger.Warn("timed out waiting for configuration reload loop", map[string]any{"timeoutMs": timeout.Milliseconds()})
			}
		}
	}
}

// ReloadConfig reparses the configured source using the exact loader captured
// at bootstrap. A candidate is published atomically only when every changed
// field is hot-safe; invalid and restart-bound candidates leave running work and
// the last-known-good snapshot untouched.
func (r *Runtime) ReloadConfig(ctx context.Context) error {
	if r == nil || r.reloadConfig == nil {
		return &ConfigReloadError{Kind: "unavailable", Err: fmt.Errorf("configuration reload is not configured")}
	}

	r.configReloadMu.Lock()
	defer r.configReloadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return &ConfigReloadError{Kind: "canceled", Err: err}
	}

	now := r.now().UTC()
	r.configReloadStatus.LastAttemptAt = timePointer(now)
	loaded, err := r.reloadConfig()
	if err != nil {
		return r.rejectConfigReloadLocked("invalid", configValidationPaths(err), err)
	}
	if err := ctx.Err(); err != nil {
		return r.rejectConfigReloadLocked("canceled", nil, err)
	}
	return r.applyLoadedConfigLocked(loaded, now)
}

func (r *Runtime) applyLoadedConfigLocked(loaded config.LoadedFileConfig, now time.Time) error {
	restartRequired := config.RestartRequiredChanges(r.loadedConfig.Config, loaded.Config)
	if len(restartRequired) > 0 {
		sort.Strings(restartRequired)
		return r.rejectConfigReloadLocked("restart_required", restartRequired, nil)
	}
	r.configBoundary.Lock()
	defer r.configBoundary.Unlock()
	return r.applyLoadedConfigBoundaryLocked(loaded, now)
}

// applyLoadedConfigBoundaryLocked requires configBoundary's write lock. It is
// split out so a dashboard patch can hold the same publication boundary across
// final validation, rename, and publication.
func (r *Runtime) applyLoadedConfigBoundaryLocked(loaded config.LoadedFileConfig, now time.Time) error {
	runtimeCandidate := loaded.Config
	if r.projectCatalog != nil {
		runtimeCandidate.Projects = r.projectCatalog.Snapshot().Projects
	}
	if err := config.Validate(runtimeCandidate); err != nil {
		return r.rejectConfigReloadLocked("invalid", nil, err)
	}

	changed := !reflect.DeepEqual(r.loadedConfig.Config, loaded.Config)
	r.loadedConfig = loaded
	r.configReloadStatus.ConfigPath = loaded.Metadata.ConfigPath
	r.configReloadStatus.Format = strings.TrimPrefix(strings.ToLower(filepath.Ext(loaded.Metadata.ConfigPath)), ".")
	r.configReloadStatus.FilePresent = loaded.Metadata.ConfigFilePresent
	r.configReloadStatus.Revision = loaded.Metadata.ConfigFileRevision
	r.configReloadStatus.FieldSources = cloneValueSources(loaded.Metadata.FieldSources)
	r.configReloadStatus.LastError = ""
	r.configReloadStatus.RejectedPaths = nil
	if !changed {
		return nil
	}

	if r.projectCatalog != nil {
		r.projectCatalog.PublishGlobals(loaded.Config)
		r.publishCatalogConsumers(r.projectCatalog.Snapshot())
	}
	r.configReloadStatus.LastAppliedAt = timePointer(now)
	if r.logger != nil {
		r.logger.Info("looperd configuration reloaded", map[string]any{
			"configPath": loaded.Metadata.ConfigPath,
		})
	}
	r.TriggerSchedulerTick()
	return nil
}

// PatchConfig applies a targeted mutation to the file layer. It rereads the
// source under the mutation lock, validates a same-directory temporary file
// through the exact startup loader, performs a final identity/mode/byte check,
// then atomically renames it into place. See the final-check comment below for
// the narrow portable-filesystem race that remains.
func (r *Runtime) PatchConfig(ctx context.Context, patch ConfigPatch) error {
	if r == nil || r.reloadConfig == nil || r.loadConfigAt == nil {
		return &ConfigPatchError{Kind: "unavailable", Message: "configuration updates are not available"}
	}
	if len(patch.Set) == 0 && len(patch.Unset) == 0 {
		return &ConfigPatchError{Kind: "validation", Message: "configuration update must set or unset at least one field"}
	}

	r.configReloadMu.Lock()
	defer r.configReloadMu.Unlock()

	paths := make([]string, 0, len(patch.Set)+len(patch.Unset))
	for path := range patch.Set {
		paths = append(paths, strings.TrimSpace(path))
	}
	for _, path := range patch.Unset {
		paths = append(paths, strings.TrimSpace(path))
	}
	sort.Strings(paths)
	for _, path := range paths {
		if !config.IsHotEditablePath(path) {
			return &ConfigPatchError{Kind: "unsupported", Message: fmt.Sprintf("configuration field %q is not hot-editable", path), Paths: []string{path}}
		}
		if !config.IsFieldLevelConfigPath(path) {
			return &ConfigPatchError{Kind: "unsupported", Message: fmt.Sprintf("configuration field %q is not a curated field-level setting", path), Paths: []string{path}}
		}
		source := configValueSourceForPath(r.loadedConfig.Metadata.FieldSources, path)
		if source == config.ValueSourceEnv || source == config.ValueSourceCLI {
			return &ConfigPatchError{Kind: "unsupported", Message: fmt.Sprintf("configuration field %q is controlled by %s and is read-only", path, source), Paths: []string{path}}
		}
	}

	configPath := strings.TrimSpace(r.loadedConfig.Metadata.ConfigPath)
	if configPath == "" {
		configPath = strings.TrimSpace(r.configPath)
	}
	if configPath == "" {
		return &ConfigPatchError{Kind: "unavailable", Message: "the daemon config path is unavailable"}
	}

	originalInfo, inspectedPresent, err := inspectConfigPathForPatch(configPath)
	if err != nil {
		return err
	}
	originalRaw, originalPresent, err := readOptionalConfigFile(configPath)
	if err != nil {
		return &ConfigPatchError{Kind: "unavailable", Message: err.Error(), Err: err}
	}
	afterReadInfo, afterReadPresent, err := inspectConfigPathForPatch(configPath)
	if err != nil {
		return err
	}
	if inspectedPresent != originalPresent || afterReadPresent != originalPresent ||
		(originalPresent && (!os.SameFile(originalInfo, afterReadInfo) || originalInfo.Mode().Perm() != afterReadInfo.Mode().Perm())) {
		return &ConfigPatchError{Kind: "conflict", Message: "configuration changed while it was being read; refresh and try again"}
	}
	if strings.TrimSpace(patch.Revision) == "" {
		return &ConfigPatchError{Kind: "validation", Message: "configuration revision is required"}
	}
	if currentRevision := config.ConfigFileRevision(originalRaw, originalPresent); patch.Revision != currentRevision {
		return &ConfigPatchError{Kind: "conflict", Message: "configuration changed since it was loaded; refresh and try again"}
	}
	partial := config.PartialConfig{}
	if originalPresent {
		partial, err = config.DecodePartialConfigFile(configPath, originalRaw)
		if err != nil {
			return &ConfigPatchError{Kind: "validation", Message: err.Error(), Paths: paths, Err: err}
		}
	}
	updated, err := config.ApplyFieldPatch(partial, patch.Set, patch.Unset)
	if err != nil {
		return &ConfigPatchError{Kind: "validation", Message: err.Error(), Paths: paths, Err: err}
	}
	raw, err := config.MarshalPatchedConfigFile(configPath, originalRaw, originalPresent, updated, paths)
	if err != nil {
		return &ConfigPatchError{Kind: "validation", Message: err.Error(), Paths: paths, Err: err}
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return &ConfigPatchError{Kind: "unavailable", Message: fmt.Sprintf("create config directory: %v", err), Err: err}
	}
	tmpPattern := ".config-reload-*" + filepath.Ext(configPath)
	tmp, err := os.CreateTemp(filepath.Dir(configPath), tmpPattern)
	if err != nil {
		return &ConfigPatchError{Kind: "unavailable", Message: fmt.Sprintf("create temporary config: %v", err), Err: err}
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if originalPresent {
		if chmodErr := tmp.Chmod(originalInfo.Mode().Perm()); chmodErr != nil {
			_ = tmp.Close()
			return &ConfigPatchError{Kind: "unavailable", Message: fmt.Sprintf("preserve config mode: %v", chmodErr), Err: chmodErr}
		}
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return &ConfigPatchError{Kind: "unavailable", Message: fmt.Sprintf("write temporary config: %v", err), Err: err}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return &ConfigPatchError{Kind: "unavailable", Message: fmt.Sprintf("sync temporary config: %v", err), Err: err}
	}
	if err := tmp.Close(); err != nil {
		return &ConfigPatchError{Kind: "unavailable", Message: fmt.Sprintf("close temporary config: %v", err), Err: err}
	}

	candidate, err := r.loadConfigAt(tmpPath)
	if err != nil {
		return &ConfigPatchError{Kind: "validation", Message: err.Error(), Err: err}
	}
	if rejected := config.RestartRequiredChanges(r.loadedConfig.Config, candidate.Config); len(rejected) > 0 {
		sort.Strings(rejected)
		return &ConfigPatchError{Kind: "unsupported", Message: fmt.Sprintf("configuration changes require a daemon restart: %s", strings.Join(rejected, ", ")), Paths: rejected}
	}
	r.configBoundary.Lock()
	defer r.configBoundary.Unlock()
	runtimeCandidate := candidate.Config
	if r.projectCatalog != nil {
		runtimeCandidate.Projects = r.projectCatalog.Snapshot().Projects
	}
	if err := config.Validate(runtimeCandidate); err != nil {
		return &ConfigPatchError{Kind: "validation", Message: err.Error(), Err: err}
	}
	if err := ctx.Err(); err != nil {
		return &ConfigPatchError{Kind: "conflict", Message: err.Error(), Err: err}
	}
	currentInfo, inspectedCurrentPresent, err := inspectConfigPathForPatch(configPath)
	if err != nil {
		return err
	}
	currentRaw, currentPresent, err := readOptionalConfigFile(configPath)
	if err != nil {
		return &ConfigPatchError{Kind: "unavailable", Message: err.Error(), Err: err}
	}
	afterCurrentInfo, afterCurrentPresent, err := inspectConfigPathForPatch(configPath)
	if err != nil {
		return err
	}
	if inspectedCurrentPresent != originalPresent || currentPresent != originalPresent || afterCurrentPresent != originalPresent ||
		(originalPresent && (!os.SameFile(originalInfo, currentInfo) || !os.SameFile(currentInfo, afterCurrentInfo) ||
			originalInfo.Mode().Perm() != currentInfo.Mode().Perm() || currentInfo.Mode().Perm() != afterCurrentInfo.Mode().Perm())) ||
		!bytes.Equal(currentRaw, originalRaw) {
		return &ConfigPatchError{Kind: "conflict", Message: "configuration changed in another editor; refresh and try again"}
	}
	// Rename is atomic, but portable filesystems do not provide a conditional
	// compare-and-rename. The identity/mode/byte check immediately above is the
	// final best-effort conflict check; an editor racing in this tiny interval
	// can still win or be replaced.
	if err := os.Rename(tmpPath, configPath); err != nil {
		return &ConfigPatchError{Kind: "unavailable", Message: fmt.Sprintf("replace config: %v", err), Err: err}
	}
	if err := syncDirectory(filepath.Dir(configPath)); err != nil {
		if r.logger != nil {
			r.logger.Warn("configuration replaced but directory sync failed", map[string]any{"error": err.Error(), "configPath": configPath})
		}
	}
	candidate.Metadata.ConfigPath = configPath
	candidate.Metadata.ConfigFilePresent = true
	now := r.now().UTC()
	r.configReloadStatus.LastAttemptAt = timePointer(now)
	if err := r.applyLoadedConfigBoundaryLocked(candidate, now); err != nil {
		return &ConfigPatchError{Kind: "validation", Message: err.Error(), Err: err}
	}
	return nil
}

func inspectConfigPathForPatch(path string) (os.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, &ConfigPatchError{Kind: "unavailable", Message: fmt.Sprintf("inspect config: %v", err), Err: err}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, true, &ConfigPatchError{Kind: "unsupported", Message: "dashboard updates do not replace symlinked config files; edit the symlink target directly"}
	}
	if !info.Mode().IsRegular() {
		return nil, true, &ConfigPatchError{Kind: "unsupported", Message: "dashboard updates require the config path to be a regular file"}
	}
	return info, true, nil
}

func configValueSourceForPath(sources map[string]config.ValueSource, path string) config.ValueSource {
	for candidate := path; candidate != ""; {
		if source, ok := sources[candidate]; ok {
			return source
		}
		index := strings.LastIndex(candidate, ".")
		if index < 0 {
			break
		}
		candidate = candidate[:index]
	}
	return config.ValueSourceDefault
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readOptionalConfigFile(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return raw, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("read config: %w", err)
}

func (r *Runtime) rejectConfigReloadLocked(kind string, paths []string, cause error) error {
	reloadErr := &ConfigReloadError{Kind: kind, Paths: append([]string(nil), paths...), Err: cause}
	message := configReloadDiagnostic(reloadErr)
	shouldLog := message != r.configReloadStatus.LastError
	r.configReloadStatus.LastError = message
	r.configReloadStatus.RejectedPaths = append([]string(nil), paths...)
	if shouldLog && r.logger != nil {
		r.logger.Warn("looperd configuration reload rejected", map[string]any{
			"error":         message,
			"rejectedPaths": append([]string(nil), paths...),
		})
	}
	return reloadErr
}

func configReloadDiagnostic(reloadErr *ConfigReloadError) string {
	if reloadErr == nil {
		return ""
	}
	return reloadErr.Error()
}

func (r *Runtime) ConfigReloadStatus() ConfigReloadStatus {
	if r == nil {
		return ConfigReloadStatus{}
	}
	r.configReloadMu.Lock()
	defer r.configReloadMu.Unlock()
	return r.configReloadStatusLocked()
}

// ConfigSnapshot returns effective values and reload metadata from one
// configReloadMu generation for API responses.
func (r *Runtime) ConfigSnapshot() (config.Config, ConfigReloadStatus) {
	if r == nil {
		return config.Config{}, ConfigReloadStatus{}
	}
	r.configReloadMu.Lock()
	defer r.configReloadMu.Unlock()
	cfg := r.loadedConfig.Config
	if r.projectCatalog != nil {
		cfg = r.projectCatalog.Snapshot()
	}
	return cfg, r.configReloadStatusLocked()
}

func (r *Runtime) configReloadStatusLocked() ConfigReloadStatus {
	status := r.configReloadStatus
	if status.ConfigPath == "" {
		status.ConfigPath = r.loadedConfig.Metadata.ConfigPath
		status.Format = strings.TrimPrefix(strings.ToLower(filepath.Ext(status.ConfigPath)), ".")
		status.FilePresent = r.loadedConfig.Metadata.ConfigFilePresent
		status.Revision = r.loadedConfig.Metadata.ConfigFileRevision
		status.FieldSources = cloneValueSources(r.loadedConfig.Metadata.FieldSources)
	}
	status.RejectedPaths = append([]string(nil), status.RejectedPaths...)
	status.FieldSources = cloneValueSources(status.FieldSources)
	return status
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func cloneValueSources(source map[string]config.ValueSource) map[string]config.ValueSource {
	if len(source) == 0 {
		return map[string]config.ValueSource{}
	}
	cloned := make(map[string]config.ValueSource, len(source))
	for path, value := range source {
		cloned[path] = value
	}
	return cloned
}

func configValidationPaths(err error) []string {
	var validationErr *config.ConfigValidationError
	if !errors.As(err, &validationErr) || validationErr == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(validationErr.Issues))
	paths := make([]string, 0, len(validationErr.Issues))
	for _, issue := range validationErr.Issues {
		path := strings.TrimSpace(issue.Path)
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

package reviewskills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// ResolveInput selects configured reviewer skills against a prepared worktree.
type ResolveInput struct {
	Mode       config.ReviewerSkillsMode
	Required   []string
	Optional   []string
	Worktree   string
	UserHome   string
	BuiltinDir string // materialized bundle.Dir
}

// ResolveResult is the ordered skill index plus optional refs that were missing.
type ResolveResult struct {
	Entries     []Entry
	Unavailable []string
}

type missingSkillError struct {
	Ref string
}

func (e *missingSkillError) Error() string {
	return fmt.Sprintf("reviewer skill %q not found", e.Ref)
}

func isMissingSkill(err error) bool {
	var missing *missingSkillError
	return errors.As(err, &missing)
}

type skillLayer struct {
	dir    string
	source string
}

type skillLayerIndex struct {
	entries map[string][]Entry
	err     error
}

type skillNameLookup struct {
	names  map[string]bool
	layers map[string]skillLayerIndex
}

// Resolve locates configured reviewer skills. extend always prepends builtin
// looper-review from BuiltinDir/SKILL.md and ignores same-name overlays.
func Resolve(in ResolveInput) (ResolveResult, error) {
	mode := in.Mode
	if mode == "" {
		mode = config.ReviewerSkillsModeExtend
	}
	if mode != config.ReviewerSkillsModeExtend && mode != config.ReviewerSkillsModeReplace {
		return ResolveResult{}, fmt.Errorf("reviewer skills mode %q is invalid", mode)
	}

	var result ResolveResult
	seenReal := make(map[string]int)
	builtinName := ""
	lookup := skillNameLookup{names: make(map[string]bool), layers: make(map[string]skillLayerIndex)}
	for _, refs := range [][]string{in.Required, in.Optional} {
		for _, ref := range refs {
			if ref = strings.TrimSpace(ref); ref != "" && !isPathRef(ref) {
				lookup.names[ref] = true
			}
		}
	}

	if mode == config.ReviewerSkillsModeExtend {
		entry, err := loadBuiltinLooperReview(in.BuiltinDir)
		if err != nil {
			return ResolveResult{}, err
		}
		builtinName = entry.Name
		result.Entries = append(result.Entries, entry)
		seenReal[canonicalSkillPath(entry.Path)] = 0
	}

	add := func(ref string, required bool) error {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			if required {
				return fmt.Errorf("required reviewer skill reference is empty")
			}
			return nil
		}
		entry, err := resolveRef(in, ref, &lookup)
		if err != nil {
			if !required && isMissingSkill(err) {
				result.Unavailable = append(result.Unavailable, ref)
				return nil
			}
			return err
		}
		entry.Required = required
		real := canonicalSkillPath(entry.Path)
		if idx, ok := seenReal[real]; ok {
			if required {
				result.Entries[idx].Required = true
			}
			return nil
		}
		if mode == config.ReviewerSkillsModeExtend && builtinName != "" && entry.Name == builtinName {
			return nil
		}
		seenReal[real] = len(result.Entries)
		result.Entries = append(result.Entries, entry)
		return nil
	}

	for _, ref := range in.Required {
		if err := add(ref, true); err != nil {
			return ResolveResult{}, err
		}
	}
	for _, ref := range in.Optional {
		if err := add(ref, false); err != nil {
			return ResolveResult{}, err
		}
	}
	return result, nil
}

func loadBuiltinLooperReview(builtinDir string) (Entry, error) {
	if strings.TrimSpace(builtinDir) == "" {
		return Entry{}, fmt.Errorf("builtin reviewer skill directory is empty")
	}
	skillPath := filepath.Join(builtinDir, "SKILL.md")
	name, description, err := readSkillFrontmatter(skillPath)
	if err != nil {
		return Entry{}, fmt.Errorf("read builtin reviewer skill: %w", err)
	}
	abs, err := filepath.Abs(skillPath)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Name:        name,
		Description: description,
		Path:        abs,
		Source:      "builtin",
		Required:    true,
	}, nil
}

func resolveRef(in ResolveInput, ref string, lookup *skillNameLookup) (Entry, error) {
	if isPathRef(ref) {
		return resolvePathRef(in, ref)
	}
	return resolveNameRef(in, ref, lookup)
}

func isPathRef(ref string) bool {
	return ref == "~" || strings.HasPrefix(ref, "~/") || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") || strings.HasPrefix(ref, "/") || filepath.IsAbs(ref)
}

func resolvePathRef(in ResolveInput, ref string) (Entry, error) {
	path, err := expandSkillPath(ref, in.Worktree, in.UserHome)
	if err != nil {
		return Entry{}, err
	}
	skillPath, err := skillFileFromPath(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, &missingSkillError{Ref: ref}
		}
		return Entry{}, fmt.Errorf("skill %q at %s is unreadable: %w", ref, path, err)
	}
	name, description, err := readSkillFrontmatter(skillPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, &missingSkillError{Ref: ref}
		}
		return Entry{}, fmt.Errorf("skill %q at %s is unreadable: %w", ref, skillPath, err)
	}
	abs, err := filepath.Abs(skillPath)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Name:        name,
		Description: description,
		Path:        abs,
		Source:      "path",
	}, nil
}

func expandSkillPath(ref, worktree, userHome string) (string, error) {
	if ref == "~" || strings.HasPrefix(ref, "~/") {
		if strings.TrimSpace(userHome) == "" {
			return "", fmt.Errorf("skill path %q: home directory is empty", ref)
		}
		if ref == "~" {
			return userHome, nil
		}
		return filepath.Join(userHome, strings.TrimPrefix(ref, "~/")), nil
	}
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, "/") {
		return ref, nil
	}
	if strings.TrimSpace(worktree) == "" {
		return "", fmt.Errorf("skill path %q: worktree is empty", ref)
	}
	return filepath.Join(worktree, ref), nil
}

func skillFileFromPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Join(path, "SKILL.md"), nil
	}
	return path, nil
}

func resolveNameRef(in ResolveInput, name string, lookup *skillNameLookup) (Entry, error) {
	for _, layer := range nameLayers(in) {
		entry, err := lookupNameAtLayer(layer, name, lookup)
		if err != nil {
			return Entry{}, err
		}
		if entry != nil {
			return *entry, nil
		}
	}
	return Entry{}, &missingSkillError{Ref: name}
}

func nameLayers(in ResolveInput) []skillLayer {
	var layers []skillLayer
	if dir := strings.TrimSpace(in.Worktree); dir != "" {
		layers = append(layers, skillLayer{dir: filepath.Join(dir, ".agents", "skills"), source: "project"})
	}
	if dir := strings.TrimSpace(in.UserHome); dir != "" {
		layers = append(layers, skillLayer{dir: filepath.Join(dir, ".agents", "skills"), source: "user"})
	}
	if dir := strings.TrimSpace(in.BuiltinDir); dir != "" {
		layers = append(layers, skillLayer{dir: dir, source: "builtin"})
	}
	return layers
}

func lookupNameAtLayer(layer skillLayer, name string, lookup *skillNameLookup) (*Entry, error) {
	intended := filepath.Join(layer.dir, name, "SKILL.md")
	if _, err := os.Lstat(intended); err == nil {
		if _, _, readErr := readSkillFrontmatter(intended); readErr != nil {
			return nil, fmt.Errorf("skill %q at %s is unreadable: %w", name, intended, readErr)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("skill %q at %s is unreadable: %w", name, intended, err)
	}

	index, ok := lookup.layers[layer.dir]
	if !ok {
		index = indexSkillLayer(layer, lookup.names)
		lookup.layers[layer.dir] = index
	}
	if index.err != nil {
		return nil, fmt.Errorf("skill %q in %s is unreadable: %w", name, layer.dir, index.err)
	}
	matches := index.entries[name]
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("skill %q is provided by multiple files: %s and %s", name, matches[0].Path, matches[1].Path)
	}
	return &matches[0], nil
}

func indexSkillLayer(layer skillLayer, names map[string]bool) skillLayerIndex {
	index := skillLayerIndex{entries: make(map[string][]Entry)}
	files, err := listSkillFiles(layer.dir)
	if err != nil {
		index.err = err
		return index
	}
	seenReal := make(map[string]struct{})
	for _, file := range files {
		parsedName, description, readErr := readSkillFrontmatter(file)
		if readErr != nil || !names[parsedName] {
			continue
		}
		abs, absErr := filepath.Abs(file)
		if absErr != nil {
			index.err = absErr
			return index
		}
		real := canonicalSkillPath(abs)
		if _, ok := seenReal[real]; ok {
			continue
		}
		seenReal[real] = struct{}{}
		index.entries[parsedName] = append(index.entries[parsedName], Entry{
			Name:        parsedName,
			Description: description,
			Path:        abs,
			Source:      layer.source,
		})
	}
	return index
}

func listSkillFiles(dir string) ([]string, error) {
	var files []string
	root := filepath.Join(dir, "SKILL.md")
	if info, err := os.Lstat(root); err == nil && !info.IsDir() {
		files = append(files, root)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return files, nil
		}
		return nil, err
	}
	for _, ent := range entries {
		childDir := filepath.Join(dir, ent.Name())
		info, statErr := os.Stat(childDir)
		if statErr != nil || !info.IsDir() {
			continue
		}
		child := filepath.Join(childDir, "SKILL.md")
		if skillInfo, skillErr := os.Lstat(child); skillErr == nil && !skillInfo.IsDir() {
			files = append(files, child)
		}
	}
	return files, nil
}

func canonicalSkillPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return real
}

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
		entry, err := resolveRef(in, ref)
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

func resolveRef(in ResolveInput, ref string) (Entry, error) {
	if isPathRef(ref) {
		return resolvePathRef(in, ref)
	}
	return resolveNameRef(in, ref)
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

func resolveNameRef(in ResolveInput, name string) (Entry, error) {
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return Entry{}, fmt.Errorf("skill name %q must be a single directory name; use an explicit path for other locations", name)
	}
	for _, layer := range nameLayers(in) {
		entry, err := lookupNameAtLayer(layer, name)
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

func lookupNameAtLayer(layer skillLayer, name string) (*Entry, error) {
	skillPath := filepath.Join(layer.dir, name, "SKILL.md")
	if layer.source == "builtin" && name == "looper-review" {
		skillPath = filepath.Join(layer.dir, "SKILL.md")
	}
	if _, err := os.Lstat(skillPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skill %q at %s is unreadable: %w", name, skillPath, err)
	}
	parsedName, description, err := readSkillFrontmatter(skillPath)
	if err != nil {
		return nil, fmt.Errorf("skill %q at %s is unreadable: %w", name, skillPath, err)
	}
	if parsedName != name {
		return nil, fmt.Errorf("skill %q at %s has a name mismatch: frontmatter declares %q; use an explicit path to select it", name, skillPath, parsedName)
	}
	abs, err := filepath.Abs(skillPath)
	if err != nil {
		return nil, err
	}
	return &Entry{Name: parsedName, Description: description, Path: abs, Source: layer.source}, nil
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

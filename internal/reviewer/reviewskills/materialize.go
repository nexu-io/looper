package reviewskills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Entry is one skill the reviewer agent must be able to read.
type Entry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Source      string `json:"source"`
	Required    bool   `json:"required"`
}

// Bundle is a temporary materialization of embedded reviewer skills.
type Bundle struct {
	Dir     string
	Entries []Entry
}

// MaterializeBuiltin copies embedded files to a new os.MkdirTemp directory
// ("looper-review-skills-*"). Paths in Entries are absolute. The caller must
// Close() to RemoveAll. Never write into a managed worktree.
func MaterializeBuiltin() (*Bundle, error) {
	dir, err := os.MkdirTemp("", "looper-review-skills-*")
	if err != nil {
		return nil, err
	}
	if err := copyBuiltin(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	skillPath := filepath.Join(dir, "SKILL.md")
	name, description, err := readSkillFrontmatter(skillPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	absSkill, err := filepath.Abs(skillPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &Bundle{
		Dir: absDir,
		Entries: []Entry{{
			Name:        name,
			Description: description,
			Path:        absSkill,
			Source:      "builtin",
			Required:    true,
		}},
	}, nil
}

// Close removes the materialized directory.
func (b *Bundle) Close() error {
	if b == nil || b.Dir == "" {
		return nil
	}
	err := os.RemoveAll(b.Dir)
	b.Dir = ""
	return err
}

func copyBuiltin(dest string) error {
	return fs.WalkDir(builtinFS, "builtin", func(fsPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(fsPath, "builtin")
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := builtinFS.ReadFile(fsPath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func readSkillFrontmatter(skillPath string) (name, description string, err error) {
	data, err := os.ReadFile(skillPath)
	if err != nil {
		return "", "", err
	}
	return parseFrontmatter(string(data))
}

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

func parseFrontmatter(content string) (name, description string, err error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return "", "", fmt.Errorf("skill missing YAML frontmatter")
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("skill frontmatter not terminated")
	}
	var meta skillFrontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &meta); err != nil {
		return "", "", fmt.Errorf("skill frontmatter is invalid YAML: %w", err)
	}
	name = strings.TrimSpace(meta.Name)
	description = strings.TrimSpace(meta.Description)
	if name == "" {
		return "", "", fmt.Errorf("skill frontmatter missing name")
	}
	return name, description, nil
}

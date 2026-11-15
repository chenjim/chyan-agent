// Package skills loads SKILL.md instruction files from ~/.agents/skills and
// the project .agents/skills (project overrides global on name collision).
package skills

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"chyan.tech/chyan/internal/frontmatter"
	"gopkg.in/yaml.v3"
)

// Skill is one loadable skill: its markdown body is injected as instruction.
type Skill struct {
	Name        string
	Description string
	Body        string
	Path        string // absolute SKILL.md path
}

// skillMeta is the SKILL.md frontmatter: name + description only.
type skillMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Load reads <dir>/<name>/SKILL.md from both roots. projectDir may be empty.
// Project skills override global skills of the same name; broken files are
// skipped with a warning. Hidden entries (dot-dirs like .git) are ignored and
// a directory without SKILL.md is treated as a group and scanned one level
// deeper (e.g. devops/my-skill/SKILL.md). The result is sorted by name.
func Load(globalDir, projectDir string) ([]Skill, error) {
	byName := map[string]Skill{}
	for _, root := range []string{globalDir, projectDir} {
		if root == "" {
			continue
		}
		paths, err := skillPaths(root)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			sk, ok := readSkill(path)
			if !ok {
				continue
			}
			byName[sk.Name] = sk
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, sk := range byName {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// skillPaths collects SKILL.md paths under root: one level for skill dirs,
// one extra level for group dirs. Hidden dirs are skipped silently. Only a
// missing root is tolerated; an unreadable root is an error.
func skillPaths(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skills: read %s: %w", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if p, ok := skillFile(root, dir); ok {
			out = append(out, p)
			continue
		}
		// Group directory: look one level deeper, skipping hidden dirs.
		subs, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() || strings.HasPrefix(sub.Name(), ".") {
				continue
			}
			if p, ok := skillFile(root, filepath.Join(dir, sub.Name())); ok {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// skillFile returns the SKILL.md inside dir when it exists and resolves inside
// root. A symlinked file must not pull content from outside the skills tree.
func skillFile(root, dir string) (string, bool) {
	p := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	base := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		base = r
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		log.Printf("skills: skip %s: symlink escapes %s", p, root)
		return "", false
	}
	return p, true
}

// readSkill parses one SKILL.md. ok is false on malformed files. A missing
// file is not worth a log line (it just means "not a skill dir").
func readSkill(path string) (Skill, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("skills: skip %s: %v", path, err)
		}
		return Skill{}, false
	}
	fm, body, err := frontmatter.Split(data)
	if err != nil {
		log.Printf("skills: skip %s: %v", path, err)
		return Skill{}, false
	}
	if fm == nil {
		log.Printf("skills: skip %s: no frontmatter", path)
		return Skill{}, false
	}
	var m skillMeta
	if err := yaml.Unmarshal(fm, &m); err != nil {
		log.Printf("skills: skip %s: %v", path, err)
		return Skill{}, false
	}
	if strings.TrimSpace(m.Name) == "" {
		log.Printf("skills: skip %s: missing name", path)
		return Skill{}, false
	}
	if strings.TrimSpace(body) == "" {
		log.Printf("skills: skip %s: empty body", path)
		return Skill{}, false
	}
	return Skill{Name: m.Name, Description: m.Description, Body: body, Path: path}, true
}

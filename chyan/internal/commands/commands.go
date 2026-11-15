// Package commands loads slash-command prompt templates from
// ~/.agents/commands/*.md and the project .agents/commands/*.md (project
// overrides global on name collision). Frontmatter carries the description;
// the body is a prompt template where $ARGUMENTS is substituted on render.
package commands

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"chyan.tech/chyan/internal/frontmatter"
	"gopkg.in/yaml.v3"
)

// ArgumentsPlaceholder is substituted by Render with the user arguments.
const ArgumentsPlaceholder = "$ARGUMENTS"

// Command is one slash command: its template renders to a user prompt.
type Command struct {
	Name        string // file name without .md
	Description string
	Template    string // body with $ARGUMENTS placeholders
	Path        string // absolute source file path
}

// cmdMeta is the command frontmatter. Only description is required.
type cmdMeta struct {
	Description string `yaml:"description"`
}

// Load reads *.md command files from both roots. projectDir may be empty.
// Project commands override global commands of the same name; broken files
// are skipped with a warning. The result is sorted by name.
func Load(globalDir, projectDir string) ([]Command, error) {
	byName := map[string]Command{}
	for _, root := range []string{globalDir, projectDir} {
		if root == "" {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("commands: read %s: %w", root, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".md") {
				continue
			}
			cmd, ok := readCommand(filepath.Join(root, name), strings.TrimSuffix(name, ".md"))
			if !ok {
				continue
			}
			byName[cmd.Name] = cmd
		}
	}
	out := make([]Command, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Render substitutes every $ARGUMENTS placeholder in the template.
func Render(cmd Command, args string) (string, error) {
	if cmd.Template == "" {
		return "", errors.New("commands: empty template")
	}
	return strings.ReplaceAll(cmd.Template, ArgumentsPlaceholder, args), nil
}

// readCommand parses one command file. ok is false on malformed files.
func readCommand(path, name string) (Command, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("commands: skip %s: %v", path, err)
		return Command{}, false
	}
	fm, body, err := frontmatter.Split(data)
	if err != nil {
		log.Printf("commands: skip %s: %v", path, err)
		return Command{}, false
	}
	if fm == nil {
		log.Printf("commands: skip %s: no frontmatter", path)
		return Command{}, false
	}
	var m cmdMeta
	if err := yaml.Unmarshal(fm, &m); err != nil {
		log.Printf("commands: skip %s: %v", path, err)
		return Command{}, false
	}
	if strings.TrimSpace(body) == "" {
		log.Printf("commands: skip %s: empty template", path)
		return Command{}, false
	}
	return Command{Name: name, Description: m.Description, Template: body, Path: path}, true
}

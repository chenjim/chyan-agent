// Package instructions loads repository and user instruction files and
// renders them as one directive block for the system prompt (FR-7 / D32).
//
// Sources, lowest priority first:
//
//	<global .agents>/AGENTS.md       user-wide rules
//	<workDir>/AGENTS.md              project rules
//	<workDir>/CLAUDE.md              compatibility with Claude Code projects
//	<workDir>/.cursor/rules/*        compatibility with Cursor rules
//
// Later files win when rules conflict. Missing files are skipped: a project
// without instructions must run exactly as before.
package instructions

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Limits keep a runaway instruction file from eating the context budget.
const (
	MaxFileBytes  = 16 << 10
	MaxTotalBytes = 40 << 10
)

// Source is one loaded instruction file.
type Source struct {
	// Path is the absolute path, shown to the model so it can cite the rule.
	Path string
	Body string
}

// Load reads the instruction sources for one workspace. globalAgentsDir is
// the user's ~/.agents root ("" skips the global file); workDir is the tool
// sandbox root. Unreadable files are skipped rather than failing the run:
// instructions are advisory, not a hard dependency.
func Load(globalAgentsDir, workDir string) []Source {
	var out []Source
	add := func(path string) {
		body, err := readCapped(path, MaxFileBytes)
		if err != nil || strings.TrimSpace(body) == "" {
			return
		}
		out = append(out, Source{Path: path, Body: body})
	}
	if globalAgentsDir != "" {
		add(filepath.Join(globalAgentsDir, "AGENTS.md"))
	}
	if workDir != "" {
		add(filepath.Join(workDir, "AGENTS.md"))
		add(filepath.Join(workDir, "CLAUDE.md"))
		// Cursor keeps one file per rule; sorted so the prompt is stable.
		matches, _ := filepath.Glob(filepath.Join(workDir, ".cursor", "rules", "*"))
		sort.Strings(matches)
		for _, path := range matches {
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				continue
			}
			add(path)
		}
	}
	return out
}

// readCapped reads at most limit+1 bytes and truncates the body with a
// marker when the file is larger.
func readCapped(path string, limit int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", err
	}
	body := string(buf)
	if len(buf) > limit {
		body = body[:limit]
		for len(body) > 0 && !utf8.ValidString(body) {
			body = body[:len(body)-1] // do not split a multi-byte rune
		}
		body += "\n[... truncated: instruction file exceeds " + fmt.Sprintf("%d bytes]", limit)
	}
	return body, nil
}

// Render builds the directive block appended to the system prompt. It returns
// "" when there are no sources, so prompt prefixes stay byte-identical for
// projects without instructions.
func Render(sources []Source) string {
	if len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Project instructions\n")
	b.WriteString("The following files are user-provided rules for this workspace. " +
		"Follow them; later files take precedence when they conflict.\n")
	total := 0
	for _, src := range sources {
		body := src.Body
		remain := MaxTotalBytes - total
		if remain <= 0 {
			b.WriteString("\n[instructions truncated: total budget reached]\n")
			break
		}
		if len(body) > remain {
			body = body[:remain] + "\n[... truncated: total instruction budget reached]"
		}
		b.WriteString("\n### ")
		b.WriteString(src.Path)
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n")
		total += len(body)
	}
	return b.String()
}

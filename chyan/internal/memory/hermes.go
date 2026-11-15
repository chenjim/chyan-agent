// Hermes memory import (~/.hermes/memories/MEMORY.md, USER.md): entries are
// text blocks separated by "§" lines. Each becomes one global fact memory
// with source "migrated:hindsight" (store.Memory.Source vocabulary).
package memory

import (
	"context"
	"fmt"
	"os"
	"strings"

	"chyan.tech/chyan/internal/store"
)

// ImportHermes reads one Hermes memory markdown file (MEMORY.md/USER.md) and
// writes every "§"-separated entry as a global fact memory. Returns the
// number of entries migrated. Empty blocks are skipped; no entry is merged
// or rewritten (lossless: body text is preserved verbatim).
func (s *FileStore) ImportHermes(ctx context.Context, path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("memory: read hermes %s: %w", path, err)
	}
	entries := splitHermesEntries(string(data))
	n := 0
	for _, entry := range entries {
		name := hermesName(entry)
		if _, err := s.AddMemory(ctx, store.Memory{
			Kind:    "fact",
			Type:    "semantic",
			Name:    name,
			Content: entry,
			Source:  "migrated:hindsight",
			Status:  StatusActive,
		}); err != nil {
			return n, fmt.Errorf("memory: import %q: %w", name, err)
		}
		n++
	}
	return n, nil
}

// splitHermesEntries splits a Hermes memory file into non-empty blocks.
// A "§" separator may sit on its own line or at a line start.
func splitHermesEntries(text string) []string {
	var out []string
	for _, block := range strings.Split(text, "§") {
		if b := strings.TrimSpace(block); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// hermesName derives a memory name from the entry's first line (up to 60
// runes) so the file list stays scannable; the body keeps the full text.
func hermesName(entry string) string {
	line, _, _ := strings.Cut(entry, "\n")
	line = strings.TrimSpace(line)
	r := []rune(line)
	if len(r) > 60 {
		r = r[:60]
	}
	return string(r)
}

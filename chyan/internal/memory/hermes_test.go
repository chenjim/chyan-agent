package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportHermes: every §-separated entry becomes a global fact memory
// with body preserved verbatim (lossless) and source migrated:hindsight.
func TestImportHermes(t *testing.T) {
	raw := "第一条记忆内容，很长的一行\n§\n\n§\n第二条：工作流铁律\n先 Hindsight 再行动。\n§\n第三条\n"
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(t.TempDir())
	n, err := s.ImportHermes(context.Background(), path)
	if err != nil {
		t.Fatalf("ImportHermes: %v", err)
	}
	if n != 3 {
		t.Fatalf("migrated %d entries, want 3", n)
	}
	got, err := s.SearchMemories(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("stored %d memories, want 3", len(got))
	}
	var bodies []string
	for _, m := range got {
		if m.Source != "migrated:hindsight" {
			t.Errorf("source = %q", m.Source)
		}
		if m.Kind != "fact" || m.Status != StatusActive {
			t.Errorf("kind/status = %q/%q", m.Kind, m.Status)
		}
		bodies = append(bodies, m.Content)
	}
	// Lossless: the multi-line second entry keeps its line break and text.
	found := false
	for _, b := range bodies {
		if strings.Contains(b, "第二条：工作流铁律") && strings.Contains(b, "先 Hindsight 再行动。") {
			found = true
		}
	}
	if !found {
		t.Errorf("multi-line entry not preserved: %q", bodies)
	}
}

// TestSplitHermesEntries covers the separator edge cases.
func TestSplitHermesEntries(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"§\n§\n", 0},
		{"only one", 1},
		{"a\n§\nb", 2},
		{"a§b§c", 3},
	}
	for _, c := range cases {
		if got := splitHermesEntries(c.in); len(got) != c.want {
			t.Errorf("splitHermesEntries(%q) = %d blocks, want %d", c.in, len(got), c.want)
		}
	}
}

// TestHermesName: first line, capped at 60 runes.
func TestHermesName(t *testing.T) {
	if got := hermesName("hello world\nrest"); got != "hello world" {
		t.Errorf("name = %q", got)
	}
	long := strings.Repeat("长", 100)
	if got := []rune(hermesName(long)); len(got) != 60 {
		t.Errorf("long name len = %d, want 60", len(got))
	}
}

// TestImportHermesMissingFile surfaces an error (unlike Load, which is
// tolerant: importing the wrong path must not silently import nothing).
func TestImportHermesMissingFile(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.ImportHermes(context.Background(), filepath.Join(t.TempDir(), "nope.md")); err == nil {
		t.Error("missing file must error")
	}
}

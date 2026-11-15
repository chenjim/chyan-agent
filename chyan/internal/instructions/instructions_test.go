// Instruction-source tests: load order, compatibility files, rendering and
// the size caps that keep a huge file out of the context budget.
package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOrderAndCompatFiles(t *testing.T) {
	global := t.TempDir()
	work := t.TempDir()
	write(t, filepath.Join(global, "AGENTS.md"), "global rule")
	write(t, filepath.Join(work, "AGENTS.md"), "project rule")
	write(t, filepath.Join(work, "CLAUDE.md"), "claude rule")
	write(t, filepath.Join(work, ".cursor", "rules", "b.mdc"), "cursor b")
	write(t, filepath.Join(work, ".cursor", "rules", "a.mdc"), "cursor a")
	write(t, filepath.Join(work, ".cursor", "rules", "notes.txt"), "cursor notes")

	got := Load(global, work)
	var bodies []string
	for _, src := range got {
		bodies = append(bodies, strings.TrimSpace(src.Body))
	}
	want := []string{"global rule", "project rule", "claude rule", "cursor a", "cursor b", "cursor notes"}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("load order = %q, want %q", bodies, want)
	}
}

func TestLoadSkipsMissingAndEmpty(t *testing.T) {
	work := t.TempDir()
	write(t, filepath.Join(work, "AGENTS.md"), "   \n")
	if got := Load(filepath.Join(t.TempDir(), "missing"), work); len(got) != 0 {
		t.Fatalf("sources = %+v, want none", got)
	}
	if got := Load("", t.TempDir()); len(got) != 0 {
		t.Fatalf("sources = %+v, want none", got)
	}
}

func TestRenderBlock(t *testing.T) {
	if got := Render(nil); got != "" {
		t.Fatalf("empty render = %q", got)
	}
	block := Render([]Source{
		{Path: "/w/AGENTS.md", Body: "rule one\n"},
		{Path: "/w/CLAUDE.md", Body: "rule two"},
	})
	for _, want := range []string{
		"## Project instructions", "later files take precedence",
		"### /w/AGENTS.md", "rule one", "### /w/CLAUDE.md", "rule two",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
}

func TestLoadTruncatesLargeFile(t *testing.T) {
	work := t.TempDir()
	big := strings.Repeat("x", MaxFileBytes+100)
	write(t, filepath.Join(work, "AGENTS.md"), big)

	got := Load("", work)
	if len(got) != 1 {
		t.Fatalf("sources = %d, want 1", len(got))
	}
	if !strings.Contains(got[0].Body, "truncated: instruction file exceeds") {
		t.Fatalf("body not truncated (len %d)", len(got[0].Body))
	}
	if len(got[0].Body) > MaxFileBytes+200 {
		t.Fatalf("truncated body still %d bytes", len(got[0].Body))
	}
}

func TestRenderTotalBudget(t *testing.T) {
	body := strings.Repeat("y", MaxFileBytes)
	sources := []Source{
		{Path: "/a", Body: body},
		{Path: "/b", Body: body},
		{Path: "/c", Body: body},
	}
	block := Render(sources)
	if !strings.Contains(block, "budget reached") {
		t.Fatalf("total budget marker missing:\n%.200s", block)
	}
	if len(block) > MaxTotalBytes+4096 {
		t.Fatalf("block = %d bytes, over budget", len(block))
	}
}

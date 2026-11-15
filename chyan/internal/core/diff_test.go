package core

import (
	"strings"
	"testing"
)

// TestPreviewDiffBasic checks the rendered unified diff for an edit.
func TestPreviewDiffBasic(t *testing.T) {
	before := []byte("alpha\nbeta\ngamma\ndelta\nepsilon\n")
	after := []byte("alpha\nbeta\nBETA\ngamma\ndelta\nepsilon\n")
	got := PreviewDiff("edit", "a.txt", before, after)
	want := "--- a/a.txt\n+++ b/a.txt\n@@ -1,5 +1,6 @@\n alpha\n beta\n+BETA\n gamma\n delta\n epsilon\n"
	if got != want {
		t.Errorf("diff mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestPreviewDiffNoChange: identical content yields an empty preview.
func TestPreviewDiffNoChange(t *testing.T) {
	for _, content := range []string{"a\nb\nc\n", "single", ""} {
		if got := PreviewDiff("write", "x.txt", []byte(content), []byte(content)); got != "" {
			t.Errorf("no-change diff = %q, want empty", got)
		}
	}
}

// TestPreviewDiffAddDelete covers pure insert and pure delete, including the
// zero-count hunk anchors for both sides.
func TestPreviewDiffAddDelete(t *testing.T) {
	got := PreviewDiff("write", "new.txt", nil, []byte("one\ntwo\n"))
	if !strings.Contains(got, "@@ -0,0 +1,2 @@\n+one\n+two\n") {
		t.Errorf("add diff:\n%s", got)
	}
	got = PreviewDiff("delete", "old.txt", []byte("one\ntwo\n"), nil)
	if !strings.Contains(got, "@@ -1,2 +0,0 @@\n-one\n-two\n") {
		t.Errorf("delete diff:\n%s", got)
	}
}

// TestPreviewDiffSeparateHunks: changes further apart than 2*ctx land in
// separate hunks; closer ones merge with shared context.
func TestPreviewDiffSeparateHunks(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "line"
	}
	before := []byte(strings.Join(lines, "\n") + "\n")
	lines[1], lines[25] = "changed-early", "changed-late"
	after := []byte(strings.Join(lines, "\n") + "\n")
	got := PreviewDiff("edit", "big.txt", before, after)
	if n := strings.Count(got, "@@ -"); n != 2 {
		t.Errorf("want 2 hunks, got %d:\n%s", n, got)
	}
	lines2 := make([]string, 12)
	for i := range lines2 {
		lines2[i] = "x"
	}
	b1 := []byte(strings.Join(lines2, "\n") + "\n")
	lines2[4], lines2[8] = "a", "b"
	got = PreviewDiff("edit", "near.txt", b1, []byte(strings.Join(lines2, "\n")+"\n"))
	if n := strings.Count(got, "@@ -"); n != 1 {
		t.Errorf("want merged hunk, got %d:\n%s", n, got)
	}
}

// TestPreviewDiffBinary: binary content gets a placeholder, never garbage.
func TestPreviewDiffBinary(t *testing.T) {
	bin := []byte{0x00, 0x01, 0x02, 'a'}
	got := PreviewDiff("write", "blob.bin", nil, bin)
	if !strings.Contains(got, "Binary") {
		t.Errorf("binary placeholder = %q", got)
	}
	got = PreviewDiff("write", "blob.bin", bin, []byte("text\n"))
	if !strings.Contains(got, "Binary") {
		t.Errorf("binary before side = %q", got)
	}
}

// TestPreviewDiffBash: non-file actions carry the command text instead.
func TestPreviewDiffBash(t *testing.T) {
	if got := PreviewDiff("bash", "go test ./...", nil, nil); got != "go test ./..." {
		t.Errorf("bash preview = %q", got)
	}
	if got := PreviewDiff("mcp", "  server_tool  ", nil, nil); got != "server_tool" {
		t.Errorf("mcp preview = %q", got)
	}
}

// TestPreviewDiffHugeFiles: beyond maxLCSCells the diff falls back to a coarse
// whole-block replace and still renders.
func TestPreviewDiffHugeFiles(t *testing.T) {
	n := 1100 // 1100 * 1100 > maxLCSCells (1<<20)
	a, b := make([]string, n), make([]string, n)
	for i := range a {
		a[i] = "aaa"
		b[i] = "bbb"
	}
	got := PreviewDiff("edit", "huge.txt", []byte(strings.Join(a, "\n")+"\n"), []byte(strings.Join(b, "\n")+"\n"))
	if !strings.Contains(got, "-aaa") || !strings.Contains(got, "+bbb") {
		t.Errorf("fallback diff missing content:\n%.200s", got)
	}
}

// TestPreviewDiffEdgeLines covers CRLF-free single lines without trailing
// newline and count-1 range formatting.
func TestPreviewDiffEdgeLines(t *testing.T) {
	got := PreviewDiff("edit", "one.txt", []byte("a"), []byte("b"))
	if !strings.Contains(got, "@@ -1 +1 @@\n-a\n+b\n") {
		t.Errorf("single-line diff:\n%s", got)
	}
}

// TestSplitLinesAndBinary helpers.
func TestSplitLinesAndBinary(t *testing.T) {
	if got := splitLines(nil); got != nil {
		t.Errorf("splitLines(nil) = %v", got)
	}
	if got := splitLines([]byte("a\nb\n")); len(got) != 2 {
		t.Errorf("splitLines = %v", got)
	}
	tail := make([]byte, 9000) // NUL only past the 8000-byte scan window
	for i := 0; i < 8000; i++ {
		tail[i] = 'a'
	}
	if isBinary(tail) {
		t.Errorf("NUL beyond scan window should not trigger binary")
	}
	head := make([]byte, 9000)
	head[10] = 0
	if !isBinary(head) {
		t.Errorf("NUL in scan window should trigger binary")
	}
}

package core

import (
	"context"
	"strings"
	"testing"
)

func TestTruncateCompressor(t *testing.T) {
	c := TruncateCompressor{}
	// A single rune always returns as-is (maxChars floors at 1).
	got, err := c.Compress(context.Background(), []Message{textMessage(RoleUser, "a")}, 0.3)
	if err != nil || got != "a" {
		t.Fatalf("single rune: %q %v", got, err)
	}
	// Long text is truncated to roughly targetRatio and marked.
	long := strings.Repeat("abcd", 100) // 400 chars
	got, err = c.Compress(context.Background(), []Message{textMessage(RoleUser, long)}, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated summary should end with ellipsis: %q", got[len(got)-5:])
	}
	if len([]rune(got)) > 121 { // 400*0.3 + ellipsis
		t.Errorf("truncated length = %d, want <= 121", len([]rune(got)))
	}
	// A ratio >= 1 asks for no reduction: returned as-is without ellipsis.
	got, _ = c.Compress(context.Background(), []Message{textMessage(RoleUser, long)}, 2.0)
	if strings.HasSuffix(got, "…") {
		t.Error("ratio 2.0 should return text as-is, not truncate")
	}
	// Empty input returns empty.
	got, _ = c.Compress(context.Background(), nil, 0.3)
	if got != "" {
		t.Errorf("empty input = %q, want empty", got)
	}
}

func TestHeuristicExtractor(t *testing.T) {
	msgs := []Message{
		textMessage(RoleUser, "please refactor the auth module\nsee docs/architecture.md"),
		textMessage(RoleAssistant, "decision: use Go\n done: extract interfaces\ncorrection: not Rust\nlesson: tools.py blacklist is authoritative"),
		textMessage(RoleUser, "todo: write tests\nactive: wiring the loop\nnext: run coverage"),
	}
	note, err := HeuristicExtractor{}.Extract(context.Background(), msgs)
	if err != nil {
		t.Fatal(err)
	}
	if note.Intent != "please refactor the auth module" {
		t.Errorf("intent = %q", note.Intent)
	}
	if len(note.Decisions) != 1 || note.Decisions[0] != "use Go" {
		t.Errorf("decisions = %v", note.Decisions)
	}
	if len(note.Corrections) != 1 || note.Corrections[0] != "not Rust" {
		t.Errorf("corrections = %v", note.Corrections)
	}
	if len(note.Done) != 1 || note.Done[0] != "extract interfaces" {
		t.Errorf("done = %v", note.Done)
	}
	if len(note.Lessons) != 1 || !strings.Contains(note.Lessons[0], "blacklist") {
		t.Errorf("lessons = %v", note.Lessons)
	}
	if len(note.Active) != 1 || note.Active[0] != "wiring the loop" {
		t.Errorf("active = %v", note.Active)
	}
	// next: from todo: and next: markers.
	if len(note.Next) != 2 {
		t.Errorf("next = %v, want 2 entries", note.Next)
	}
	if len(note.References) == 0 {
		t.Error("references should capture the path mention")
	}
}

func TestHeuristicExtractorEmpty(t *testing.T) {
	note, err := HeuristicExtractor{}.Extract(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if note.Intent != "" || len(note.Decisions) != 0 {
		t.Errorf("empty input should give empty note: %+v", note)
	}
}

func TestLooksLikePath(t *testing.T) {
	cases := []struct {
		tok  string
		want bool
	}{
		{"docs/architecture.md", true},
		{"go.mod", true},
		{"./local.go", true},
		{"https://example.com", false}, // handled by URL branch separately
		{"hello", false},
		{".git", false},
		{".", false},
		{"ab", false},
	}
	for _, tc := range cases {
		if got := looksLikePath(tc.tok); got != tc.want {
			t.Errorf("looksLikePath(%q) = %v, want %v", tc.tok, got, tc.want)
		}
	}
}

func TestExtractRefsCaps(t *testing.T) {
	line := strings.Repeat("a.go ", 10)
	refs := extractRefs(line)
	if len(refs) > 3 {
		t.Errorf("extractRefs caps at 3, got %d", len(refs))
	}
}

func TestHasPrefixFold(t *testing.T) {
	if !hasPrefixFold("DONE: x", "done:") {
		t.Error("case-insensitive prefix should match")
	}
	if hasPrefixFold("notdone", "done:") {
		t.Error("non-prefix must not match")
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("a\nb\nc"); got != "a" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("single"); got != "single" {
		t.Errorf("firstLine = %q", got)
	}
}

func TestJoinNonEmpty(t *testing.T) {
	if got := joinNonEmpty("", "a", "", "b"); got != "a\nb" {
		t.Errorf("joinNonEmpty = %q", got)
	}
	if got := joinNonEmpty("", ""); got != "" {
		t.Errorf("joinNonEmpty empty = %q", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x"); got != "x" {
		t.Errorf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty empty = %q", got)
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"a", "b", "a", "", "c", "b"})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("dedupeStrings = %v", got)
	}
}

func TestRenderNoteSections(t *testing.T) {
	note := &HandoffNote{
		Intent:     "goal",
		Done:       []string{"d1"},
		Lessons:    []string{"l1"},
		Summary:    "sum",
		References: []string{"r1"},
	}
	out := renderNote(note)
	for _, want := range []string{"Intent", "goal", "Done", "d1", "Lessons", "l1", "Summary", "sum", "References", "r1"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderNote missing %q in:\n%s", want, out)
		}
	}
	if renderNote(nil) != "" {
		t.Error("renderNote(nil) should be empty")
	}
}

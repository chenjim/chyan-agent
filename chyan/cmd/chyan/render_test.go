// render_test.go — block splitting and theme handling for the chat markdown
// renderer.
package main

import (
	"regexp"
	"strings"
	"testing"
)

// ansiSeq matches ANSI escape sequences emitted by the renderer (colors,
// chroma token styling). Assertions strip them first so content checks do
// not depend on style internals.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plain(s string) string { return ansiSeq.ReplaceAllString(s, "") }

func TestMDRendererRawPassthrough(t *testing.T) {
	var sb strings.Builder
	r := newMDRenderer(&sb, true)
	md := "# 标题\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	if _, err := r.Write([]byte(md)); err != nil {
		t.Fatal(err)
	}
	if sb.String() != md {
		t.Errorf("raw output must be untouched: %q", sb.String())
	}
}

func TestMDRendererRendersBlocks(t *testing.T) {
	var sb strings.Builder
	r := newMDRenderer(&sb, false)
	// Streaming in small chunks across a table and a code fence.
	chunks := []string{
		"## 磁盘\n\n| 点 | 用量 |\n|---|---|\n", "| / | 63% |\n",
		"\n```bash\necho hi\n```\n", "结尾段落",
	}
	for _, c := range chunks {
		if _, err := r.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	// The closing paragraph has no trailing blank line: it waits for Flush.
	if !r.Pending() {
		t.Error("final paragraph must stay pending until Flush")
	}
	r.Flush()
	if r.Pending() {
		t.Error("Flush must clear the buffer")
	}
	out := plain(sb.String())
	// The raw pipe-syntax must be gone (rendered) and content kept.
	if strings.Contains(out, "```") {
		t.Errorf("fence markers leaked to output:\n%s", out)
	}
	if !strings.Contains(out, "磁盘") || !strings.Contains(out, "63%") || !strings.Contains(out, "echo hi") || !strings.Contains(out, "结尾段落") {
		t.Errorf("content lost during render:\n%s", out)
	}
}

func TestMDRendererFenceKeepsBlockOpen(t *testing.T) {
	var sb strings.Builder
	r := newMDRenderer(&sb, false)
	if _, err := r.Write([]byte("```go\nfmt.Println(1)\n")); err != nil {
		t.Fatal(err)
	}
	if !r.Pending() {
		t.Fatal("open fence must keep the block pending")
	}
	if sb.Len() != 0 {
		t.Errorf("nothing should print before the fence closes: %q", sb.String())
	}
	if _, err := r.Write([]byte("```\n")); err != nil {
		t.Fatal(err)
	}
	if r.Pending() {
		t.Error("closing fence completes the block")
	}
	if !strings.Contains(plain(sb.String()), "fmt.Println") {
		t.Errorf("code block lost: %q", plain(sb.String()))
	}
}

func TestMDRendererTheme(t *testing.T) {
	r := newMDRenderer(&strings.Builder{}, false)
	if !r.setTheme("light") {
		t.Error("light must be a valid theme")
	}
	if r.setTheme("neon") {
		t.Error("unknown theme must be rejected")
	}
	if r.theme != "light" {
		t.Errorf("theme = %s", r.theme)
	}
}

// TestStyleForInlineCodeNotRed: the stock glamour styles color inline code
// bright red ("203"); with replies full of code tokens that reads as a wall
// of errors. Every color style must neutralize it.
func TestStyleForInlineCodeNotRed(t *testing.T) {
	for theme, want := range map[string]string{"dark": "251", "light": "0"} {
		got := styleFor(theme).Code.Color
		if got == nil || *got != want {
			t.Errorf("theme %s inline-code color = %v, want %q", theme, got, want)
		}
	}
	// auto resolves to one of the two stock schemes, terminal-dependent.
	auto := styleFor("auto").Code.Color
	if auto == nil || (*auto != "251" && *auto != "0") {
		t.Errorf("auto inline-code color = %v, want 251 or 0", auto)
	}
}

// render.go — markdown rendering for the chat REPL: replies are streamed
// block by block (paragraph / table / fenced code …) and each complete block
// is rendered with glamour before printing, so long answers stay live but
// come out themed instead of raw markdown source.
package main

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

// mdThemes lists the accepted /theme values.
var mdThemes = map[string]bool{
	"auto":  true, // detect the terminal background (dark or light)
	"dark":  true,
	"light": true,
	"pink":  true,
	"ascii": true, // no colors
}

// mdRenderer buffers one markdown block at a time from the reply stream and
// prints each finished block rendered. A fenced code block is finished at its
// closing fence (never split or double-rendered); a paragraph at a blank
// line; Flush renders whatever is left when the turn ends.
type mdRenderer struct {
	mu      sync.Mutex
	out     io.Writer
	theme   string
	width   int
	raw     bool // non-TTY: pass text through untouched
	pending strings.Builder
}

// newMDRenderer builds a renderer writing to out. When raw is true every
// byte passes through verbatim (pipes, `chyan run`, tests).
func newMDRenderer(out io.Writer, raw bool) *mdRenderer {
	width, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil || width <= 0 {
		width = 80
	}
	if width > 100 {
		width = 100 // keep tables and code blocks readable on wide terminals
	}
	return &mdRenderer{out: out, theme: "auto", width: width, raw: raw}
}

// setTheme switches the render style by name.
func (m *mdRenderer) setTheme(name string) bool {
	if !mdThemes[name] {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.theme = name
	return true
}

// Write accepts one streamed chunk of the reply and prints any blocks that
// became complete.
func (m *mdRenderer) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raw {
		return m.out.Write(p)
	}
	m.pending.Write(p)
	m.drain()
	return len(p), nil
}

// Flush renders whatever block is still pending (end of the reply).
func (m *mdRenderer) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raw {
		return
	}
	m.render(m.pending.String())
	m.pending.Reset()
}

// Pending reports whether an unfinished block is buffered (used to decide
// where progress markers can safely be printed).
func (m *mdRenderer) Pending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending.Len() > 0
}

// drain prints every complete block currently buffered. Blocks end at a
// blank line outside a fence, or at the closing fence of a code block.
func (m *mdRenderer) drain() {
	for {
		s := m.pending.String()
		cut := m.blockEnd(s)
		if cut < 0 {
			return
		}
		m.render(s[:cut])
		m.pending.Reset()
		m.pending.WriteString(s[cut:])
	}
}

// blockEnd returns the end offset of the first complete block in s, or -1
// when the block is still streaming. It rescans the whole buffer each call,
// so fence state must be derived from the scan itself (never persisted).
func (m *mdRenderer) blockEnd(s string) int {
	inFence := false
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '\n' {
			continue
		}
		line := s[start:i]
		end := i + 1
		if end > len(s) {
			end = len(s) // no trailing newline on the last line
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			// fence line: toggle state, block is complete at the closing fence
			inFence = !inFence
			if !inFence {
				return end
			}
		} else if !inFence && strings.TrimSpace(line) == "" && i > 0 {
			return end
		}
		if i < len(s) {
			start = i + 1
		}
	}
	return -1
}

// render prints one markdown block through glamour (falling back to the raw
// block when rendering fails).
func (m *mdRenderer) render(block string) {
	block = strings.TrimLeft(block, "\n")
	if strings.TrimSpace(block) == "" {
		return
	}
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(styleFor(m.theme)),
		glamour.WithWordWrap(m.width),
	)
	out := block
	if err == nil {
		if styled, rerr := renderer.Render(block); rerr == nil {
			out = styled
		}
	}
	out = strings.TrimRight(out, "\n")
	_, _ = io.WriteString(m.out, out+"\n\n")
}

// zeroMargin is shared: each theme config is a value copy whose Document
// margin pointer is then re-pointed here (chat wants flush-left output).
var zeroMargin uint

// styleFor returns the glamour style config for a theme with the left
// document margin removed (chat output is not a slide).
func styleFor(theme string) ansi.StyleConfig {
	var cfg ansi.StyleConfig
	light := false
	switch theme {
	case "light":
		cfg, light = styles.LightStyleConfig, true
	case "pink":
		cfg = styles.PinkStyleConfig
	case "ascii":
		cfg = styles.ASCIIStyleConfig
	case "auto": // follow the terminal's actual background
		light = !termenv.HasDarkBackground()
		if light {
			cfg = styles.LightStyleConfig
		} else {
			cfg = styles.DarkStyleConfig
		}
	default: // dark
		cfg = styles.DarkStyleConfig
	}
	// The stock styles color inline code bright red ("203"); replies are full
	// of code tokens and a red flood reads as errors, not emphasis. Go neutral.
	code := "251" // light gray on the dark code background
	if light {
		code = "0" // near-black on the light code background
	}
	cfg.Code.Color = &code
	cfg.Document.Margin = &zeroMargin
	return cfg
}

// lineinput.go — REPL input lines: bubbletea textinput on a terminal (arrows,
// editing, history, Shift+Tab to toggle the phase, "!" to enter shell mode),
// plain scanning on pipes and tests.
package main

import (
	"bufio"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
)

// paintAnsi wraps s in a 256-color code; raw output (pipes) stays plain.
func paintAnsi(raw bool, code, s string) string {
	if raw {
		return s
	}
	return "\x1b[38;5;" + code + "m" + s + "\x1b[0m"
}

// newLineReader picks the input strategy: line editing on a terminal, plain
// line scanning otherwise (pipes, scripted tests). onToggle fires when the
// user presses Shift+Tab (phase switch) — the edit reader redraws the prompt
// to reflect the new phase without ending the line. completions supplies the
// Tab-completion candidates (nil disables suggestions).
func newLineReader(in io.Reader, raw bool, onToggle func(), completions func() []string) lineReader {
	if f, ok := in.(*os.File); ok && isTerminal(f) {
		return newEditReader(in, raw, onToggle, completions)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &scanReader{sc: sc}
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// editReader is the interactive path: one bubbletea program per input line
// (cursor movement, word/line kill, history via arrows, Shift+Tab to toggle
// phase, "!" to enter shell mode, Tab completes slash commands, Ctrl-C
// cancels the draft, Ctrl-D on an empty line ends the session).
type editReader struct {
	in          io.Reader
	raw         bool
	out         io.Writer
	onToggle    func()
	completions func() []string
	history     []string
}

// newEditReader builds the interactive reader over the given input stream and
// the terminal output.
func newEditReader(in io.Reader, raw bool, onToggle func(), completions func() []string) *editReader {
	return &editReader{in: in, raw: raw, out: os.Stdout, onToggle: onToggle, completions: completions}
}

// ReadLine draws the current prompt (via promptFn) and reads one line.
func (e *editReader) ReadLine(promptFn func() string) (string, error) {
	ti := textinput.New()
	ti.Prompt = promptFn()
	// Slash suggestions: typing "/" narrows the candidate list live; Tab
	// accepts, further Tabs cycle matches. Plain text never matches (every
	// candidate starts with "/").
	if e.completions != nil {
		ti.ShowSuggestions = true
		ti.SetSuggestions(e.completions())
	}
	// Use the terminal's real cursor: bubbletea v2 places it via tea.View.Cursor,
	// so IME preedit (pinyin, kana, hangul) and the candidate window anchor on
	// the edit cell. The virtual reverse-video caret is hidden to avoid doubling.
	ti.SetVirtualCursor(false)
	ti.Focus()
	m := inputModel{ti: ti, promptFn: promptFn, onToggle: e.onToggle,
		raw: e.raw, history: e.history, histIdx: len(e.history), completions: e.completions}
	final, err := tea.NewProgram(m, tea.WithInput(e.in), tea.WithOutput(e.out)).Run()
	if err != nil {
		return "", err
	}
	im := final.(inputModel)
	switch {
	case im.aborted:
		return "", errCanceled
	case im.eof:
		return "", io.EOF
	}
	line := im.ti.Value()
	if im.shell {
		line = "!" + line
	}
	if s := strings.TrimSpace(line); s != "" {
		e.history = append(e.history, s)
	}
	return line, nil
}

func (e *editReader) Close() error { return nil }

// inputModel drives one input line. It owns the textinput widget plus history
// navigation, the Shift+Tab phase toggle and the "!" shell-escape mode.
type inputModel struct {
	ti          textinput.Model
	promptFn    func() string
	onToggle    func()
	completions func() []string // slash-command candidates (nil = none)
	history     []string
	histIdx     int // index into history; len(history) = editing a fresh draft
	draft       string
	aborted     bool
	eof         bool
	shell       bool // "!" escape: prompt turns Shell, Enter submits "!cmd"
	raw         bool // output is not a terminal: prompts stay plain
	width       int  // terminal width in cells, 0 = unknown
	quitting    bool // line submitted or canceled: the cursor is no longer shown
}

// shellPrompt is the shell-escape variant of the phase prompt (yellow bar).
func shellPrompt(raw bool) string {
	return paintAnsi(raw, "184", "▌Shell") + " " + paintAnsi(raw, "244", "›") + " "
}

// enterShell flips into (or out of) shell-escape mode and redraws the prompt.
func (m *inputModel) enterShell(on bool) {
	m.shell = on
	if on {
		m.ti.Prompt = shellPrompt(m.raw)
		return
	}
	m.ti.Prompt = m.promptFn()
}

func (m inputModel) Init() tea.Cmd { return nil }

func (m inputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
	}
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		var cmd tea.Cmd
		m.ti, cmd = m.ti.Update(msg)
		return m, cmd
	}
	switch key.String() {
	case "enter":
		// An empty shell prompt falls back to the phase prompt instead of
		// submitting a blank command.
		if m.shell && strings.TrimSpace(m.ti.Value()) == "" {
			m.enterShell(false)
			return m, nil
		}
		m.finish()
		return m, tea.Quit
	case "ctrl+c":
		m.aborted = true
		m.finish()
		return m, tea.Quit
	case "ctrl+d":
		if m.ti.Value() == "" {
			m.eof = true
			m.finish()
			return m, tea.Quit
		}
	case "shift+tab":
		// Toggle the run phase and redraw the prompt; keep editing the draft.
		// In shell mode the prompt stays the shell one: the line is still a
		// shell command, only the pending phase changes.
		if m.onToggle != nil {
			m.onToggle()
		}
		if m.shell {
			m.ti.Prompt = shellPrompt(m.raw)
		} else {
			m.ti.Prompt = m.promptFn()
		}
		return m, nil
	case "backspace":
		// Deleting at the start of an empty shell draft exits shell mode and
		// restores the previous Build/Plan prompt.
		if m.shell && m.ti.Value() == "" {
			m.enterShell(false)
			return m, nil
		}
	case "up":
		if len(m.history) == 0 {
			return m, nil
		}
		if m.histIdx == len(m.history) {
			m.draft = m.currentLine() // save the in-progress draft (with its "!")
		}
		if m.histIdx > 0 {
			m.histIdx--
		}
		m.restore(m.history[m.histIdx])
		return m, nil
	case "down":
		if m.histIdx < len(m.history) {
			m.histIdx++
			if m.histIdx == len(m.history) {
				m.restore(m.draft)
			} else {
				m.restore(m.history[m.histIdx])
			}
		}
		return m, nil
	}
	// "!" on an empty draft enters shell mode; the bang itself is re-added on
	// submit so history and dispatch keep the real line.
	if !m.shell && key.Text == "!" && m.ti.Value() == "" {
		m.enterShell(true)
		return m, nil
	}
	var cmd tea.Cmd
	m.ti, cmd = m.ti.Update(msg)
	return m, cmd
}

// restore puts a history entry (or the saved draft) back into the editor,
// re-deriving shell mode from its "!" prefix. SetValue does not re-run
// suggestion matching, so the candidate list is refreshed explicitly —
// otherwise Tab after a recall accepts a match from the previous draft.
func (m *inputModel) restore(s string) {
	m.enterShell(strings.HasPrefix(s, "!"))
	m.ti.SetValue(strings.TrimPrefix(s, "!"))
	m.ti.CursorEnd()
	if m.completions != nil {
		m.ti.SetSuggestions(m.completions())
	}
}

// currentLine renders the editor buffer as the dispatched line (with the "!"
// shell prefix, which is stripped from the widget's value).
func (m inputModel) currentLine() string {
	if m.shell {
		return "!" + m.ti.Value()
	}
	return m.ti.Value()
}

// finish marks the line done so the renderer's closing frame does not keep the
// caret parked on the erased line.
func (m *inputModel) finish() { m.quitting = true }

// View renders the editor and hands the caret column to the terminal cursor.
func (m inputModel) View() tea.View {
	if m.aborted {
		return tea.NewView("") // Ctrl-C discards the draft: clear the line
	}
	v := tea.NewView(m.ti.View())
	if m.quitting {
		return v
	}
	if c := m.ti.Cursor(); c != nil {
		c.X = m.caretColumn()
		v.Cursor = c
	}
	return v
}

// caretColumn is the 0-based screen column of the caret: prompt width plus the
// display width of the text before it (CJK glyphs take two cells). textinput's
// own Cursor() counts runes, which drifts on wide characters.
func (m inputModel) caretColumn() int {
	pos, runes := m.ti.Position(), []rune(m.ti.Value())
	if pos < 0 {
		pos = 0
	}
	if pos > len(runes) {
		pos = len(runes)
	}
	col := lipgloss.Width(m.ti.Prompt) + lipgloss.Width(string(runes[:pos]))
	if m.width > 0 && col > m.width-1 { // the renderer clips frames to width
		col = m.width - 1
	}
	return col
}

// scanReader is the non-TTY path: reads lines verbatim (no phase toggle).
type scanReader struct {
	sc *bufio.Scanner
}

func (s *scanReader) ReadLine(func() string) (string, error) {
	if !s.sc.Scan() {
		if err := s.sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return s.sc.Text(), nil
}

func (s *scanReader) Close() error { return nil }

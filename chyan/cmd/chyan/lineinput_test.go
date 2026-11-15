package main

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// newTestModel builds an input model with the given history and callbacks.
func newTestModel(history []string, onToggle func()) inputModel {
	ti := textinput.New()
	ti.SetVirtualCursor(false) // the terminal cursor is the caret (production path)
	ti.Focus()                 // the real reader focuses before Run; keys are ignored otherwise
	return inputModel{ti: ti, promptFn: func() string { return "x> " },
		onToggle: onToggle, history: history, histIdx: len(history)}
}

// runeKey builds a printable key press message.
func runeKey(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: []rune(s)[0], Text: s}
}

func TestInputModelShiftTabToggles(t *testing.T) {
	phase := "exec"
	m := newTestModel(nil, func() {
		if phase == "exec" {
			phase = "plan"
		} else {
			phase = "exec"
		}
	})
	m.promptFn = func() string { return phase + "> " }
	m.ti.Prompt = m.promptFn()

	res, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	im := res.(inputModel)
	if phase != "plan" {
		t.Fatalf("phase = %q, want plan", phase)
	}
	if im.ti.Prompt != "plan> " {
		t.Fatalf("prompt = %q, want plan> ", im.ti.Prompt)
	}
}

// TestInputModelShellEscape: "!" on an empty draft flips to the yellow Shell
// prompt, backspace (or an empty Enter) flips back, and submission re-attaches
// the bang; history entries carrying "!" restore shell mode on recall.
func TestInputModelShellEscape(t *testing.T) {
	m := newTestModel(nil, nil)

	// Enter shell mode: prompt turns Shell, value stays empty (no stray "!").
	res, _ := m.Update(runeKey("!"))
	im := res.(inputModel)
	if !im.shell || im.ti.Value() != "" {
		t.Fatalf("shell = %v value = %q, want true/empty", im.shell, im.ti.Value())
	}
	if !strings.Contains(im.ti.Prompt, "▌Shell") {
		t.Fatalf("prompt = %q, want ▌Shell", im.ti.Prompt)
	}
	// Typing "!" again inside shell mode must not re-toggle — it is command text.
	res, _ = im.Update(runeKey("!"))
	im = res.(inputModel)
	if !im.shell || im.ti.Value() != "!" {
		t.Fatalf("second bang: shell = %v value = %q", im.shell, im.ti.Value())
	}

	// Backspace on the empty draft exits and restores the phase prompt.
	res, _ = m.Update(runeKey("!"))
	im = res.(inputModel)
	res, _ = im.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	im = res.(inputModel)
	if im.shell || im.ti.Prompt != "x> " {
		t.Fatalf("backspace exit: shell = %v prompt = %q", im.shell, im.ti.Prompt)
	}

	// An empty Enter in shell mode also exits instead of submitting.
	res, _ = m.Update(runeKey("!"))
	im = res.(inputModel)
	res, _ = im.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	im = res.(inputModel)
	if im.shell {
		t.Fatal("empty Enter must leave shell mode")
	}
}

// TestShellLineAssembly: shell-mode submission re-attaches "!", and a history
// entry with a bang re-enters shell mode when recalled.
func TestShellLineAssembly(t *testing.T) {
	m := newTestModel(nil, nil)
	res, _ := m.Update(runeKey("!"))
	im := res.(inputModel)
	im.ti.SetValue("ls -la") // simulate the typed command
	line := im.ti.Value()
	if im.shell {
		line = "!" + line
	}
	if line != "!ls -la" {
		t.Fatalf("assembled line = %q, want !ls -la", line)
	}

	// History recall: "!ls -la" restores shell mode and strips the prefix.
	m2 := newTestModel([]string{"!ls -la"}, nil)
	m2.histIdx--
	m2.restore(m2.history[m2.histIdx])
	if !m2.shell || m2.ti.Value() != "ls -la" {
		t.Fatalf("recall: shell = %v value = %q", m2.shell, m2.ti.Value())
	}
}

func TestInputModelHistoryNavigation(t *testing.T) {
	m := newTestModel([]string{"one", "two"}, nil)
	m.ti.SetValue("draft")

	up1, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := up1.(inputModel).ti.Value(); got != "two" {
		t.Errorf("first up = %q, want two", got)
	}
	up2, _ := up1.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := up2.(inputModel).ti.Value(); got != "one" {
		t.Errorf("second up = %q, want one", got)
	}
	dn1, _ := up2.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := dn1.(inputModel).ti.Value(); got != "two" {
		t.Errorf("first down = %q, want two", got)
	}
	dn2, _ := dn1.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := dn2.(inputModel).ti.Value(); got != "draft" {
		t.Errorf("down to draft = %q, want draft", got)
	}
}

// TestInputModelTabCompletesSlash: with suggestions enabled, Tab accepts the
// matching completion ("/se" -> "/sessions"); plain text stays untouched.
// Input arrives as real rune key messages because textinput only refreshes
// its suggestion matches on key events (SetValue alone does not).
func TestInputModelTabCompletesSlash(t *testing.T) {
	typeKeys := func(m inputModel, s string) inputModel {
		for _, r := range s {
			next, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
			m = next.(inputModel)
		}
		return m
	}

	m := newTestModel(nil, nil)
	m.ti.Focus() // textinput drops key events until focused
	m.ti.ShowSuggestions = true
	m.ti.SetSuggestions([]string{"/help", "/sessions", "/status"})
	m = typeKeys(m, "/se")
	res, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := res.(inputModel).ti.Value(); got != "/sessions" {
		t.Errorf("tab completion = %q, want /sessions", got)
	}

	// Plain text has no matching candidate: Tab changes nothing.
	m2 := newTestModel(nil, nil)
	m2.ti.Focus()
	m2.ti.ShowSuggestions = true
	m2.ti.SetSuggestions([]string{"/help", "/sessions"})
	m2 = typeKeys(m2, "fix the bug")
	res2, _ := m2.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := res2.(inputModel).ti.Value(); got != "fix the bug" {
		t.Errorf("plain text must not be completed, got %q", got)
	}
}

func TestInputModelEnterQuits(t *testing.T) {
	m := newTestModel(nil, nil)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter must return a command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("enter must quit the program")
	}
}

func TestInputModelCtrlCAborts(t *testing.T) {
	m := newTestModel(nil, nil)
	res, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !res.(inputModel).aborted {
		t.Fatal("ctrl-c must abort the draft")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl-c must quit the program")
	}
}

func TestInputModelCtrlDEndOfInput(t *testing.T) {
	// Empty input + Ctrl-D ends the session (EOF).
	m := newTestModel(nil, nil)
	res, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	if !res.(inputModel).eof {
		t.Fatal("ctrl-d on empty input must signal EOF")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl-d on empty input must quit")
	}
	// Non-empty input + Ctrl-D does not end the session.
	m2 := newTestModel(nil, nil)
	m2.ti.SetValue("text")
	res2, _ := m2.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	if res2.(inputModel).eof {
		t.Fatal("ctrl-d on non-empty input must not signal EOF")
	}
}

func TestPromptFnAndTogglePhase(t *testing.T) {
	// raw renderer: prompts stay plain text (pipes), colored on a terminal.
	r := &chatRunner{md: newMDRenderer(&strings.Builder{}, true)}
	if got := r.promptFn(); got != "▌Build › " {
		t.Errorf("default prompt = %q, want ▌Build › ", got)
	}
	r.togglePhase()
	if got := r.promptFn(); got != "▌Plan › " {
		t.Errorf("after toggle prompt = %q, want ▌Plan › ", got)
	}
	r.togglePhase()
	if got := r.promptFn(); got != "▌Build › " {
		t.Errorf("after second toggle prompt = %q, want ▌Build › ", got)
	}

	// Terminal output: the phase names carry distinct 256-color codes.
	live := &chatRunner{md: newMDRenderer(&strings.Builder{}, false)}
	if got := live.promptFn(); !strings.Contains(got, "\x1b[38;5;39m▌Build") {
		t.Errorf("terminal Build prompt missing blue bar: %q", got)
	}
	live.togglePhase()
	if got := live.promptFn(); !strings.Contains(got, "\x1b[38;5;215m▌Plan") {
		t.Errorf("terminal Plan prompt missing orange bar: %q", got)
	}
}

// TestCaretColumnCountsWideRunes: CJK glyphs occupy two cells.
func TestCaretColumnCountsWideRunes(t *testing.T) {
	m := newTestModel(nil, nil)
	m.ti.Prompt = "› "
	m.ti.SetValue("你好")
	if got := m.caretColumn(); got != 6 { // 2 (prompt) + 4 (two wide glyphs)
		t.Fatalf("caretColumn = %d, want 6", got)
	}

	// The column follows the cursor, not just the text end.
	m.ti.CursorStart()
	if got := m.caretColumn(); got != 2 {
		t.Fatalf("caret at start = %d, want 2", got)
	}
}

// TestCaretColumnClampsToWidth: a line wider than the terminal is truncated by
// the renderer, so the caret is pinned to the last usable cell.
func TestCaretColumnClampsToWidth(t *testing.T) {
	m := newTestModel(nil, nil)
	m.ti.Prompt = "› "
	m.ti.SetValue("你好")
	m.width = 4
	if got := m.caretColumn(); got != 3 {
		t.Fatalf("clamped caret = %d, want 3", got)
	}
}

// TestViewSetsCursor: the frame carries the caret as tea.View.Cursor (the
// terminal places the real cursor there, anchoring IME preedit and the
// candidate window).
func TestViewSetsCursor(t *testing.T) {
	m := newTestModel(nil, nil)
	m.ti.Prompt = "› "
	m.ti.SetValue("你好")
	v := m.View()
	if v.Cursor == nil {
		t.Fatal("view must carry a cursor")
	}
	if v.Cursor.X != 6 { // 2 (prompt) + 4 (two wide glyphs)
		t.Fatalf("cursor X = %d, want 6", v.Cursor.X)
	}
	if v.Content != m.ti.View() {
		t.Fatalf("content = %q, want textinput view %q", v.Content, m.ti.View())
	}

	// The cursor follows the caret position, not just the text end.
	m.ti.CursorStart()
	if got := m.View().Cursor.X; got != 2 {
		t.Fatalf("cursor at start X = %d, want 2", got)
	}
}

// TestEnterStopsCursor: submitting ends the line, so the closing frame must
// not keep the caret on the erased input row.
func TestEnterStopsCursor(t *testing.T) {
	m := newTestModel(nil, nil)
	res, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	im := res.(inputModel)
	if !im.quitting {
		t.Fatal("enter must mark the line done")
	}
	if v := im.View(); v.Cursor != nil {
		t.Fatalf("quitting view carries a cursor: %+v", v.Cursor)
	}
}

// TestRestoreRefreshesSuggestions: recall goes through SetValue, which does not
// re-match suggestions — Tab would otherwise accept a stale candidate.
func TestRestoreRefreshesSuggestions(t *testing.T) {
	m := newTestModel([]string{"one"}, nil)
	m.completions = func() []string { return []string{"/help", "/status"} }
	m.ti.ShowSuggestions = true
	m.ti.SetSuggestions(m.completions())
	m.restore("/h")
	res, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := res.(inputModel).ti.Value(); got != "/help" {
		t.Fatalf("tab after recall = %q, want /help", got)
	}
}

// TestShiftTabKeepsShellPrompt: toggling the phase mid-shell must not repaint
// the Build/Plan prompt over a line that will be dispatched as a shell command.
func TestShiftTabKeepsShellPrompt(t *testing.T) {
	m := newTestModel(nil, func() {})
	m.enterShell(true)
	res, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if got := res.(inputModel).ti.Prompt; got != shellPrompt(false) {
		t.Fatalf("shell prompt after shift+tab = %q", got)
	}
}

// TestDraftKeepsShellPrefix: the in-progress draft is saved with its "!",
// otherwise returning from history silently leaves shell mode.
func TestDraftKeepsShellPrefix(t *testing.T) {
	m := newTestModel([]string{"one"}, nil)
	m.enterShell(true)
	m.ti.SetValue("ls")
	up, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	down, _ := up.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	im := down.(inputModel)
	if !im.shell || im.ti.Value() != "ls" {
		t.Fatalf("draft restored: shell=%v value=%q", im.shell, im.ti.Value())
	}
}

// TestAbortClearsLine: Ctrl-C discards the draft, so the closing frame is empty
// (the next prompt must not duplicate the abandoned line).
func TestAbortClearsLine(t *testing.T) {
	m := newTestModel(nil, nil)
	m.ti.SetValue("half typed")
	res, _ := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if v := res.(inputModel).View(); v.Content != "" {
		t.Fatalf("aborted view still renders %q", v.Content)
	}
}

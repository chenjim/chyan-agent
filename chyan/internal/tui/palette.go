// palette.go — command palette, open-session tabs and MCP status line (U2).
// The palette is a filterable action list over the panels; tabs keep every
// session opened in this run one keystroke away.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// paletteAction is one command-palette entry: a label the filter matches, the
// equivalent shortcut as a hint, and the action itself.
type paletteAction struct {
	label string
	hint  string
	run   func() tea.Cmd
}

// openPalette resets the palette to its full list and disarms any sub-mode it
// would otherwise hide: an armed rename/filter/delete prompt must not survive
// behind the panel and swallow keys after it closes.
func (m *model) openPalette() {
	m.paletteOpen = true
	m.paletteQuery = ""
	m.paletteCur = 0
	m.renaming = false
	m.renameBuf = ""
	m.searching = false
	m.pending = ""
}

// closePalette dismisses the palette (the action itself already ran or the
// user backed out).
func (m *model) closePalette() {
	m.paletteOpen = false
	m.paletteQuery = ""
	m.paletteCur = 0
}

// actionSessionID is the session a verb applies to: the open transcript wins,
// then the highlighted row in the list.
func (m *model) actionSessionID() string {
	if m.openID != "" {
		return m.openID
	}
	if s := m.selectedSession(); s != nil {
		return s.ID
	}
	return ""
}

// paletteActions builds the entry list. Actions reuse the same helpers as the
// key bindings so both paths cannot drift.
func (m *model) paletteActions() []paletteAction {
	sessionVerb := func(label, hint string, fn func(id string) tea.Cmd) paletteAction {
		return paletteAction{label: label, hint: hint, run: func() tea.Cmd {
			id := m.actionSessionID()
			if id == "" {
				m.notice = "没有可操作的会话"
				return nil
			}
			return fn(id)
		}}
	}
	return []paletteAction{
		{"新会话（回到任务流）", "/new", func() tea.Cmd { m.closeSession(); return nil }},
		{"刷新面板", "ctrl+r", func() tea.Cmd { return m.refreshAll() }},
		{"打开会话", "enter", func() tea.Cmd {
			// The palette is reachable from any panel, so it must not inherit
			// openSelection's "sessions panel only" guard.
			s := m.selectedSession()
			if s == nil {
				m.notice = "没有可操作的会话"
				return nil
			}
			return m.openSession(s.ID)
		}},
		{"下一个会话 tab", "ctrl+→", func() tea.Cmd { return m.switchTab(1) }},
		{"上一个会话 tab", "ctrl+←", func() tea.Cmd { return m.switchTab(-1) }},
		{"关闭当前 tab", "ctrl+w", func() tea.Cmd { return m.closeTab() }},
		sessionVerb("重命名会话", "r", func(id string) tea.Cmd {
			// Renaming is metadata only: switching openID here would show the
			// new title over the previous transcript (and desync the tab bar).
			m.renaming = true
			m.renameBuf = m.titleOf(id)
			return nil
		}),
		sessionVerb("删除会话", "x", func(id string) tea.Cmd {
			m.pending = id
			return nil
		}),
		sessionVerb("导出会话", "e", func(id string) tea.Cmd { return m.cmdExport(id) }),
		{"重载配置（daemon）", "", func() tea.Cmd { return m.cmdReloadConfig() }},
		{"MCP 状态", "", func() tea.Cmd { m.showMCPStatus(); return nil }},
	}
}

// paletteMatches filters the actions by the typed query (label or hint).
func (m *model) paletteMatches() []paletteAction {
	all := m.paletteActions()
	q := strings.ToLower(strings.TrimSpace(m.paletteQuery))
	if q == "" {
		return all
	}
	out := make([]paletteAction, 0, len(all))
	for _, a := range all {
		if strings.Contains(strings.ToLower(a.label), q) ||
			strings.Contains(strings.ToLower(a.hint), q) {
			out = append(out, a)
		}
	}
	return out
}

// paletteKey drives the open palette. Movement is arrow-only: "j"/"k" are
// ordinary characters while filtering.
func (m *model) paletteKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+p":
		m.closePalette()
		return nil
	case "up":
		m.paletteCur = clamp(m.paletteCur-1, 0, max(0, len(m.paletteMatches())-1))
		return nil
	case "down":
		m.paletteCur = clamp(m.paletteCur+1, 0, max(0, len(m.paletteMatches())-1))
		return nil
	case "enter":
		acts := m.paletteMatches()
		if len(acts) == 0 {
			return nil
		}
		a := acts[clamp(m.paletteCur, 0, len(acts)-1)]
		m.closePalette()
		return a.run()
	case "backspace":
		r := []rune(m.paletteQuery)
		m.paletteQuery = string(r[:max(0, len(r)-1)])
		m.paletteCur = 0
		return nil
	}
	if msg.Text != "" {
		m.paletteQuery += msg.Text
		m.paletteCur = 0
	}
	return nil
}

// ------------------------------------------------------------- session tabs

// openSession switches the console to one session, adding a tab when it is
// not open yet.
func (m *model) openSession(id string) tea.Cmd {
	m.openID = id
	m.messages = nil
	m.focus = focusConsole
	m.tabCur = m.addTab(id)
	return m.cmdSession(id)
}

// addTab makes id the active tab (opening it at the end when new) and returns
// its index.
func (m *model) addTab(id string) int {
	for i, t := range m.tabs {
		if t == id {
			return i
		}
	}
	m.tabs = append(m.tabs, id)
	return len(m.tabs) - 1
}

// switchTab moves to the neighbouring tab (+1/-1) and loads its transcript.
func (m *model) switchTab(delta int) tea.Cmd {
	if len(m.tabs) == 0 {
		return nil
	}
	next := clamp(m.tabCur+delta, 0, len(m.tabs)-1)
	if next == m.tabCur && m.openID == m.tabs[next] {
		return nil
	}
	m.tabCur = next
	return m.openSession(m.tabs[next])
}

// closeTab drops the active tab; the neighbour becomes active, or the console
// falls back to the task stream when it was the last one.
func (m *model) closeTab() tea.Cmd {
	if len(m.tabs) == 0 {
		return nil
	}
	idx := clamp(m.tabCur, 0, len(m.tabs)-1)
	closed := m.tabs[idx]
	m.tabs = append(m.tabs[:idx], m.tabs[idx+1:]...)
	wasActive := m.openID == closed
	if len(m.tabs) == 0 {
		m.tabCur = 0
		if wasActive {
			m.closeSession()
		}
		return nil
	}
	m.tabCur = clamp(idx, 0, len(m.tabs)-1)
	if !wasActive {
		// Closing a background tab keeps the transcript on screen; point the
		// cursor back at the tab that is actually open.
		for i, id := range m.tabs {
			if id == m.openID {
				m.tabCur = i
				break
			}
		}
		return nil
	}
	// The closed tab was on screen: show its neighbour.
	return m.openSession(m.tabs[m.tabCur])
}

// dropTab removes a deleted session from the tab bar.
func (m *model) dropTab(id string) {
	idx := -1
	for i, t := range m.tabs {
		if t == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	m.tabs = append(m.tabs[:idx], m.tabs[idx+1:]...)
	if len(m.tabs) == 0 {
		m.tabCur = 0
		return
	}
	m.tabCur = clamp(m.tabCur, 0, len(m.tabs)-1)
}

// titleOf resolves a session label for the rename prompt.
func (m *model) titleOf(id string) string {
	for _, s := range m.sessions {
		if s.ID == id {
			return s.Title
		}
	}
	return ""
}

// showMCPStatus prints the per-server state on the notice line (one line, so
// a stale MCP warning cannot pile up).
func (m *model) showMCPStatus() {
	if len(m.status.MCPServers) == 0 {
		m.notice = "MCP：未配置服务器"
		return
	}
	parts := make([]string, 0, len(m.status.MCPServers))
	for _, s := range m.status.MCPServers {
		state := "ok"
		if !s.Available {
			state = "down"
		}
		parts = append(parts, fmt.Sprintf("%s %s(%d)", s.Name, state, s.Tools))
	}
	m.notice = "MCP：" + strings.Join(parts, " · ")
}

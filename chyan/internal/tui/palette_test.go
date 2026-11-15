// palette_test.go — command palette, session tabs and MCP status (U2).
package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"chyan.tech/chyan/internal/api"
)

// openTab opens the session at row idx (sessions panel focused) and returns
// the command that loads its transcript.
func openTab(t *testing.T, m *model, idx int) tea.Cmd {
	t.Helper()
	m.focus = focusSessions
	m.sessionCur = idx
	return runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
}

func TestPaletteOpensFiltersAndRuns(t *testing.T) {
	m, f := newTestModel()
	runMsg(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if !m.paletteOpen {
		t.Fatal("ctrl+p must open the palette")
	}
	if !strings.Contains(m.View().Content, "命令面板") {
		t.Fatalf("view has no palette:\n%s", m.View().Content)
	}
	if strings.Contains(m.View().Content, "Console —") {
		t.Fatal("palette must replace the console")
	}
	// The filter narrows on the label; j/k are ordinary characters here.
	for _, r := range "导出" {
		runMsg(t, m, key(string(r)))
	}
	if acts := m.paletteMatches(); len(acts) != 1 || !strings.Contains(acts[0].label, "导出") {
		t.Fatalf("matches = %+v", acts)
	}
	cmd := runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.paletteOpen {
		t.Fatal("enter must close the palette")
	}
	if cmd == nil {
		t.Fatal("export action returned no command")
	}
	t.Chdir(t.TempDir()) // export writes next to the process cwd
	runMsg(t, m, cmd())
	if len(f.exports) != 1 || f.exports[0] != "ses1" {
		t.Fatalf("exports = %v", f.exports)
	}
}

func TestPaletteEscAndNoMatch(t *testing.T) {
	m, _ := newTestModel()
	m.openPalette()
	for _, r := range "zzz" {
		runMsg(t, m, key(string(r)))
	}
	if len(m.paletteMatches()) != 0 {
		t.Fatalf("matches = %+v", m.paletteMatches())
	}
	// enter on an empty list is a no-op, not a panic.
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.paletteOpen || m.paletteQuery != "" {
		t.Fatalf("esc must close: open=%v query=%q", m.paletteOpen, m.paletteQuery)
	}
}

func TestPaletteRenameUsesOpenSession(t *testing.T) {
	m, _ := newTestModel()
	openTab(t, m, 1) // ses2
	runMsg(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	for _, r := range "重命名" {
		runMsg(t, m, key(string(r)))
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.renaming || m.renameBuf != "t2" {
		t.Fatalf("palette rename: renaming=%v buf=%q", m.renaming, m.renameBuf)
	}
}

// TestPaletteRenameDoesNotSwitchContext: renaming never switches the console.
// With a transcript open it targets that session; with the task stream showing
// it targets the highlighted row and leaves openID empty.
func TestPaletteRenameDoesNotSwitchContext(t *testing.T) {
	m, _ := newTestModel()
	openTab(t, m, 0) // ses1 open
	m.focus = focusSessions
	m.sessionCur = 1 // highlight ses2
	m.openPalette()
	for _, r := range "重命名" {
		runMsg(t, m, key(string(r)))
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.openID != "ses1" || m.renameBuf != "t1" {
		t.Fatalf("open transcript wins: openID=%q buf=%q", m.openID, m.renameBuf)
	}
	// Back on the task stream the highlighted row is the target, and the
	// console must stay on the stream instead of adopting that session.
	m.renaming = false
	m.closeSession()
	m.openPalette()
	for _, r := range "重命名" {
		runMsg(t, m, key(string(r)))
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.openID != "" || m.renameBuf != "t2" {
		t.Fatalf("openID=%q buf=%q, want no context switch", m.openID, m.renameBuf)
	}
	if m.tabCur != 0 || len(m.tabs) != 1 {
		t.Fatalf("tabs = %+v cur=%d, want the tab bar untouched", m.tabs, m.tabCur)
	}
}

// TestPaletteOpenSessionFromConsole: the palette is reachable from any panel, so
// "打开会话" must not depend on the sessions panel having focus.
func TestPaletteOpenSessionFromConsole(t *testing.T) {
	m, _ := newTestModel()
	m.focus = focusConsole
	m.openPalette()
	for _, r := range "打开会话" {
		runMsg(t, m, key(string(r)))
	}
	cmd := runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.openID != "ses1" || cmd == nil {
		t.Fatalf("openID = %q cmd = %v", m.openID, cmd)
	}
	if len(m.tabs) != 1 || m.tabs[0] != "ses1" {
		t.Fatalf("tabs = %+v", m.tabs)
	}
}

// TestPaletteDisarmsSubModes: ctrl+p during a rename/filter/delete confirmation
// must cancel that prompt, or its keys would be swallowed after the panel closes.
func TestPaletteDisarmsSubModes(t *testing.T) {
	m, _ := newTestModel()
	m.renaming = true
	m.renameBuf = "t1"
	m.searching = true
	m.filter = "x"
	m.pending = "ses1"
	runMsg(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if m.renaming || m.renameBuf != "" || m.searching || m.pending != "" {
		t.Fatalf("sub-modes survived: renaming=%v buf=%q searching=%v pending=%q",
			m.renaming, m.renameBuf, m.searching, m.pending)
	}
	// The filter is a view state, not an input mode, and stays as it was.
	if m.filter != "x" {
		t.Fatalf("filter = %q, want it kept", m.filter)
	}
	// Closing the palette returns to normal typing, not the old prompt.
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	runMsg(t, m, key("a"))
	if m.input != "a" {
		t.Fatalf("input = %q, want normal console typing", m.input)
	}
}

func TestTabsOpenSwitchAndClose(t *testing.T) {
	m, _ := newTestModel()
	openTab(t, m, 0) // ses1
	if len(m.tabs) != 1 || m.openID != "ses1" || m.tabCur != 0 {
		t.Fatalf("tabs = %+v open=%q cur=%d", m.tabs, m.openID, m.tabCur)
	}
	openTab(t, m, 1) // ses2 in a second tab
	if len(m.tabs) != 2 || m.openID != "ses2" || m.tabCur != 1 {
		t.Fatalf("tabs = %+v open=%q cur=%d", m.tabs, m.openID, m.tabCur)
	}
	// The tab bar rides the console and marks the active tab.
	m.width, m.height = 100, 30
	if bar := m.renderTabs(100); !strings.Contains(bar, "[2 ses2]") || !strings.Contains(bar, "1 ses1") {
		t.Fatalf("tab bar = %q", bar)
	}
	// ctrl+← switches to the previous tab and reloads its transcript.
	m.focus = focusConsole
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl})
	if m.openID != "ses1" || m.tabCur != 0 {
		t.Fatalf("switch: open=%q cur=%d", m.openID, m.tabCur)
	}
	// ctrl+w closes the active tab and activates its neighbour.
	runMsg(t, m, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if len(m.tabs) != 1 || m.openID != "ses2" {
		t.Fatalf("close: tabs=%+v open=%q", m.tabs, m.openID)
	}
	// Closing the last tab falls back to the task stream.
	runMsg(t, m, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if len(m.tabs) != 0 || m.openID != "" {
		t.Fatalf("last close: tabs=%+v open=%q", m.tabs, m.openID)
	}
}

func TestOpeningSameSessionReusesTab(t *testing.T) {
	m, _ := newTestModel()
	openTab(t, m, 0)
	openTab(t, m, 0)
	if len(m.tabs) != 1 {
		t.Fatalf("tabs = %+v, want one entry", m.tabs)
	}
}

func TestDeletedSessionLeavesTabs(t *testing.T) {
	m, f := newTestModel()
	openTab(t, m, 0)
	m.focus = focusSessions
	runMsg(t, m, key("x"))
	runMsg(t, m, runMsg(t, m, key("y"))())
	if len(f.deleted) != 1 || len(m.tabs) != 0 {
		t.Fatalf("deleted=%v tabs=%+v", f.deleted, m.tabs)
	}
}

func TestMCPStatusSegmentAndNotice(t *testing.T) {
	m, _ := newTestModel()
	if seg := mcpSegment(m.status); seg != "" {
		t.Fatalf("segment without servers = %q", seg)
	}
	m.status = api.Status{MCPServers: []api.MCPStatus{
		{Name: "search", Available: true, Tools: 3},
		{Name: "playwright", Available: false, Tools: 0},
	}}
	m.width = 200
	if seg := mcpSegment(m.status); seg != "mcp 1/2 · 3 tools" {
		t.Fatalf("segment = %q", seg)
	}
	if !strings.Contains(m.renderStatus(), "mcp 1/2") {
		t.Fatalf("status = %q", m.renderStatus())
	}
	m.showMCPStatus()
	if !strings.Contains(m.notice, "search ok(3)") || !strings.Contains(m.notice, "playwright down(0)") {
		t.Fatalf("notice = %q", m.notice)
	}
	// No servers configured: the palette action says so instead of printing "".
	m.status = api.Status{}
	m.showMCPStatus()
	if !strings.Contains(m.notice, "未配置") {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestPaletteVerbWithoutSession(t *testing.T) {
	m, _ := newTestModel()
	m.sessions = nil
	m.openPalette()
	for _, r := range "删除" {
		runMsg(t, m, key(string(r)))
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pending != "" || !strings.Contains(m.notice, "没有可操作的会话") {
		t.Fatalf("pending=%q notice=%q", m.pending, m.notice)
	}
}

// TestPaletteReloadsDaemonConfig: the palette carries the daemon-side reload
// (U3) and reports the applied model/mode on the status line.
func TestPaletteReloadsDaemonConfig(t *testing.T) {
	m, f := newTestModel()
	m.openPalette()
	for _, r := range "重载" {
		runMsg(t, m, key(string(r)))
	}
	cmd := runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("reload action returned no command")
	}
	runMsg(t, m, cmd())
	if f.reloads != 1 {
		t.Fatalf("reload calls = %d", f.reloads)
	}
	if !strings.Contains(m.notice, "配置已重载") || !strings.Contains(m.notice, "工具 18") {
		t.Fatalf("notice = %q", m.notice)
	}
	// A busy daemon surfaces as an error, not a fake success.
	f.err = errors.New("a run is in flight")
	m.openPalette()
	for _, r := range "重载" {
		runMsg(t, m, key(string(r)))
	}
	runMsg(t, m, runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})())
	if !strings.Contains(m.err, "in flight") {
		t.Fatalf("err = %q", m.err)
	}
}

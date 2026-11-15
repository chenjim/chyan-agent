// view.go — terminal rendering: panel layout, status bar and diff preview.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"chyan.tech/chyan/internal/api"
)

// diffPage is the scroll step for the approval diff preview.
const diffPage = 10

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleCursor = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleSel    = lipgloss.NewStyle().Background(lipgloss.Color("236"))
)

// View implements tea.Model.
func (m *model) View() tea.View {
	if m.quitting {
		return tea.View{}
	}
	bodyH := m.height - 2 // status bar + help line
	if bodyH < 6 {
		bodyH = 6
	}
	leftW := m.width * 3 / 10
	if leftW < 24 {
		leftW = 24
	}
	rightW := m.width - leftW - 1
	if rightW < 24 {
		rightW = 24
	}
	topH := bodyH / 2
	if topH < 6 {
		topH = 6
	}
	bottomH := bodyH - topH

	left := m.renderSessions(leftW, topH)
	right := m.renderApprovals(rightW, topH)
	bottom := m.renderConsole(m.width, bottomH)
	if m.paletteOpen {
		bottom = m.renderPalette(m.width, bottomH)
	}

	top := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
	return tea.NewView(strings.Join([]string{
		m.renderStatus(),
		top,
		bottom,
		m.renderHelp(),
	}, "\n"))
}

// renderStatus is the one-line daemon/observability bar (api.Status), plus the
// live context window / usage of the most recent session and the last CRUD
// confirmation (U1/U2).
func (m *model) renderStatus() string {
	s := m.status
	line := fmt.Sprintf("chyan · v%s · sessions %d · running %d · pending %d · mode %s",
		orDash(s.Version), s.Sessions, s.RunningTasks, s.PendingApprovals, orDash(s.ReviewMode))
	if pct, ok := contextPercent(s); ok {
		line += fmt.Sprintf(" · ctx %.0f%% %s %s/%s",
			pct, usageBar(pct), compactTokens(s.FixedCost+s.HistoryTokens), compactTokens(s.ContextLimit))
	}
	if seg := mcpSegment(s); seg != "" {
		line += " · " + seg
	}
	if m.notice != "" {
		line += "  " + m.notice
	}
	if m.err != "" {
		line += "  " + styleErr.Render("✗ "+m.err)
	}
	return styleDim.Render(truncate(line, m.width))
}

// contextPercent reports the live window fill for the status bar. A daemon
// that never reported a limit (no session yet) yields ok=false.
func contextPercent(s api.Status) (float64, bool) {
	if s.ContextLimit <= 0 {
		return 0, false
	}
	used := s.FixedCost + s.HistoryTokens
	return float64(used) / float64(s.ContextLimit) * 100, true
}

// usageBar draws a compact 10-cell fill bar for a percentage.
func usageBar(pct float64) string {
	const cells = 10
	filled := int(clamp(int(pct+0.5), 0, 100) * cells / 100)
	return strings.Repeat("▮", filled) + strings.Repeat("▯", cells-filled)
}

// compactTokens renders a token count as 999 / 4.7k / 128k / 1.0M.
func compactTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// mcpSegment summarizes MCP health for the status bar ("mcp 2/3" + tool count);
// empty when no server is configured.
func mcpSegment(s api.Status) string {
	if len(s.MCPServers) == 0 {
		return ""
	}
	ok, tools := 0, 0
	for _, sv := range s.MCPServers {
		if sv.Available {
			ok++
		}
		tools += sv.Tools
	}
	return fmt.Sprintf("mcp %d/%d · %d tools", ok, len(s.MCPServers), tools)
}

// renderTabs draws the open-session tab bar; "" when nothing was opened yet.
func (m *model) renderTabs(width int) string {
	if len(m.tabs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.tabs))
	for i, id := range m.tabs {
		label := fmt.Sprintf("%d %s", i+1, shortSessionID(id))
		if i == m.tabCur && m.openID == id {
			parts = append(parts, styleSel.Render("["+label+"]"))
		} else {
			parts = append(parts, styleDim.Render(" "+label+" "))
		}
	}
	return truncate(strings.Join(parts, " "), width)
}

// renderPalette draws the command palette in place of the console (U2).
func (m *model) renderPalette(width, height int) string {
	b := &strings.Builder{}
	b.WriteString(styleTitle.Render("命令面板") + " " + styleDim.Render("ctrl+p 关闭 · ↑↓ 选择 · enter 执行") + "\n")
	b.WriteString(styleDim.Render("▸ ") + truncate(m.paletteQuery+"_", max(0, width-2)) + "\n")
	acts := m.paletteMatches()
	if len(acts) == 0 {
		b.WriteString(styleDim.Render("  (无匹配动作)") + "\n")
	}
	maxLines := height - 3
	for i, a := range acts {
		if i >= maxLines {
			break
		}
		line := fmt.Sprintf("%s %s", selCursor(i == m.paletteCur), truncate(a.label, width-16))
		if a.hint != "" {
			line += " " + styleDim.Render(a.hint)
		}
		if i == m.paletteCur {
			line = styleSel.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return fit(b.String(), width, height)
}

// renderSessions draws the session list panel with the "/" filter line and the
// inline rename / delete prompts (U1).
func (m *model) renderSessions(width, height int) string {
	b := &strings.Builder{}
	b.WriteString(styleTitle.Render("Sessions") + "\n")
	list := m.visibleSessions()
	switch {
	case m.renaming:
		b.WriteString(styleDim.Render("rename ▸ ") + truncate(m.renameBuf+"_", width-9) + "\n")
	case m.searching:
		b.WriteString(styleDim.Render("/ ") + truncate(m.filter+"_", width-2) + "\n")
	case m.pending != "":
		b.WriteString(styleErr.Render("删除 "+shortSessionID(m.pending)+"？ y/n") + "\n")
	case m.filter != "":
		b.WriteString(styleDim.Render(fmt.Sprintf("/%s (%d/%d)  esc 清除", m.filter, len(list), len(m.sessions))) + "\n")
	}
	if len(list) == 0 {
		hint := "  (none — r refresh)"
		if m.filter != "" {
			hint = "  (no match)"
		}
		b.WriteString(styleDim.Render(hint) + "\n")
	}
	maxLines := height - 2
	for i, s := range list {
		if i >= maxLines {
			break
		}
		title := orDash(s.Title)
		if title == "" {
			title = s.ID
		}
		line := fmt.Sprintf("%s %s %s", selCursor(i == m.sessionCur), truncate(title, width-14), styleDim.Render(costTag(s)))
		if i == m.sessionCur {
			line = styleSel.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return fit(b.String(), width, height)
}

// costTag is the per-row usage badge: output tokens and cost when the daemon
// reported them, empty otherwise (a brand-new session has neither).
func costTag(s api.SessionSummary) string {
	if s.TokensOut == 0 && s.CostUSD == 0 {
		return ""
	}
	if s.CostUSD > 0 {
		return fmt.Sprintf("%s/$%.3f", compactTokens(s.TokensOut), s.CostUSD)
	}
	return compactTokens(s.TokensOut)
}

// shortSessionID keeps the panel readable while still identifying the row.
func shortSessionID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:10] + "…"
}

// renderApprovals draws the approval panel: ticket list + diff preview.
func (m *model) renderApprovals(width, height int) string {
	b := &strings.Builder{}
	b.WriteString(styleTitle.Render(fmt.Sprintf("Approvals (%d pending)", len(m.approvals))) + "\n")
	listH := min(len(m.approvals)+1, height/2)
	for i, t := range m.approvals {
		if i >= listH-1 {
			break
		}
		line := fmt.Sprintf("%s [%s] %s %s", selCursor(i == m.approvalCur),
			t.Action, truncate(t.Resource, width-10), verdictTag(t.Verdict))
		if i == m.approvalCur {
			line = styleSel.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if len(m.approvals) == 0 {
		b.WriteString(styleDim.Render("  (no pending tickets)") + "\n")
	}
	t := m.selectedTicket()
	if t != nil && t.Diff != "" {
		diffH := height - listH - 2
		if diffH > 0 {
			lines := strings.Split(t.Diff, "\n")
			start := clamp(m.diffScroll, 0, max(0, len(lines)-diffH))
			b.WriteString(styleDim.Render("── diff preview (pgup/pgdn) ") + strings.Repeat("─", max(0, width-27)) + "\n")
			for _, ln := range lines[start:min(len(lines), start+diffH)] {
				b.WriteString(truncate(ln, width) + "\n")
			}
		}
	}
	return fit(b.String(), width, height)
}

// renderConsole draws the task stream / session transcript and the input.
func (m *model) renderConsole(width, height int) string {
	b := &strings.Builder{}
	title := "Console — task stream"
	if m.openID != "" {
		title = "Console — session " + m.openID
	}
	b.WriteString(styleTitle.Render(title) + "\n")

	// Tab bar (U2): one line only when sessions were opened in this run.
	chrome := 3 // title + input + prompt line
	if bar := m.renderTabs(width); bar != "" {
		b.WriteString(bar + "\n")
		chrome++
	}
	stream := m.streamLines()
	streamH := height - chrome
	if streamH < 1 {
		streamH = 1
	}
	start := clamp(len(stream)-streamH-m.streamScroll, 0, max(0, len(stream)-streamH))
	end := min(len(stream), start+streamH)
	for _, ln := range stream[start:end] {
		b.WriteString(ln + "\n")
	}
	b.WriteString(styleDim.Render("> ") + truncate(m.input, width-2) + "\n")
	return fit(b.String(), width, height)
}

// streamLines returns the visible stream content: session transcript when a
// session is open, the live task stream otherwise.
func (m *model) streamLines() []string {
	if m.openID == "" {
		return m.stream
	}
	out := make([]string, 0, len(m.messages))
	for _, msg := range m.messages {
		out = append(out, fmt.Sprintf("[%s] %s", msg.Role, msg.Text))
		for _, tc := range msg.ToolCalls {
			out = append(out, "  → "+tc)
		}
	}
	return out
}

// renderHelp is the one-line key legend.
func (m *model) renderHelp() string {
	return styleDim.Render(truncate(
		"ctrl+p 命令面板 · tab 切面板 · ↑↓ 选择 · enter 打开 · / 过滤 · r 改名 · x 删除 · e 导出 · ctrl+←/→ 切 tab · ctrl+w 关 tab · a 批准 · d 拒绝 · ctrl+x 中止 · ctrl+r 刷新 · q 退出",
		m.width))
}

// --------------------------------------------------------------- helpers

// selCursor marks the selected row.
func selCursor(sel bool) string {
	if sel {
		return styleCursor.Render("▸")
	}
	return " "
}

// verdictTag colors the AI reviewer hint.
func verdictTag(v string) string {
	if v == "" {
		return ""
	}
	return styleDim.Render("(" + v + ")")
}

// orDash renders an empty string as "-".
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// truncate cuts s to at most width display cells and appends an ellipsis.
// Counting cells instead of runes keeps CJK titles and diffs inside the panel:
// two wide glyphs occupy four cells, not two.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", "⏎")
	if lipgloss.Width(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// fit pads or trims a block to exactly height lines, each clipped to width.
func fit(block string, width, height int) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	out := make([]string, 0, height)
	for i := 0; i < height; i++ {
		if i < len(lines) {
			out = append(out, truncate(lines[i], width))
		} else {
			out = append(out, "")
		}
	}
	return strings.Join(out, "\n")
}

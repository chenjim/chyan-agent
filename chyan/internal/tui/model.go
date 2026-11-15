// model.go — TUI state machine: panels, selections, keys and async messages.
package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"chyan.tech/chyan/internal/api"
)

// maxTitleRunes mirrors the daemon's rename cap (20 runes); the inline prompt
// stops accepting input there instead of letting the server reject it.
const maxTitleRunes = 20

// focus identifies the active panel.
type focus int

const (
	focusSessions focus = iota
	focusApprovals
	focusConsole
)

// focusCount is the number of tab-cyclable panels.
const focusCount = 3

func (f focus) String() string {
	switch f {
	case focusSessions:
		return "sessions"
	case focusApprovals:
		return "approvals"
	default:
		return "console"
	}
}

// daemonAPI is the REST surface the UI needs (implemented by *Client).
type daemonAPI interface {
	ListSessions(ctx context.Context) ([]api.SessionSummary, error)
	GetSession(ctx context.Context, id string) (api.Session, error)
	RenameSession(ctx context.Context, id, title string) (api.SessionSummary, error)
	DeleteSession(ctx context.Context, id string) error
	ExportSession(ctx context.Context, id string) ([]byte, error)
	CreateTask(ctx context.Context, req api.CreateTaskRequest) (api.Task, error)
	CancelTask(ctx context.Context, id string) (api.Task, error)
	ListTasks(ctx context.Context) ([]api.Task, error)
	ListApprovals(ctx context.Context) ([]api.ApprovalTicket, error)
	DecideApproval(ctx context.Context, id string, d api.ApprovalDecision) error
	GetStatus(ctx context.Context) (api.Status, error)
	ReloadConfig(ctx context.Context) (api.ReloadResult, error)
}

// async results delivered back to Update.
type (
	msgSessions struct {
		list []api.SessionSummary
		err  error
	}
	msgSession struct {
		id  string // session the reply belongs to; stale replies are dropped
		s   api.Session
		err error
	}
	msgApprovals struct {
		list []api.ApprovalTicket
		err  error
	}
	msgStatus struct {
		s   api.Status
		err error
	}
	msgTask struct {
		task api.Task
		err  error
	}
	msgCancelled struct {
		task api.Task
		err  error
	}
	msgTasks struct {
		list []api.Task
		err  error
	}
	msgDecided struct {
		id  string
		err error
	}
	msgRenamed struct {
		id    string
		title string
		err   error
	}
	msgDeleted struct {
		id  string
		err error
	}
	msgExported struct {
		id   string
		path string
		err  error
	}
	msgReloaded struct {
		res api.ReloadResult
		err error
	}
	msgEvent struct{ ev Event }
	msgPoll  struct{}
)

// model is the bubbletea model: one panel set (sessions / approvals /
// console) plus the daemon status bar. All network calls go through
// daemonAPI so tests drive the state machine with a fake.
type model struct {
	ctx    context.Context
	api    daemonAPI
	events <-chan Event

	focus focus

	sessions   []api.SessionSummary
	sessionCur int

	openID   string // session shown in the console, "" = task stream
	messages []api.Message

	approvals   []api.ApprovalTicket
	approvalCur int
	diffScroll  int

	stream       []string
	streamScroll int // offset from the tail: 0 = follow live output
	task         api.Task

	status api.Status
	input  string

	// Session CRUD (U1): a filtered view of the list, an inline rename buffer
	// and a pending delete confirmation. Exactly one of these sub-modes is
	// active at a time, and each captures keys before the panel handlers.
	filter    string // session list filter (title/id, case-insensitive)
	searching bool   // "/" prompt is open: keys edit filter
	renaming  bool   // "r" prompt is open: keys edit renameBuf
	renameBuf string
	pending   string // session id awaiting y/n delete confirmation
	notice    string // last one-line confirmation (export path, rename result)

	// Command palette (U2): a filterable action list that captures every key
	// while open, so ctrl+p is reachable from any panel.
	paletteOpen  bool
	paletteQuery string
	paletteCur   int

	// Session tabs (U2): sessions opened in this run, in open order; tabCur is
	// the active one (ignored while the console shows the task stream).
	tabs   []string
	tabCur int

	width, height int
	err           string
	quitting      bool
}

// newModel builds the initial model.
func newModel(ctx context.Context, api daemonAPI, events <-chan Event) *model {
	return &model{ctx: ctx, api: api, events: events, width: 80, height: 24}
}

// Init implements tea.Model: first data load, SSE subscription and polling.
func (m *model) Init() tea.Cmd {
	return tea.Batch(m.refreshAll(), m.waitEvent(), pollCmd())
}

// Update implements tea.Model.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyPressMsg:
		return m, m.onKey(msg)
	default:
		return m, m.onMsg(msg)
	}
}

// ------------------------------------------------------------------ keys

// onKey handles one key press and returns the follow-up command.
func (m *model) onKey(msg tea.KeyPressMsg) tea.Cmd {
	// A confirmation lives until the next keystroke: stale "已导出 …" text must
	// not sit on the status bar for the rest of the session.
	m.notice = ""
	if m.paletteOpen {
		return m.paletteKey(msg)
	}
	if msg.String() == "ctrl+p" {
		m.openPalette()
		return nil
	}
	// Sub-modes capture every key next: a rename buffer must be able to hold
	// "q" or "x" without quitting or deleting.
	switch {
	case m.renaming:
		return m.renameKey(msg)
	case m.searching:
		return m.searchKey(msg)
	case m.pending != "":
		return m.confirmKey(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return tea.Quit
	case "tab":
		m.focus = (m.focus + 1) % focusCount
		return nil
	case "shift+tab":
		m.focus = (m.focus + focusCount - 1) % focusCount
		return nil
	}
	if m.focus == focusConsole {
		return m.consoleKey(msg)
	}
	return m.panelKey(msg)
}

// renameKey edits the inline rename prompt (U1): enter commits via PATCH,
// esc cancels, everything else edits the buffer (server caps at 20 runes).
func (m *model) renameKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.renaming = false
		m.renameBuf = ""
		return nil
	case "enter":
		id, title := m.pendingRename(), strings.TrimSpace(m.renameBuf)
		m.renaming = false
		m.renameBuf = ""
		if id == "" {
			return nil
		}
		return m.cmdRename(id, title)
	case "backspace":
		r := []rune(m.renameBuf)
		m.renameBuf = string(r[:max(0, len(r)-1)])
		return nil
	}
	if msg.Text != "" && len([]rune(m.renameBuf)) < maxTitleRunes {
		m.renameBuf += msg.Text
	}
	return nil
}

// searchKey edits the session filter (U1): enter keeps the filter, esc clears
// it, backspace narrows it and printable keys extend it.
func (m *model) searchKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.searching = false
		m.filter = ""
		m.sessionCur = 0
		return nil
	case "enter":
		m.searching = false
		return nil
	case "backspace":
		r := []rune(m.filter)
		m.filter = string(r[:max(0, len(r)-1)])
		m.sessionCur = 0
		return nil
	}
	if msg.Text != "" {
		m.filter += msg.Text
		m.sessionCur = 0
	}
	return nil
}

// confirmKey answers the pending delete confirmation (U1).
func (m *model) confirmKey(msg tea.KeyPressMsg) tea.Cmd {
	id := m.pending
	m.pending = ""
	switch strings.ToLower(msg.String()) {
	case "y", "enter":
		return m.cmdDelete(id)
	}
	return nil
}

// consoleKey routes keys while the console (input + stream) is focused.
func (m *model) consoleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case msg.String() == "enter":
		return m.submitTask()
	case msg.String() == "ctrl+x":
		// Interrupt the running task without leaving the console.
		return m.cancelRunning()
	case msg.String() == "ctrl+right":
		return m.switchTab(1)
	case msg.String() == "ctrl+left":
		return m.switchTab(-1)
	case msg.String() == "ctrl+w":
		return m.closeTab()
	case msg.String() == "esc":
		if m.input != "" {
			m.input = ""
		} else {
			m.closeSession()
		}
		return nil
	case msg.String() == "backspace":
		r := []rune(m.input)
		m.input = string(r[:max(0, len(r)-1)])
		return nil
	case msg.String() == "up":
		m.streamScroll++
		return nil
	case msg.String() == "down":
		m.streamScroll = max(0, m.streamScroll-1)
		return nil
	case msg.String() == "q" && m.input == "":
		m.quitting = true
		return tea.Quit
	case msg.Text != "":
		m.input += msg.Text
		return nil
	}
	return nil
}

// panelKey routes keys while a list panel (sessions / approvals) is focused.
// Unhandled printable keys fall through to the task input, so typing always
// starts a task.
func (m *model) panelKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "q":
		m.quitting = true
		return tea.Quit
	case "ctrl+r":
		return m.refreshAll()
	case "up", "k":
		m.moveSelection(-1)
		return nil
	case "down", "j":
		m.moveSelection(1)
		return nil
	case "enter":
		return m.openSelection()
	}
	if m.focus == focusSessions {
		if cmd, handled := m.sessionKey(msg); handled {
			return cmd
		}
	}
	if m.focus == focusApprovals {
		switch msg.String() {
		case "a":
			return m.decideSelection(true)
		case "d":
			return m.decideSelection(false)
		case "pgup":
			m.diffScroll = max(0, m.diffScroll-diffPage)
			return nil
		case "pgdown":
			m.diffScroll += diffPage
			return nil
		}
	}
	if msg.Text != "" {
		m.focus = focusConsole
		m.input += msg.Text
	}
	return nil
}

// sessionKey handles the sessions-panel verbs (U1): "/" filter, "r" rename,
// "x" delete, "e" export. handled=false lets the key fall through to the
// task input so typing still starts a task.
func (m *model) sessionKey(msg tea.KeyPressMsg) (cmd tea.Cmd, handled bool) {
	switch msg.String() {
	case "/":
		m.searching = true
		return nil, true
	case "r":
		s := m.selectedSession()
		if s == nil {
			return nil, true
		}
		m.renaming = true
		m.renameBuf = s.Title
		return nil, true
	case "x":
		s := m.selectedSession()
		if s == nil {
			return nil, true
		}
		m.pending = s.ID
		return nil, true
	case "e":
		s := m.selectedSession()
		if s == nil {
			return nil, true
		}
		return m.cmdExport(s.ID), true
	}
	return nil, false
}

// moveSelection shifts the cursor of the focused list by delta.
func (m *model) moveSelection(delta int) {
	if m.focus == focusSessions {
		m.sessionCur = clamp(m.sessionCur+delta, 0, max(0, len(m.visibleSessions())-1))
		return
	}
	m.approvalCur = clamp(m.approvalCur+delta, 0, max(0, len(m.approvals)-1))
	m.diffScroll = 0
}

// visibleSessions applies the "/" filter to the loaded list: a case-insensitive
// match on title or id, so either the name or a copied id finds a session (U1).
func (m *model) visibleSessions() []api.SessionSummary {
	if m.filter == "" {
		return m.sessions
	}
	q := strings.ToLower(m.filter)
	out := make([]api.SessionSummary, 0, len(m.sessions))
	for _, s := range m.sessions {
		if strings.Contains(strings.ToLower(s.Title), q) || strings.Contains(strings.ToLower(s.ID), q) {
			out = append(out, s)
		}
	}
	return out
}

// openSelection opens the selected session transcript in the console.
func (m *model) openSelection() tea.Cmd {
	if m.focus != focusSessions {
		return nil
	}
	s := m.selectedSession()
	if s == nil {
		return nil
	}
	return m.openSession(s.ID)
}

// decideSelection sends an approve/deny for the selected ticket.
func (m *model) decideSelection(approve bool) tea.Cmd {
	t := m.selectedTicket()
	if t == nil {
		return nil
	}
	return m.cmdDecide(t.ID, approve)
}

// submitTask posts the input line as a new task (POST /api/v1/tasks).
func (m *model) submitTask() tea.Cmd {
	prompt := strings.TrimSpace(m.input)
	if prompt == "" {
		return nil
	}
	req := api.CreateTaskRequest{Prompt: prompt, SessionID: m.openID}
	m.input = ""
	// Task output streams into the task stream panel.
	m.closeSession()
	m.streamScroll = 0
	return m.cmdCreateTask(req)
}

// closeSession drops back from a session transcript to the task stream.
func (m *model) closeSession() {
	m.openID = ""
	m.messages = nil
}

// cancelRunning cancels the task currently streaming, if it is still live.
func (m *model) cancelRunning() tea.Cmd {
	if m.task.ID == "" || (m.task.Status != "pending" && m.task.Status != "running") {
		return nil
	}
	return m.cmdCancel(m.task.ID)
}

// selectedSession returns the highlighted session (of the filtered list), or nil.
func (m *model) selectedSession() *api.SessionSummary {
	list := m.visibleSessions()
	if m.sessionCur < 0 || m.sessionCur >= len(list) {
		return nil
	}
	return &list[m.sessionCur]
}

// pendingRename returns the session the inline rename applies to.
func (m *model) pendingRename() string {
	if s := m.selectedSession(); s != nil {
		return s.ID
	}
	return m.openID
}

// selectedTicket returns the highlighted approval ticket, or nil.
func (m *model) selectedTicket() *api.ApprovalTicket {
	if m.approvalCur < 0 || m.approvalCur >= len(m.approvals) {
		return nil
	}
	return &m.approvals[m.approvalCur]
}

// ----------------------------------------------------------------- async

// onMsg folds one async result into the model.
func (m *model) onMsg(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case msgSessions:
		m.sessions = msg.list
		m.sessionCur = clamp(m.sessionCur, 0, max(0, len(m.sessions)-1))
		m.setErr(msg.err)
	case msgSession:
		// Only the session currently on screen may be filled: a slow reply
		// for session A must not overwrite the transcript of B.
		if m.openID == "" || msg.id != m.openID {
			return nil
		}
		m.setErr(msg.err)
		if msg.err == nil {
			m.messages = msg.s.Messages
		}
	case msgApprovals:
		m.approvals = msg.list
		m.approvalCur = clamp(m.approvalCur, 0, max(0, len(m.approvals)-1))
		m.setErr(msg.err)
	case msgStatus:
		m.setErr(msg.err)
		if msg.err == nil {
			m.status = msg.s
		}
	case msgTask:
		m.setErr(msg.err)
		if msg.err == nil {
			m.task = msg.task
			m.appendStream(fmt.Sprintf("→ task %s started", msg.task.ID))
		}
	case msgCancelled:
		m.setErr(msg.err)
		if msg.err == nil {
			m.task = msg.task
			m.appendStream(fmt.Sprintf("… task %s cancel requested", msg.task.ID))
		}
	case msgTasks:
		// Poll-based recovery: after an SSE drop (or a missed event) the
		// daemon list is the truth for the task currently on screen.
		m.setErr(msg.err)
		if msg.err == nil {
			for _, t := range msg.list {
				if t.ID == m.task.ID {
					m.task = t
					break
				}
			}
		}
	case msgDecided:
		m.setErr(msg.err)
		if msg.err == nil {
			m.dropTicket(msg.id)
			return m.cmdStatus()
		}
	case msgRenamed:
		m.setErr(msg.err)
		if msg.err == nil {
			m.applyRename(msg.id, msg.title)
			m.notice = "已重命名 " + msg.id
		}
	case msgDeleted:
		m.setErr(msg.err)
		if msg.err == nil {
			m.dropSession(msg.id)
			m.notice = "已删除 " + msg.id
		}
	case msgExported:
		m.setErr(msg.err)
		if msg.err == nil {
			m.notice = "已导出 " + msg.path
		}
	case msgReloaded:
		m.setErr(msg.err)
		if msg.err == nil {
			m.notice = fmt.Sprintf("配置已重载：模型 %s｜审核 %s｜工具 %d",
				orDash(msg.res.Model), msg.res.ReviewMode, msg.res.Tools)
			return m.cmdStatus()
		}
	case msgEvent:
		return tea.Batch(m.waitEvent(), m.applyEvent(msg.ev))
	case msgPoll:
		return tea.Batch(m.refreshAll(), pollCmd())
	}
	return nil
}

// applyEvent folds one SSE event into the model (architecture 4.2/4.5).
func (m *model) applyEvent(ev Event) tea.Cmd {
	if ev.IsTask() {
		if t, err := ev.Task(); err == nil {
			m.task = t
			switch ev.Name {
			case api.EventTaskOutput:
				m.appendStream(ev.TaskOutput())
			case api.EventTaskDone:
				m.finishTask(t)
			}
		}
		return nil
	}
	switch ev.Name {
	case api.EventApprovalNew, api.EventApprovalDone:
		return tea.Batch(m.cmdApprovals(), m.cmdStatus())
	}
	return nil
}

// finishTask appends the terminal state of a task to the stream.
func (m *model) finishTask(t api.Task) {
	if t.Reply != "" {
		m.appendStream(t.Reply)
	}
	if t.Err != "" {
		m.appendStream("error: " + t.Err)
	}
	m.appendStream(fmt.Sprintf("✓ task %s %s", t.ID, t.Status))
}

// appendStream appends output text to the task stream, line by line.
func (m *model) appendStream(text string) {
	if text == "" {
		return
	}
	m.stream = append(m.stream, strings.Split(text, "\n")...)
}

// applyRename writes a confirmed title back into the loaded list so the panel
// reflects it without waiting for the next poll.
func (m *model) applyRename(id, title string) {
	for i := range m.sessions {
		if m.sessions[i].ID == id {
			m.sessions[i].Title = title
			return
		}
	}
}

// dropSession removes a deleted session from the list; an open transcript of
// that session closes too (it no longer exists on the daemon).
func (m *model) dropSession(id string) {
	out := m.sessions[:0]
	for _, s := range m.sessions {
		if s.ID != id {
			out = append(out, s)
		}
	}
	m.sessions = out
	m.sessionCur = clamp(m.sessionCur, 0, max(0, len(m.visibleSessions())-1))
	if m.openID == id {
		m.closeSession()
	}
	m.dropTab(id)
}

// dropTicket removes a resolved ticket and re-clamps the cursor.
func (m *model) dropTicket(id string) {
	out := m.approvals[:0]
	for _, t := range m.approvals {
		if t.ID != id {
			out = append(out, t)
		}
	}
	m.approvals = out
	m.approvalCur = clamp(m.approvalCur, 0, max(0, len(m.approvals)-1))
	m.diffScroll = 0
}

// setErr records (or clears) the last client error.
func (m *model) setErr(err error) {
	if err != nil {
		m.err = err.Error()
		return
	}
	m.err = ""
}

// -------------------------------------------------------------- commands

// refreshAll reloads every panel.
func (m *model) refreshAll() tea.Cmd {
	return tea.Batch(m.cmdSessions(), m.cmdApprovals(), m.cmdStatus(), m.cmdTasks())
}

// waitEvent delivers the next SSE event (or nil when the stream is gone).
func (m *model) waitEvent() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		if ch == nil {
			return nil
		}
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return msgEvent{ev: ev}
	}
}

func (m *model) cmdSessions() tea.Cmd {
	return func() tea.Msg {
		list, err := m.api.ListSessions(m.ctx)
		return msgSessions{list: list, err: err}
	}
}

func (m *model) cmdSession(id string) tea.Cmd {
	return func() tea.Msg {
		s, err := m.api.GetSession(m.ctx, id)
		return msgSession{id: id, s: s, err: err}
	}
}

func (m *model) cmdApprovals() tea.Cmd {
	return func() tea.Msg {
		list, err := m.api.ListApprovals(m.ctx)
		return msgApprovals{list: list, err: err}
	}
}

func (m *model) cmdStatus() tea.Cmd {
	return func() tea.Msg {
		s, err := m.api.GetStatus(m.ctx)
		return msgStatus{s: s, err: err}
	}
}

func (m *model) cmdCreateTask(req api.CreateTaskRequest) tea.Cmd {
	return func() tea.Msg {
		t, err := m.api.CreateTask(m.ctx, req)
		return msgTask{task: t, err: err}
	}
}

func (m *model) cmdCancel(id string) tea.Cmd {
	return func() tea.Msg {
		t, err := m.api.CancelTask(m.ctx, id)
		return msgCancelled{task: t, err: err}
	}
}

func (m *model) cmdTasks() tea.Cmd {
	return func() tea.Msg {
		list, err := m.api.ListTasks(m.ctx)
		return msgTasks{list: list, err: err}
	}
}

// cmdReloadConfig asks the daemon to re-read config.toml (U3).
func (m *model) cmdReloadConfig() tea.Cmd {
	return func() tea.Msg {
		res, err := m.api.ReloadConfig(m.ctx)
		return msgReloaded{res: res, err: err}
	}
}

func (m *model) cmdDecide(id string, approve bool) tea.Cmd {
	d := api.ApprovalDecision{Decision: "deny"}
	if approve {
		d.Decision = "approve"
	}
	return func() tea.Msg {
		err := m.api.DecideApproval(m.ctx, id, d)
		return msgDecided{id: id, err: err}
	}
}

func (m *model) cmdRename(id, title string) tea.Cmd {
	return func() tea.Msg {
		updated, err := m.api.RenameSession(m.ctx, id, title)
		if err != nil {
			return msgRenamed{id: id, err: err}
		}
		return msgRenamed{id: id, title: updated.Title}
	}
}

func (m *model) cmdDelete(id string) tea.Cmd {
	return func() tea.Msg {
		return msgDeleted{id: id, err: m.api.DeleteSession(m.ctx, id)}
	}
}

// cmdExport downloads the session envelope and writes it next to the user's
// working directory (the TUI has no access to the daemon's home, and a file in
// cwd is where `chyan export --out` would put it too).
func (m *model) cmdExport(id string) tea.Cmd {
	return func() tea.Msg {
		raw, err := m.api.ExportSession(m.ctx, id)
		if err != nil {
			return msgExported{id: id, err: err}
		}
		name := fmt.Sprintf("chyan-session-%s.json", id)
		path := name
		if wd, err := os.Getwd(); err == nil {
			path = filepath.Join(wd, name)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return msgExported{id: id, err: err}
		}
		return msgExported{id: id, path: path}
	}
}

// ----------------------------------------------------------------- utils

// clamp keeps v within [lo, hi].
func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

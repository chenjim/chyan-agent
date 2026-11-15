// model_test.go — state machine and rendering tests driven by a fake daemon.
package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"

	"chyan.tech/chyan/internal/api"
)

// fakeAPI is an in-memory daemonAPI recording calls.
type fakeAPI struct {
	sessions  []api.SessionSummary
	session   api.Session
	approvals []api.ApprovalTicket
	status    api.Status
	task      api.Task
	exported  []byte

	created   []api.CreateTaskRequest
	decided   []api.ApprovalDecision
	decideID  string
	cancelled []string
	renamed   []string
	deleted   []string
	exports   []string
	reloads   int
	err       error
}

func (f *fakeAPI) ListSessions(context.Context) ([]api.SessionSummary, error) {
	return f.sessions, f.err
}
func (f *fakeAPI) GetSession(context.Context, string) (api.Session, error) {
	return f.session, f.err
}
func (f *fakeAPI) RenameSession(_ context.Context, id, title string) (api.SessionSummary, error) {
	f.renamed = append(f.renamed, id+":"+title)
	return api.SessionSummary{ID: id, Title: title}, f.err
}
func (f *fakeAPI) DeleteSession(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.err
}
func (f *fakeAPI) ExportSession(_ context.Context, id string) ([]byte, error) {
	f.exports = append(f.exports, id)
	return f.exported, f.err
}
func (f *fakeAPI) CreateTask(_ context.Context, req api.CreateTaskRequest) (api.Task, error) {
	f.created = append(f.created, req)
	return f.task, f.err
}
func (f *fakeAPI) CancelTask(_ context.Context, id string) (api.Task, error) {
	f.cancelled = append(f.cancelled, id)
	t := f.task
	t.Status = "interrupted"
	return t, f.err
}
func (f *fakeAPI) ListTasks(context.Context) ([]api.Task, error) {
	return []api.Task{f.task}, f.err
}
func (f *fakeAPI) ListApprovals(context.Context) ([]api.ApprovalTicket, error) {
	return f.approvals, f.err
}
func (f *fakeAPI) DecideApproval(_ context.Context, id string, d api.ApprovalDecision) error {
	f.decideID, f.decided = id, append(f.decided, d)
	return f.err
}
func (f *fakeAPI) GetStatus(context.Context) (api.Status, error) {
	return f.status, f.err
}
func (f *fakeAPI) ReloadConfig(context.Context) (api.ReloadResult, error) {
	f.reloads++
	return api.ReloadResult{ConfigPath: "/tmp/config.toml", Model: "m", ReviewMode: "ai", Tools: 18}, f.err
}

// runMsg feeds one message through Update and runs the returned command.
func runMsg(t *testing.T, m *model, msg tea.Msg) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(msg)
	return cmd
}

// key builds a printable key press message.
func key(s string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: []rune(s)[0], Text: s}
}

func newTestModel() (*model, *fakeAPI) {
	f := &fakeAPI{
		sessions:  []api.SessionSummary{{ID: "ses1", Title: "t1"}, {ID: "ses2", Title: "t2"}},
		approvals: []api.ApprovalTicket{{ID: "ap1", Action: "bash", Resource: "rm", Diff: "--- a\n+++ b"}},
		status:    api.Status{Version: "0.1", Sessions: 2, PendingApprovals: 1, ReviewMode: "ai"},
		task:      api.Task{ID: "task1", Status: "pending"},
	}
	m := newModel(context.Background(), f, nil)
	m.sessions = f.sessions
	m.approvals = f.approvals
	return m, f
}

func TestKeyNavigationAndFocus(t *testing.T) {
	m, _ := newTestModel()
	m.sessions = []api.SessionSummary{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	// Tab cycles focus: sessions -> approvals -> console -> sessions.
	for i, want := range []focus{focusApprovals, focusConsole, focusSessions} {
		runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
		if m.focus != want {
			t.Fatalf("tab #%d: focus = %v, want %v", i, m.focus, want)
		}
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.focus != focusConsole {
		t.Fatalf("shift+tab: focus = %v", m.focus)
	}

	m.focus = focusSessions
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.sessionCur != 2 {
		t.Fatalf("sessionCur = %d", m.sessionCur)
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyDown}) // clamps at end
	if m.sessionCur != 2 {
		t.Fatalf("sessionCur = %d", m.sessionCur)
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.sessionCur != 1 {
		t.Fatalf("sessionCur = %d", m.sessionCur)
	}
	runMsg(t, m, key("j")) // vim-style down
	if m.sessionCur != 2 {
		t.Fatalf("sessionCur = %d", m.sessionCur)
	}
	runMsg(t, m, key("k"))
	if m.sessionCur != 1 {
		t.Fatalf("sessionCur = %d", m.sessionCur)
	}
}

func TestOpenSessionAndEscape(t *testing.T) {
	m, f := newTestModel()
	f.session = api.Session{
		SessionSummary: api.SessionSummary{ID: "ses1"},
		Messages:       []api.Message{{ID: "m1", Role: "user", Text: "hello", ToolCalls: []string{"bash ls"}}},
	}
	m.focus = focusSessions
	m.sessionCur = 0
	cmd := runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.openID != "ses1" || m.focus != focusConsole {
		t.Fatalf("openID = %q focus = %v", m.openID, m.focus)
	}
	// Execute the async load command.
	msg := cmd()
	if _, ok := msg.(msgSession); !ok {
		t.Fatalf("cmd returned %T", msg)
	}
	runMsg(t, m, msg)
	if len(m.messages) != 1 {
		t.Fatalf("messages = %+v", m.messages)
	}
	lines := m.streamLines()
	if len(lines) != 2 || !strings.Contains(lines[1], "bash ls") {
		t.Fatalf("streamLines = %q", lines)
	}

	// esc closes the session view (input empty).
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.openID != "" {
		t.Fatalf("openID = %q after esc", m.openID)
	}
}

func TestSubmitTask(t *testing.T) {
	m, f := newTestModel()
	m.focus = focusConsole
	for _, r := range "fix bug" {
		runMsg(t, m, key(string(r)))
	}
	if m.input != "fix bug" {
		t.Fatalf("input = %q", m.input)
	}
	cmd := runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	runMsg(t, m, cmd())
	if len(f.created) != 1 || f.created[0].Prompt != "fix bug" {
		t.Fatalf("created = %+v", f.created)
	}
	if m.input != "" {
		t.Fatalf("input not cleared: %q", m.input)
	}
	runMsg(t, m, cmd())
	if m.task.ID != "task1" {
		t.Fatalf("task = %+v", m.task)
	}
	if !strings.Contains(strings.Join(m.stream, "\n"), "task task1 started") {
		t.Fatalf("stream = %q", m.stream)
	}

	// Empty input submits nothing.
	cmd = runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("empty submit should be a no-op")
	}

	// backspace edits the input.
	runMsg(t, m, key("a"))
	runMsg(t, m, key("b"))
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.input != "a" {
		t.Fatalf("input = %q", m.input)
	}
}

// TestCancelRunningTask checks ctrl+x cancels only a live task and that the
// daemon's returned state replaces the local one.
func TestCancelRunningTask(t *testing.T) {
	m, f := newTestModel()
	m.focus = focusConsole
	m.task = api.Task{ID: "task1", Status: "running"}

	cmd := runMsg(t, m, tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+x should issue a cancel command")
	}
	runMsg(t, m, cmd())
	if len(f.cancelled) != 1 || f.cancelled[0] != "task1" {
		t.Fatalf("cancelled = %+v", f.cancelled)
	}
	if m.task.Status != "interrupted" {
		t.Fatalf("task status = %q", m.task.Status)
	}
	if !strings.Contains(strings.Join(m.stream, "\n"), "cancel requested") {
		t.Fatalf("stream = %q", m.stream)
	}

	// A finished task is not cancelled again.
	f.cancelled = nil
	m.task.Status = "done"
	if cmd := runMsg(t, m, tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}); cmd != nil {
		t.Fatal("ctrl+x on a finished task should be a no-op")
	}
	if len(f.cancelled) != 0 {
		t.Fatalf("unexpected cancel calls: %+v", f.cancelled)
	}
}

// TestPollRecoversTaskState checks a missed SSE event is repaired by the
// periodic task poll (daemon list is the truth).
func TestPollRecoversTaskState(t *testing.T) {
	m, f := newTestModel()
	m.task = api.Task{ID: "task1", Status: "running"}
	f.task = api.Task{ID: "task1", Status: "done", Reply: "ok"}

	cmd := m.cmdTasks()
	if cmd == nil {
		t.Fatal("cmdTasks should return a command")
	}
	runMsg(t, m, cmd())
	if m.task.Status != "done" {
		t.Fatalf("task status = %q after poll, want done", m.task.Status)
	}
}

func TestSubmitTaskUsesOpenSession(t *testing.T) {
	m, f := newTestModel()
	m.openID = "ses9"
	m.focus = focusConsole
	m.input = "continue"
	cmd := runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	runMsg(t, m, cmd())
	if len(f.created) != 1 || f.created[0].SessionID != "ses9" {
		t.Fatalf("created = %+v", f.created)
	}
	if m.openID != "" {
		t.Fatalf("stream switch did not close session: %q", m.openID)
	}
}

func TestTypingSwitchesToConsole(t *testing.T) {
	m, _ := newTestModel()
	m.focus = focusSessions
	runMsg(t, m, key("h"))
	runMsg(t, m, key("i"))
	if m.focus != focusConsole || m.input != "hi" {
		t.Fatalf("focus = %v input = %q", m.focus, m.input)
	}
	// Space is appended while in console.
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if m.input != "hi " {
		t.Fatalf("input = %q", m.input)
	}
	// esc clears the input first, then closes the session.
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.input != "" {
		t.Fatalf("input = %q", m.input)
	}
}

func TestApprovalDecide(t *testing.T) {
	m, f := newTestModel()
	m.focus = focusApprovals
	m.approvals = []api.ApprovalTicket{
		{ID: "ap1", Action: "bash", Resource: "rm"},
		{ID: "ap2", Action: "edit", Resource: "main.go"},
	}
	m.approvalCur = 1
	cmd := runMsg(t, m, key("a"))
	runMsg(t, m, cmd()) // execute POST, then fold the result
	if f.decideID != "ap2" || f.decided[0].Decision != "approve" {
		t.Fatalf("decide = %v %+v", f.decideID, f.decided)
	}
	if len(m.approvals) != 1 || m.approvals[0].ID != "ap1" {
		t.Fatalf("approvals = %+v", m.approvals)
	}

	cmd = runMsg(t, m, key("d"))
	runMsg(t, m, cmd())
	if f.decideID != "ap1" || f.decided[1].Decision != "deny" {
		t.Fatalf("decide = %v %+v", f.decideID, f.decided)
	}
	if len(m.approvals) != 0 {
		t.Fatalf("approvals = %+v", m.approvals)
	}

	// No ticket selected: decide is a no-op.
	if cmd := m.decideSelection(true); cmd != nil {
		t.Fatal("decide on empty list should be a no-op")
	}
}

func TestDiffScroll(t *testing.T) {
	m, _ := newTestModel()
	m.focus = focusApprovals
	m.approvals = []api.ApprovalTicket{{ID: "ap1", Diff: "line1\nline2"}}
	m.approvals[0].Diff = strings.Repeat("x\n", 50)
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.diffScroll != diffPage {
		t.Fatalf("diffScroll = %d", m.diffScroll)
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.diffScroll != 0 {
		t.Fatalf("diffScroll = %d", m.diffScroll)
	}
	// Changing selection resets the scroll.
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown})
	m.moveSelection(1)
	if m.diffScroll != 0 {
		t.Fatalf("diffScroll = %d", m.diffScroll)
	}
}

func TestQuit(t *testing.T) {
	m, _ := newTestModel()
	m.focus = focusSessions
	cmd := runMsg(t, m, key("q"))
	if cmd == nil {
		t.Fatal("q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("q should return QuitMsg")
	}

	m2, _ := newTestModel()
	m2.focus = focusConsole
	m2.input = "quiet"
	runMsg(t, m2, key("q"))
	if m2.quitting {
		t.Fatal("q inside input must not quit")
	}
	if cmd := runMsg(t, m2, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("ctrl+c should quit")
	}
}

func TestOnMsgSessionAndStatus(t *testing.T) {
	m, f := newTestModel()
	f.session = api.Session{Messages: []api.Message{{ID: "m1", Role: "assistant", Text: "ok"}}}
	m.openID = "ses1"
	runMsg(t, m, msgSession{id: "ses1", s: f.session})
	if len(m.messages) != 1 {
		t.Fatalf("messages = %+v", m.messages)
	}
	runMsg(t, m, msgSession{id: "ses1", err: errors.New("no session")})
	if !strings.Contains(m.err, "no session") {
		t.Fatalf("err = %q", m.err)
	}
	// A reply for another (previously opened) session must be ignored.
	runMsg(t, m, msgSession{id: "ses2", s: api.Session{Messages: []api.Message{{ID: "x"}}}})
	if len(m.messages) != 1 {
		t.Fatalf("stale session reply applied: %+v", m.messages)
	}
	// msgSessions with an error still clamps the cursor.
	m.sessionCur = 5
	runMsg(t, m, msgSessions{err: errors.New("down")})
	if m.sessionCur != 0 {
		t.Fatalf("sessionCur = %d", m.sessionCur)
	}
	// msgApprovals re-clamps too.
	m.approvalCur = 3
	runMsg(t, m, msgApprovals{list: f.approvals})
	if m.approvalCur != 0 {
		t.Fatalf("approvalCur = %d", m.approvalCur)
	}
	// Task creation error surfaces in the status line.
	runMsg(t, m, msgTask{err: errors.New("refused")})
	if !strings.Contains(m.err, "refused") {
		t.Fatalf("err = %q", m.err)
	}
}

func TestWaitEvent(t *testing.T) {
	ch := make(chan Event, 1)
	m := newModel(context.Background(), nil, ch)
	ch <- Event{Name: "ping"}
	msg := m.waitEvent()()
	if me, ok := msg.(msgEvent); !ok || me.ev.Name != "ping" {
		t.Fatalf("msg = %#v", msg)
	}
	// A closed stream delivers a nil message instead of blocking.
	close(ch)
	if msg := m.waitEvent()(); msg != nil {
		t.Fatalf("msg = %#v", msg)
	}
	// A model without a stream delivers nil immediately.
	m2 := newModel(context.Background(), nil, nil)
	if msg := m2.waitEvent()(); msg != nil {
		t.Fatalf("msg = %#v", msg)
	}
}

func TestNilSelection(t *testing.T) {
	m, _ := newTestModel()
	m.sessions = nil
	m.approvals = nil
	if m.selectedSession() != nil || m.selectedTicket() != nil {
		t.Fatal("selection on empty lists should be nil")
	}
	// Opening without a session is a no-op.
	m.focus = focusSessions
	if cmd := m.openSelection(); cmd != nil {
		t.Fatal("openSelection on empty list should be nil")
	}
	// Out-of-range cursors return nil.
	m.sessions = []api.SessionSummary{{ID: "a"}}
	m.sessionCur = 9
	if m.selectedSession() != nil {
		t.Fatal("out-of-range session cursor")
	}
}

func TestSubmitTaskError(t *testing.T) {
	m, f := newTestModel()
	f.err = errors.New("refused")
	m.focus = focusConsole
	m.input = "do it"
	cmd := runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("want create command")
	}
	runMsg(t, m, cmd())
	if !strings.Contains(m.err, "refused") {
		t.Fatalf("err = %q", m.err)
	}
}

func TestTaskEvents(t *testing.T) {
	m, _ := newTestModel()
	runMsg(t, m, msgEvent{ev: Event{
		Name: api.EventTaskOutput, Data: map[string]any{"text": "step 1\nstep 2"},
	}})
	if len(m.stream) != 2 {
		t.Fatalf("stream = %q", m.stream)
	}
	// Stream scrolling follows the tail at 0 and pages up (console focused).
	m.focus = focusConsole
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if m.streamScroll != 1 {
		t.Fatalf("streamScroll = %d", m.streamScroll)
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.streamScroll != 0 {
		t.Fatalf("streamScroll = %d", m.streamScroll)
	}

	runMsg(t, m, msgEvent{ev: Event{
		Name: api.EventTaskDone,
		Data: map[string]any{"id": "t1", "status": "failed", "err": "boom", "reply": "partial"},
	}})
	joined := strings.Join(m.stream, "\n")
	for _, want := range []string{"partial", "error: boom", "✓ task t1 failed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stream missing %q: %q", want, joined)
		}
	}

	// approval events trigger a refresh command.
	cmd := runMsg(t, m, msgEvent{ev: Event{Name: api.EventApprovalNew}})
	if cmd == nil {
		t.Fatal("approval event should refresh")
	}
}

func TestPollAndErrors(t *testing.T) {
	m, f := newTestModel()
	if cmd := runMsg(t, m, msgPoll{}); cmd == nil {
		t.Fatal("poll should refresh")
	}

	f.err = errors.New("daemon down")
	runMsg(t, m, msgStatus{err: f.err})
	if !strings.Contains(m.err, "daemon down") {
		t.Fatalf("err = %q", m.err)
	}
	runMsg(t, m, msgStatus{s: f.status})
	if m.err != "" {
		t.Fatalf("err = %q", m.err)
	}
	// Failed decide keeps the ticket.
	m.approvals = []api.ApprovalTicket{{ID: "ap1"}}
	runMsg(t, m, msgDecided{id: "ap1", err: errors.New("nope")})
	if len(m.approvals) != 1 {
		t.Fatalf("approvals = %+v", m.approvals)
	}
}

func TestWindowSizeAndView(t *testing.T) {
	m, _ := newTestModel()
	m.sessions = []api.SessionSummary{{ID: "ses1", Title: "A very long session title that needs truncation"}}
	m.approvals = []api.ApprovalTicket{{
		ID: "ap1", Action: "edit", Resource: "main.go", Verdict: "uncertain",
		Diff: strings.Repeat("diff line\n", 40),
	}}
	m.stream = []string{"out1", "out2"}
	m.input = "hello"
	runMsg(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	v := m.View().Content
	if v == "" {
		t.Fatal("empty view")
	}
	lines := strings.Split(v, "\n")
	if len(lines) != 30 { // full frame: status + top + console + help
		t.Fatalf("view lines = %d", len(lines))
	}
	for _, want := range []string{"Sessions", "Approvals", "diff preview", "> hello", "命令面板"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view missing %q", want)
		}
	}
	// Quitting renders nothing.
	m.quitting = true
	if m.View().Content != "" {
		t.Fatal("quitting view should be empty")
	}
}

func TestHelpers(t *testing.T) {
	if clamp(5, 0, 3) != 3 || clamp(-1, 0, 3) != 0 || clamp(2, 0, 3) != 2 {
		t.Fatal("clamp")
	}
	if clamp(5, 3, 1) != 3 {
		t.Fatal("clamp inverted range")
	}
	if truncate("abcdef", 4) != "abc…" {
		t.Fatalf("truncate = %q", truncate("abcdef", 4))
	}
	if truncate("abc", 10) != "abc" {
		t.Fatalf("truncate = %q", truncate("abc", 10))
	}
	if truncate("ab\ncd", 10) != "ab⏎cd" {
		t.Fatalf("truncate newline = %q", truncate("ab\ncd", 10))
	}
	if truncate("abc", 0) != "" || truncate("abc", 1) != "…" {
		t.Fatal("truncate edge")
	}
	// CJK glyphs take two cells: a 6-cell budget fits two characters + ellipsis.
	if got := truncate("中文标题", 6); got != "中文…" {
		t.Fatalf("truncate CJK = %q", got)
	}
	if orDash("") != "-" || orDash("x") != "x" {
		t.Fatal("orDash")
	}
	if verdictTag("") != "" || verdictTag("low") != "(low)" {
		t.Fatal("verdictTag")
	}
	block := fit("a\nb", 5, 4)
	if len(strings.Split(block, "\n")) != 4 {
		t.Fatalf("fit = %q", block)
	}
	// fit clips long lines.
	if got := fit(strings.Repeat("x", 10), 4, 1); len([]rune(got)) > 4 {
		t.Fatalf("fit clip = %q", got)
	}
}

func TestTruncateANSI(t *testing.T) {
	got := truncate("\x1b[31mabcdef\x1b[0m", 4)
	if !strings.Contains(got, "\x1b[31mabc…") {
		t.Fatalf("truncate ANSI = %q, want complete color sequence and abc…", got)
	}
	if width := lipgloss.Width(got); width != 4 {
		t.Fatalf("truncate ANSI width = %d, want 4 (%q)", width, got)
	}
}

func TestFocusNames(t *testing.T) {
	if focusSessions.String() != "sessions" || focusApprovals.String() != "approvals" || focusConsole.String() != "console" {
		t.Fatal("focus names")
	}
}

// ---------------------------------------------------------- session verbs

// typeQuery feeds every rune of s as one key press (filter/rename prompts).
func typeQuery(t *testing.T, m *model, s string) {
	t.Helper()
	for _, r := range s {
		runMsg(t, m, key(string(r)))
	}
}

func TestSessionFilterNarrowsList(t *testing.T) {
	m, _ := newTestModel()
	m.focus = focusSessions
	m.sessions = []api.SessionSummary{
		{ID: "ses1", Title: "代码审查"}, {ID: "ses2", Title: "写周报"},
	}
	runMsg(t, m, key("/"))
	if !m.searching {
		t.Fatal("'/' must open the filter prompt")
	}
	typeQuery(t, m, "周报")
	if m.filter != "周报" || len(m.visibleSessions()) != 1 {
		t.Fatalf("filter = %q, visible = %+v", m.filter, m.visibleSessions())
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.searching || m.filter != "周报" {
		t.Fatalf("enter keeps the filter: searching=%v filter=%q", m.searching, m.filter)
	}
	// Ids match too, and esc clears the filter.
	runMsg(t, m, key("/"))
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.searching || m.filter != "" || len(m.visibleSessions()) != 2 {
		t.Fatalf("esc must clear: searching=%v filter=%q", m.searching, m.filter)
	}
}

func TestSessionRenameSendsPatch(t *testing.T) {
	m, f := newTestModel()
	m.focus = focusSessions
	m.sessionCur = 1
	runMsg(t, m, key("r"))
	if !m.renaming || m.renameBuf != "t2" {
		t.Fatalf("rename prompt: renaming=%v buf=%q", m.renaming, m.renameBuf)
	}
	// The prompt captures keys: "q" edits, it does not quit.
	runMsg(t, m, key("q"))
	if m.quitting || m.renameBuf != "t2q" {
		t.Fatalf("rename buffer = %q quitting=%v", m.renameBuf, m.quitting)
	}
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	cmd := runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.renaming {
		t.Fatal("enter closes the prompt")
	}
	runMsg(t, m, cmd())
	if len(f.renamed) != 1 || f.renamed[0] != "ses2:t2" {
		t.Fatalf("renamed = %v", f.renamed)
	}
	if m.sessions[1].Title != "t2" {
		t.Fatalf("list not updated: %+v", m.sessions)
	}
	if !strings.Contains(m.notice, "已重命名") {
		t.Fatalf("notice = %q", m.notice)
	}
	// The confirmation clears on the next keystroke instead of lingering.
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if m.notice != "" {
		t.Fatalf("notice must clear on the next key: %q", m.notice)
	}
	// esc cancels without a call.
	runMsg(t, m, key("r"))
	runMsg(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.renaming || len(f.renamed) != 1 {
		t.Fatalf("esc must cancel: renaming=%v renamed=%v", m.renaming, f.renamed)
	}
}

func TestSessionDeleteNeedsConfirmation(t *testing.T) {
	m, f := newTestModel()
	m.focus = focusSessions
	runMsg(t, m, key("x"))
	if m.pending != "ses1" {
		t.Fatalf("pending = %q", m.pending)
	}
	// "n" aborts.
	runMsg(t, m, key("n"))
	if m.pending != "" || len(f.deleted) != 0 {
		t.Fatalf("abort failed: pending=%q deleted=%v", m.pending, f.deleted)
	}
	// "y" deletes and drops the row.
	runMsg(t, m, key("x"))
	cmd := runMsg(t, m, key("y"))
	runMsg(t, m, cmd())
	if len(f.deleted) != 1 || f.deleted[0] != "ses1" {
		t.Fatalf("deleted = %v", f.deleted)
	}
	if len(m.sessions) != 1 || m.sessions[0].ID != "ses2" {
		t.Fatalf("sessions = %+v", m.sessions)
	}
	if !strings.Contains(m.notice, "已删除") {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestSessionExportWritesFile(t *testing.T) {
	m, f := newTestModel()
	m.focus = focusSessions
	f.exported = []byte(`{"version":1}`)
	t.Chdir(t.TempDir())
	cmd := runMsg(t, m, key("e"))
	runMsg(t, m, cmd())
	if len(f.exports) != 1 || f.exports[0] != "ses1" {
		t.Fatalf("exports = %v", f.exports)
	}
	path := "chyan-session-ses1.json"
	if !strings.HasSuffix(m.notice, path) {
		t.Fatalf("notice = %q", m.notice)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(f.exported) {
		t.Fatalf("export file = %q err=%v", raw, err)
	}
}

func TestSessionDeleteFailureKeepsList(t *testing.T) {
	m, f := newTestModel()
	m.focus = focusSessions
	f.err = errors.New("session has a running task")
	runMsg(t, m, key("x"))
	runMsg(t, m, runMsg(t, m, key("y"))())
	if len(m.sessions) != 2 {
		t.Fatalf("failed delete must keep the row: %+v", m.sessions)
	}
	if !strings.Contains(m.err, "running task") {
		t.Fatalf("err = %q", m.err)
	}
}

func TestStatusBarContextUsage(t *testing.T) {
	m, _ := newTestModel()
	m.width = 160
	m.status = api.Status{Version: "0.1", ContextLimit: 200000, FixedCost: 4000, HistoryTokens: 96000}
	got := m.renderStatus()
	if !strings.Contains(got, "ctx 50%") || !strings.Contains(got, "100k/200k") {
		t.Fatalf("status = %q", got)
	}
	// No reported window: the segment is omitted, not shown as 0/0.
	m.status = api.Status{}
	if strings.Contains(m.renderStatus(), "ctx") {
		t.Fatalf("status without limit = %q", m.renderStatus())
	}
}

func TestCompactTokens(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1500: "2k", 131072: "131k", 2_000_000: "2.0M"}
	for in, want := range cases {
		if got := compactTokens(in); got != want {
			t.Errorf("compactTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

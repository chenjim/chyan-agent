// Package daemon: REST handlers. Tasks run as background goroutines; their
// progress is broadcast over the SSE hub and persisted to the store.
package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// taskState tracks one background dispatch in memory (source of truth is the
// store row; this map is the live view for polling + SSE).
type taskState struct {
	mu      sync.Mutex
	task    api.Task
	replies []string
	// interrupt cancels the run's LLM/tool loop once the agent is built.
	// cancelRequested latches a cancel that arrives before the run starts.
	interrupt       func()
	cancelRequested bool
}

// newTaskID generates a task identifier.
func newTaskID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("tsk%016x", buf), nil
}

// handleCreateTask starts one background agent run (POST /api/v1/tasks).
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTaskRequest
	if err := decodeBodyLimit(w, r, &req, maxTaskBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	// Validate an explicit workspace before accepting the task: a bad path
	// should fail the request, not a queued job the user is watching.
	if req.WorkDir != "" {
		if _, err := app.ResolveWorkDir(s.app.WorkDir(), req.WorkDir); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Retry && req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "retry requires a session")
		return
	}
	if req.Phase != "" && req.Phase != string(core.PhasePlanner) && req.Phase != string(core.PhaseExecutor) {
		writeError(w, http.StatusBadRequest, "phase must be planner or executor")
		return
	}
	if req.ForkFrom != "" && req.SessionID != "" {
		writeError(w, http.StatusBadRequest, "fork_from and session_id are mutually exclusive")
		return
	}
	// Resolve (or create) the target conversation before dispatch: the client
	// navigates to its transcript as soon as POST /tasks is accepted. "continue"
	// resolves to the latest session here, so the accepted task already carries
	// a real id (CLI --continue).
	if req.ForkFrom != "" {
		sid, err := s.app.ForkSession(r.Context(), req.ForkFrom)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.SessionID = sid
	} else if req.SessionID == "" || req.SessionID == "continue" {
		sid, err := s.app.EnsureSession(r.Context(), req.SessionID, req.Agent, req.Model)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.SessionID = sid
	}
	id, err := newTaskID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UTC()
	ts := &taskState{task: api.Task{
		ID: id, SessionID: req.SessionID,
		Status: "pending", CreatedAt: now, UpdatedAt: now,
	}}
	s.tasksMu.Lock()
	s.tasks[id] = ts
	s.tasksMu.Unlock()

	if err := s.store.CreateTask(r.Context(), store.Task{
		ID: id, SessionID: req.SessionID, Kind: "dispatch",
		Status: "pending", CreatedAt: now,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	images, err := imagesFromRequest(req.Images)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.tasksWG.Add(1)
	go func() {
		defer s.tasksWG.Done()
		s.runTask(id, req, images)
	}()

	writeJSON(w, http.StatusAccepted, ts.snapshot())
}

// runTask executes one background dispatch to completion. It first waits for
// a free slot (queue), then runs under the per-task timeout.
func (s *Server) runTask(id string, req api.CreateTaskRequest, images []app.ImageInput) {
	start := time.Now()
	queueWait := time.Duration(0)
	if queued, ok := s.getTask(id); ok && !queued.CreatedAt.IsZero() {
		queueWait = start.Sub(queued.CreatedAt)
	}
	// Queue: the task stays "pending" in the API/SSE view until a slot frees.
	if !s.acquireTaskSlot() {
		// The daemon is stopping: nothing will run this task.
		s.interruptTask(id, "")
		return
	}
	defer s.releaseTaskSlot()

	s.updateTask(id, func(t *api.Task) {
		t.Status = "running"
		t.UpdatedAt = time.Now().UTC()
	})
	if started, ok := s.getTask(id); ok {
		s.hub.publish(Event{Name: api.EventTaskStarted, Data: started})
	}
	// The run inherits the daemon lifetime: SIGTERM cancels in-flight tasks
	// instead of leaving them writing to a dead server.
	ctx, cancel := s.taskContext()
	defer cancel()

	workDir, err := s.taskWorkDir(ctx, req)
	if err != nil {
		s.failTask(id, req.SessionID, err)
		return
	}

	// Live streaming: forward each token delta as a task.output event so
	// TUI/PWA render the reply as it is generated (architecture 4.7).
	// The agent is pinned per run: a concurrent task must not share the
	// process-wide prompt/model/permission set.
	phase := core.Phase(req.Phase)
	if phase != core.PhasePlanner {
		phase = core.PhaseExecutor
	}
	msg, sid, err := s.app.RunOpts(ctx, req.SessionID, req.Prompt, app.RunOptions{
		Agent:   req.Agent,
		Model:   req.Model,
		WorkDir: workDir,
		TaskID:  id,
		Phase:   phase,
		Sink: func(text string) {
			s.hub.publish(Event{Name: api.EventTaskOutput, Data: map[string]any{
				"id": id, "session_id": req.SessionID, "status": "running", "text": text,
			}})
		},
		OnInterruptReady: func(interrupt func()) { s.attachInterrupt(id, interrupt) },
		OnProgress: func(ev core.ProgressEvent) {
			// Forward tool/thinking milestones so the Web timeline can show
			// what the agent is doing without waiting for the final reply.
			kind := string(ev.Kind)
			switch ev.Kind {
			case core.ProgressThinking, core.ProgressToolRun, core.ProgressToolDone, core.ProgressReflexion:
			default:
				return // reasoning deltas stay out of the timeline payload
			}
			s.hub.publish(Event{Name: api.EventTaskProgress, Data: map[string]any{
				"id":         id,
				"kind":       kind,
				"tool":       ev.Tool,
				"detail":     ev.Detail,
				"elapsed_ms": ev.Elapsed.Milliseconds(),
				"err":        errString(ev.Err),
			}})
		},
		Images: images,
		Retry:  req.Retry,
	})
	// A new session's id is only known after the run starts, so bind the
	// workspace here as well: later turns must reuse the same project root.
	if sid != "" && workDir != "" {
		_ = s.store.SetKV(ctx, workspaceSessionKey(sid), []byte(workDir))
	}
	reply := ""
	if err == nil {
		reply = core.FlattenText(msg)
	}
	// Scheduling metric: how long the task waited for a slot and how long the
	// turn itself took (P1 observability).
	s.publishMetric(map[string]any{
		"kind": "task.turn", "task_id": id, "session_id": sid,
		"queued_ms": queueWait.Milliseconds(),
		"turn_ms":   time.Since(start).Milliseconds(),
		"ok":        err == nil,
	})
	if err != nil {
		if errors.Is(err, core.ErrInterrupted) || errors.Is(err, context.Canceled) {
			s.interruptTask(id, sid)
			return
		}
		err = normalizeTaskError(err, s.taskTimeout)
		s.failTask(id, sid, err)
		return
	}

	now := time.Now().UTC()
	s.updateTask(id, func(t *api.Task) {
		t.Status = "done"
		t.SessionID = sid
		t.Reply = reply
		t.UpdatedAt = now
	})
	_ = s.store.UpdateTask(context.Background(), store.Task{
		ID: id, SessionID: sid, Kind: "dispatch", Status: "done", LastRunAt: now, CreatedAt: now,
	})
	if done, ok := s.getTask(id); ok {
		s.hub.publish(Event{Name: api.EventTaskDone, Data: done})
	}
}

// normalizeTaskError turns fatal run errors into user-facing task errors.
// An unanswered approval denies the ask and aborts the run, so the transcript
// must say that instead of reporting a generic tool failure.
func normalizeTaskError(err error, timeout time.Duration) error {
	if errors.Is(err, core.ErrApprovalTimeout) {
		return errors.New("审批超时：已拒绝该操作并停止执行")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("task timed out after %s", timeout)
	}
	return err
}

// errString renders an optional error for JSON payloads.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// failTask marks a task failed and broadcasts the error. A known session id is
// bound even when the run failed, so the transcript can show this failure.
func (s *Server) failTask(id, sessionID string, err error) {
	log.Printf("daemon: task %s failed: %v", id, err)
	s.updateTask(id, func(t *api.Task) {
		t.Status = "failed"
		t.Err = err.Error()
		if sessionID != "" {
			t.SessionID = sessionID
		}
		t.UpdatedAt = time.Now().UTC()
	})
	s.persistTaskStatus(id, "failed")
	if failed, ok := s.getTask(id); ok {
		s.hub.publish(Event{Name: api.EventTaskDone, Data: failed})
	}
}

// interruptTask marks a task interrupted (user cancel or shutdown) and
// broadcasts the terminal state. Repeated calls are harmless: the status is
// simply written again.
func (s *Server) interruptTask(id, sessionID string) {
	s.updateTask(id, func(t *api.Task) {
		t.Status = "interrupted"
		if sessionID != "" {
			t.SessionID = sessionID
		}
		t.UpdatedAt = time.Now().UTC()
	})
	s.persistTaskStatus(id, "interrupted")
	if done, ok := s.getTask(id); ok {
		s.hub.publish(Event{Name: api.EventTaskDone, Data: done})
	}
}

// persistTaskStatus mirrors a terminal in-memory status to the store row.
func (s *Server) persistTaskStatus(id, status string) {
	t, ok := s.getTask(id)
	if !ok {
		return
	}
	_ = s.store.UpdateTask(context.Background(), store.Task{
		ID: id, SessionID: t.SessionID, Kind: "dispatch", Status: status,
		LastRunAt: t.UpdatedAt, CreatedAt: t.CreatedAt,
	})
}

// attachInterrupt installs the run's interrupt function. A cancel that
// arrived before the agent was built (fast client) fires immediately.
func (s *Server) attachInterrupt(id string, interrupt func()) {
	s.tasksMu.Lock()
	ts := s.tasks[id]
	s.tasksMu.Unlock()
	if ts == nil {
		return
	}
	ts.mu.Lock()
	ts.interrupt = interrupt
	cancel := ts.cancelRequested
	ts.mu.Unlock()
	if cancel {
		interrupt()
	}
}

// errTaskNotFound marks an unknown task id.
var errTaskNotFound = errors.New("task not found")

// CancelTask interrupts one running dispatch and returns its live state.
// Idempotent: a task that already reached a terminal state is returned
// unchanged, and repeated cancels while running only re-fire the interrupt.
func (s *Server) CancelTask(id string) (api.Task, error) {
	s.tasksMu.Lock()
	ts := s.tasks[id]
	s.tasksMu.Unlock()
	if ts == nil {
		return api.Task{}, errTaskNotFound
	}
	ts.mu.Lock()
	terminal := ts.task.Status == "done" || ts.task.Status == "failed" ||
		ts.task.Status == "interrupted"
	interrupt := ts.interrupt
	if !terminal {
		ts.cancelRequested = true
	}
	ts.mu.Unlock()
	if terminal {
		return ts.snapshot(), nil
	}
	if interrupt != nil {
		interrupt()
	}
	return ts.snapshot(), nil
}

// handleCancelTask cancels one background task
// (POST /api/v1/tasks/{id}/cancel).
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.CancelTask(r.PathValue("id"))
	if errors.Is(err, errTaskNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, t)
}

// updateTask mutates one task under its lock and publishes an output event.
// The SSE payload carries both the task snapshot (id/status) and the
// incremental output text ("text") so TUI/PWA can render a live stream
// without a second fetch.
func (s *Server) updateTask(id string, mutate func(*api.Task)) {
	s.tasksMu.Lock()
	ts := s.tasks[id]
	s.tasksMu.Unlock()
	if ts == nil {
		return
	}
	ts.mu.Lock()
	mutate(&ts.task)
	snapshot := ts.task
	ts.mu.Unlock()
	s.hub.publish(Event{Name: api.EventTaskOutput, Data: map[string]any{
		"id":     snapshot.ID,
		"status": snapshot.Status,
		"text":   snapshot.Reply,
		"err":    snapshot.Err,
	}})
}

// snapshot returns a copy of the task's current state.
func (t *taskState) snapshot() api.Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.task
}

// getTask returns one task's live state and whether it exists.
func (s *Server) getTask(id string) (api.Task, bool) {
	s.tasksMu.Lock()
	ts := s.tasks[id]
	s.tasksMu.Unlock()
	if ts == nil {
		return api.Task{}, false
	}
	return ts.snapshot(), true
}

// handleListTasks lists live background tasks (GET /api/v1/tasks).
func (s *Server) handleListTasks(w http.ResponseWriter, _ *http.Request) {
	s.tasksMu.Lock()
	all := make([]*taskState, 0, len(s.tasks))
	for _, ts := range s.tasks {
		all = append(all, ts)
	}
	s.tasksMu.Unlock()
	out := make([]api.Task, 0, len(all))
	for _, ts := range all {
		out = append(out, ts.snapshot())
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetTask returns one task (GET /api/v1/tasks/{id}).
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.getTask(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleListSessions lists conversation threads (GET /api/v1/sessions).
func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	sessions, err := s.store.ListSessions(context.Background(), 50, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]api.SessionSummary, 0, len(sessions))
	for _, se := range sessions {
		out = append(out, sessionSummary(se))
	}
	writeJSON(w, http.StatusOK, out)
}

// sessionSummary converts one stored session row to the API shape.
func sessionSummary(se store.Session) api.SessionSummary {
	return api.SessionSummary{
		ID: se.ID, ParentID: se.ParentID, Agent: se.Agent,
		Title: se.Title, Model: se.Model, Status: se.Status,
		CostUSD: se.CostUSD, TokensIn: se.TokensIn, TokensOut: se.TokensOut,
		CacheReadTokens: se.CacheReadTokens, ReasoningTokens: se.ReasoningTokens,
		CreatedAt: se.CreatedAt, UpdatedAt: se.UpdatedAt,
	}
}

// handleUpdateSession renames one session (PATCH /api/v1/sessions/{id}).
func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.UpdateSessionRequest
	if err := decodeBodyLimit(w, r, &req, 64<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if len([]rune(title)) > 20 {
		writeError(w, http.StatusBadRequest, "title is too long (max 20 characters)")
		return
	}
	se, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	se.Title = title
	se.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateSession(r.Context(), se); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessionSummary(se))
}

// handleDeleteSession removes one session and its messages
// (DELETE /api/v1/sessions/{id}). A running/pending task must finish first.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.tasksMu.Lock()
	for _, ts := range s.tasks {
		t := ts.snapshot()
		if t.SessionID == id && (t.Status == "pending" || t.Status == "running") {
			s.tasksMu.Unlock()
			writeError(w, http.StatusConflict, "session has a running task; cancel it first")
			return
		}
	}
	s.tasksMu.Unlock()
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleExportSession downloads one session as the portable envelope
// (GET /api/v1/sessions/{id}/export): versioned, full transcript and audit,
// identical to `chyan export` so the file can be re-imported.
func (s *Server) handleExportSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ex, err := store.ExportSession(r.Context(), s.store, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="chyan-session-%s.json"`, id))
	writeJSON(w, http.StatusOK, ex)
}

// handleGetSession returns one session with its transcript. Query parameters:
// limit caps the rows (default 500), tail=1 returns the newest rows instead of
// the oldest (CLI session-switch replay).
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	se, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, 500)
	}
	tail := r.URL.Query().Get("tail") == "1"
	out := api.Session{SessionSummary: sessionSummary(se)}
	if tail {
		msgs, err := s.app.ReadRecentSession(r.Context(), id, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out.Messages = apiMessages(msgs)
		writeJSON(w, http.StatusOK, out)
		return
	}
	msgs, budget, err := s.app.SessionDetail(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx := contextUsage(budget)
	out.Messages = apiMessages(msgs)
	out.Context = &ctx
	writeJSON(w, http.StatusOK, out)
}

// handleSessionContext reports one session's live window usage
// (GET /api/v1/sessions/{id}/context). The Web header reads the same snapshot
// from GET /sessions/{id}; this endpoint stays for direct API/CLI callers.
func (s *Server) handleSessionContext(w http.ResponseWriter, r *http.Request) {
	b, err := s.app.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, contextUsage(b))
}

// contextUsage maps one context budget onto the API shape shared by
// GET /sessions/{id} and GET /sessions/{id}/context.
func contextUsage(b core.Budget) api.ContextUsage {
	return api.ContextUsage{
		ContextLimit:  b.ContextLimit,
		FixedCost:     b.FixedCost,
		HistoryTokens: b.HistoryTokens,
		HistoryBytes:  b.HistoryBytes,
		Used:          b.FixedCost + b.HistoryTokens,
		SoftLimit:     b.SoftLimit,
		HardLimit:     b.HardLimit,
	}
}

// imagePaths returns the artifact pointers of image parts so the Web can show
// what the user attached; reasoning stays internal as visibleText does.
func imagePaths(m core.Message) []string {
	var out []string
	for _, p := range m.Content {
		if p.Type == "image" && p.Image != nil && p.Image.Path != "" {
			out = append(out, p.Image.Path)
		}
	}
	return out
}

// visibleText flattens a message for the Web transcript while dropping
// reasoning parts: thinking is internal and must not leak into the chat UI.
func visibleText(m core.Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if p.Type == "reasoning" || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// handleSearch searches message content across sessions
// (GET /api/v1/search?q=...). The dialog shows the owning session plus a
// snippet, so a title-only match is not enough.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len([]rune(q)) > 100 {
		writeJSON(w, http.StatusOK, []api.SearchHit{})
		return
	}
	const limit = 30
	sessions, err := s.store.ListSessions(r.Context(), 300, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hits := make([]api.SearchHit, 0, limit)
	for _, se := range sessions {
		msgs, err := s.app.ReadSession(r.Context(), se.ID, 500)
		if err != nil {
			continue // one unreadable session cannot break the dialog
		}
		for _, m := range msgs {
			// Tool rows are model-facing JSON (call arguments / results): a hit
			// would surface unreadable payload in the dialog. Conversation only.
			if m.Role == core.RoleTool {
				continue
			}
			flat := strings.Join(strings.Fields(core.FlattenText(m)), " ")
			snippet, ok := matchSnippet(flat, q)
			if !ok {
				continue
			}
			hits = append(hits, api.SearchHit{
				SessionID: se.ID, Title: se.Title, Model: se.Model,
				Role: string(m.Role), Snippet: snippet,
				CreatedAt: m.Timestamp, UpdatedAt: se.UpdatedAt,
			})
			if len(hits) >= limit {
				break
			}
		}
		if len(hits) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, hits)
}

// matchSnippet returns a one-line window around the query. Matching is
// case-insensitive and rune-safe so CJK text is never cut mid-character.
func matchSnippet(text, query string) (string, bool) {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, strings.ToLower(query))
	if idx < 0 {
		return "", false
	}
	runes := []rune(text)
	before := utf8.RuneCountInString(text[:idx])
	matched := utf8.RuneCountInString(text[idx : idx+len(query)])
	start := before - 40
	if start < 0 {
		start = 0
	}
	end := before + matched + 60
	if end > len(runes) {
		end = len(runes)
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out, true
}

// --------------------------------------------------------------- approvals

// handleListApprovals lists pending approval tickets (GET /api/v1/approvals).
// Tickets live in the gate; the daemon view is a thin bridge.
func (s *Server) handleListApprovals(w http.ResponseWriter, _ *http.Request) {
	if s.gate == nil {
		writeJSON(w, http.StatusOK, []api.ApprovalTicket{})
		return
	}
	writeJSON(w, http.StatusOK, s.gate.Pending())
}

// handleResolveApproval approves or denies one ticket
// (POST /api/v1/approvals/{id}).
func (s *Server) handleResolveApproval(w http.ResponseWriter, r *http.Request) {
	var dec api.ApprovalDecision
	if err := decodeBody(w, r, &dec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if dec.Decision != "approve" && dec.Decision != "deny" {
		writeError(w, http.StatusBadRequest, "decision must be approve|deny")
		return
	}
	if s.gate == nil {
		writeError(w, http.StatusNotImplemented, "approval gate does not support remote resolution")
		return
	}
	// approval.resolved is published by the gate (ApprovalHub) itself.
	if err := s.gate.Resolve(r.PathValue("id"), dec.Decision, dec.Reason); err != nil {
		// An already-resolved ticket (double click, another device) is a state
		// conflict, not a malformed request: clients treat 409 as "已处理" and
		// refresh instead of showing the raw error.
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not pending") {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dec)
}

// -------------------------------------------------------------------- files

// errOutsideWorkDir marks a path that resolves outside the work directory.
var errOutsideWorkDir = errors.New("path outside work dir")

// resolveFile maps a client path to a real file inside the work dir and returns
// the cleaned relative path for responses. Symlinks are resolved and checked
// again: the lexical join alone lets a link inside the tree expose /etc.
func (s *Server) resolveFile(root, rel string) (real, clean string, err error) {
	if r, rerr := filepath.EvalSymlinks(root); rerr == nil {
		root = r
	}
	if filepath.IsAbs(rel) {
		// Absolute paths (artifact markers) still have to resolve inside root.
		real, err = filepath.EvalSymlinks(filepath.Clean(rel))
		if err != nil {
			return "", "", err
		}
		clean, err = filepath.Rel(root, real)
		if err != nil {
			return "", "", errOutsideWorkDir
		}
		clean = filepath.ToSlash(clean)
	} else {
		clean = strings.TrimPrefix(filepath.Clean("/"+rel), "/")
		real, err = filepath.EvalSymlinks(filepath.Join(root, clean))
		if err != nil {
			return "", clean, err
		}
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", clean, errOutsideWorkDir
	}
	return real, clean, nil
}

// handleListFiles lists the read-only project file tree (GET /api/v1/files).
func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	root, err := s.workspaceRoot(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	real, rel, err := s.resolveFile(root, r.URL.Query().Get("path"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errOutsideWorkDir) {
			status = http.StatusForbidden
		}
		writeError(w, status, err.Error())
		return
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]api.FileNode, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		size := int64(0)
		if err == nil {
			size = info.Size()
		}
		out = append(out, api.FileNode{
			Name: e.Name(), Path: filepath.Join(rel, e.Name()),
			IsDir: e.IsDir(), Size: size,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFileContent returns one file's preview (GET /api/v1/files/content).
func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	root, err := s.workspaceRoot(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	real, rel, err := s.resolveFile(root, r.URL.Query().Get("path"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errOutsideWorkDir) {
			status = http.StatusForbidden
		}
		writeError(w, status, err.Error())
		return
	}
	const maxPreview = 256 << 10
	f, err := os.Open(real)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Read at most maxPreview bytes: a huge file must never be loaded whole
	// just to be truncated for preview.
	data, err := io.ReadAll(io.LimitReader(f, maxPreview))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.FileContent{
		Path: rel, Content: string(data), Trunc: info.Size() > maxPreview,
	})
}

// handleFileDownload streams one file as an attachment
// (GET /api/v1/files/download). Paths obey the same workspace sandbox.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	root, err := s.workspaceRoot(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	real, rel, err := s.resolveFile(root, r.URL.Query().Get("path"))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errOutsideWorkDir) {
			status = http.StatusForbidden
		}
		writeError(w, status, err.Error())
		return
	}
	f, err := os.Open(real)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "directory download is not supported")
		return
	}
	name := attachmentName(filepath.Base(rel))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// attachmentName keeps a user-controlled filename safe inside a
// Content-Disposition header: quotes, backslashes and control characters
// would break the header or inject parameters.
func attachmentName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return name
}

// --------------------------------------------------------------- status/SSE

// runningTaskCount returns pending + running dispatches (global header cue).
func (s *Server) runningTaskCount() int {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	n := 0
	for _, ts := range s.tasks {
		if st := ts.snapshot().Status; st == "pending" || st == "running" {
			n++
		}
	}
	return n
}

// handleStatus reports daemon-wide observability (GET /api/v1/status).
// Context budget fields reflect the most recent session's window usage.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	sessions, _ := s.store.ListSessions(context.Background(), 1000, 0)
	pending := 0
	if s.gate != nil {
		pending = len(s.gate.Pending())
	}
	out := api.Status{
		Version: "0.1.0-dev", Sessions: len(sessions), PendingApprovals: pending,
		RunningTasks: s.runningTaskCount(), ReviewMode: string(s.app.ReviewMode()),
		MCPServers: mcpStatuses(s.app.MCPAvailable(), s.app.MCPToolCounts()),
	}
	// Budget is per-session; report the latest session's usage when present.
	if len(sessions) > 0 {
		if b, err := s.app.Status(context.Background(), sessions[0].ID); err == nil {
			out.ContextLimit = b.ContextLimit
			out.FixedCost = b.FixedCost
			out.HardLimit = b.HardLimit
			out.SoftLimit = b.SoftLimit
			out.HistoryTokens = b.HistoryTokens
			out.HistoryBytes = b.HistoryBytes
		}
	}
	writeJSON(w, http.StatusOK, out)
}

package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
)

// waitTask polls one task until a terminal state.
func waitTask(t *testing.T, h http.Handler, id string) api.Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w, req := doJSON(t, http.MethodGet, "/api/v1/tasks/"+id, nil, "test-token")
		h.ServeHTTP(w, req)
		var got api.Task
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if terminalStatus(got.Status) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", id)
	return api.Task{}
}

// terminalStatus mirrors the task lifecycle for test polling.
func terminalStatus(s string) bool { return s == "done" || s == "failed" || s == "interrupted" }

// TestCreateSessionEndpoint: POST /sessions returns a real id and an empty
// transcript, so the CLI can record shell notes before the first turn.
func TestCreateSessionEndpoint(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/sessions", api.CreateSessionRequest{}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session = %d body=%s", w.Code, w.Body.String())
	}
	var se api.SessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &se); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(se.ID, "ses") {
		t.Fatalf("session id = %q", se.ID)
	}

	w, req = doJSON(t, http.MethodGet, "/api/v1/sessions/"+se.ID, nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get session = %d", w.Code)
	}
	var full api.Session
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	if len(full.Messages) != 0 {
		t.Errorf("new session messages = %+v", full.Messages)
	}
}

// TestAppendNoteEndpoint: the "!" shell escape records a user message.
func TestAppendNoteEndpoint(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/sessions", api.CreateSessionRequest{}, "test-token")
	h.ServeHTTP(w, req)
	var se api.SessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &se); err != nil {
		t.Fatal(err)
	}

	w, req = doJSON(t, http.MethodPost, "/api/v1/sessions/"+se.ID+"/note",
		api.AppendNoteRequest{Command: "echo hi", Output: "hi\n"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("note = %d body=%s", w.Code, w.Body.String())
	}
	var res api.AppendNoteResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.SessionID != se.ID {
		t.Errorf("note session = %q, want %q", res.SessionID, se.ID)
	}

	w, req = doJSON(t, http.MethodGet, "/api/v1/sessions/"+se.ID, nil, "test-token")
	h.ServeHTTP(w, req)
	var full api.Session
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	if len(full.Messages) != 1 || !strings.Contains(full.Messages[0].Text, "[shell] $ echo hi") {
		t.Fatalf("messages = %+v", full.Messages)
	}

	// Missing command is a client error.
	w, req = doJSON(t, http.MethodPost, "/api/v1/sessions/"+se.ID+"/note",
		api.AppendNoteRequest{Output: "x"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty command = %d", w.Code)
	}
}

// TestSessionTranscriptLimitTail: limit caps rows; tail returns the newest
// rows (CLI replay) instead of the oldest (title fallback).
func TestSessionTranscriptLimitTail(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/sessions", api.CreateSessionRequest{}, "test-token")
	h.ServeHTTP(w, req)
	var se api.SessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &se); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"cmd-one", "cmd-two", "cmd-three"} {
		w, req = doJSON(t, http.MethodPost, "/api/v1/sessions/"+se.ID+"/note",
			api.AppendNoteRequest{Command: cmd}, "test-token")
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("note %s = %d", cmd, w.Code)
		}
	}

	get := func(query string) api.Session {
		t.Helper()
		w, req := doJSON(t, http.MethodGet, "/api/v1/sessions/"+se.ID+query, nil, "test-token")
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("get %s = %d", query, w.Code)
		}
		var full api.Session
		if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
			t.Fatal(err)
		}
		return full
	}
	first := get("?limit=1")
	if len(first.Messages) != 1 || !strings.Contains(first.Messages[0].Text, "cmd-one") {
		t.Fatalf("head = %+v", first.Messages)
	}
	latest := get("?limit=1&tail=1")
	if len(latest.Messages) != 1 || !strings.Contains(latest.Messages[0].Text, "cmd-three") {
		t.Fatalf("tail = %+v", latest.Messages)
	}
	if latest.Context != nil {
		t.Error("tail responses skip the context budget")
	}
}

// TestCompactEmptySession: compacting a fresh session needs no LLM and returns
// an empty note.
func TestCompactEmptySession(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/sessions", api.CreateSessionRequest{}, "test-token")
	h.ServeHTTP(w, req)
	var se api.SessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &se); err != nil {
		t.Fatal(err)
	}
	w, req = doJSON(t, http.MethodPost, "/api/v1/sessions/"+se.ID+"/compact", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("compact = %d body=%s", w.Code, w.Body.String())
	}
	var res api.CompactResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Note.Intent != "" || res.Note.Summary != "" {
		t.Errorf("empty session note = %+v", res.Note)
	}
}

// TestCreateTaskContinueResolves: "continue" is normalized before dispatch, so
// the accepted task carries a real session id (CLI --continue).
func TestCreateTaskContinueResolves(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "hello", SessionID: "continue"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create = %d body=%s", w.Code, w.Body.String())
	}
	var task api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.SessionID == "" || task.SessionID == "continue" {
		t.Fatalf("session id = %q", task.SessionID)
	}
	if done := waitTask(t, h, task.ID); done.Status != "done" {
		t.Fatalf("task = %+v", done)
	}
}

// TestCreateTaskFork: ForkFrom copies the source session and runs there.
func TestCreateTaskFork(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "first"}, "test-token")
	h.ServeHTTP(w, req)
	var first api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	first = waitTask(t, h, first.ID)
	if first.Status != "done" || first.SessionID == "" {
		t.Fatalf("first task = %+v", first)
	}

	w, req = doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "branch", ForkFrom: first.SessionID}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("fork = %d body=%s", w.Code, w.Body.String())
	}
	var fork api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &fork); err != nil {
		t.Fatal(err)
	}
	if fork.SessionID == first.SessionID {
		t.Fatalf("fork reused session %q", fork.SessionID)
	}
	if done := waitTask(t, h, fork.ID); done.Status != "done" {
		t.Fatalf("fork task = %+v", done)
	}
}

// TestCreateTaskRejectsForkWithSession: forking and continuing an explicit
// session are contradictory; reject instead of silently dropping one.
func TestCreateTaskRejectsForkWithSession(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "x", SessionID: "ses1", ForkFrom: "ses2"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("fork+session = %d", w.Code)
	}
}

// TestCreateTaskRejectsUnknownPhase keeps planner/executor as the only values.
func TestCreateTaskRejectsUnknownPhase(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "x", Phase: "warp"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("phase validation = %d", w.Code)
	}
}

// TestCommandsEndpoint returns a JSON array even when no project commands load.
func TestCommandsEndpoint(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodGet, "/api/v1/commands", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("commands = %d", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Errorf("commands body = %q, want []", body)
	}
}

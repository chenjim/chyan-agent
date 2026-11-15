// traces_test.go — span API for the Web trace viewer (FR-13/U4).
package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/internal/trace"
)

// newTraceServer builds a server over the real SQLite store (the in-memory
// store does not persist spans) with two recorded turns in one session.
func newTraceServer(t *testing.T) (*Server, store.Store, time.Time) {
	t.Helper()
	home := t.TempDir()
	st, err := store.NewSQLite(filepath.Join(home, "chyan.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	a, err := app.New(app.Options{
		SkipExtensions: true, SkipGate: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: &stubBackend{reply: "ok"}, Store: st,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{Home: home, App: a, Store: st, Token: "test-token", Gate: &stubGate{}})
	if err != nil {
		t.Fatalf("daemon New: %v", err)
	}

	base := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	write := func(traceID, kind, name string, offset time.Duration, dur time.Duration) {
		sp := trace.Span{
			ID: traceID + "-" + kind, TraceID: traceID, Kind: trace.Kind(kind), Name: name,
			SessionID: "ses-a", Start: base.Add(offset), End: base.Add(offset + dur),
			Status: trace.StatusOK, Attrs: map[string]any{"tokens_in": 10},
		}
		if err := st.AppendSpan(t.Context(), sp); err != nil {
			t.Fatalf("append span: %v", err)
		}
	}
	// Older turn (t1) plus a newer one (t2) so ordering is observable.
	write("t1", "turn", "turn", 0, 2*time.Second)
	write("t1", "llm_request", "primary", 100*time.Millisecond, 900*time.Millisecond)
	write("t2", "turn", "turn", time.Minute, 3*time.Second)
	write("t2", "tool_execution", "bash", time.Minute+time.Second, time.Second)
	return s, st, base
}

func getJSON(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	w, req := doJSON(t, http.MethodGet, url, nil, "test-token")
	h.ServeHTTP(w, req)
	return w
}

func TestTracesGroupsTurnsNewestFirst(t *testing.T) {
	s, _, _ := newTraceServer(t)
	w := getJSON(t, s.Handler(), "/api/v1/traces?session_id=ses-a")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp api.TraceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(resp.Turns) != 2 {
		t.Fatalf("turns = %d, want 2 (%s)", len(resp.Turns), w.Body.String())
	}
	if resp.Turns[0].Summary.TraceID != "t2" {
		t.Fatalf("first turn = %s, want the newest (t2)", resp.Turns[0].Summary.TraceID)
	}
	if got := len(resp.Turns[0].Spans); got != 2 {
		t.Fatalf("t2 spans = %d", got)
	}
	// Spans inside a turn keep start order (turn before the tool it ran).
	if resp.Turns[0].Spans[0].Kind != trace.KindTurn {
		t.Fatalf("span order = %+v", resp.Turns[0].Spans)
	}
	if resp.Turns[0].Summary.DurationMS != 3000 {
		t.Fatalf("duration = %d, want 3000", resp.Turns[0].Summary.DurationMS)
	}
}

func TestTracesByTraceIDAndTask(t *testing.T) {
	s, _, _ := newTraceServer(t)
	h := s.Handler()
	w := getJSON(t, h, "/api/v1/traces?trace_id=t1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp api.TraceResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Turns) != 1 || resp.Turns[0].Summary.TraceID != "t1" {
		t.Fatalf("trace filter = %+v", resp.Turns)
	}
	// An unknown task id is empty, not an error.
	w = getJSON(t, h, "/api/v1/traces?task_id=task-x")
	if w.Code != http.StatusOK || w.Body.String() != "{\"turns\":[]}\n" {
		t.Fatalf("empty task = %d %s", w.Code, w.Body.String())
	}
}

func TestTracesRequiresSelectorAndValidLimit(t *testing.T) {
	s, _, _ := newTraceServer(t)
	h := s.Handler()
	if w := getJSON(t, h, "/api/v1/traces"); w.Code != http.StatusBadRequest {
		t.Fatalf("no selector status = %d", w.Code)
	}
	if w := getJSON(t, h, "/api/v1/traces?session_id=ses-a&limit=0"); w.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 status = %d", w.Code)
	}
	if w := getJSON(t, h, "/api/v1/traces?session_id=ses-a&limit=abc"); w.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc status = %d", w.Code)
	}
	// Limit 1 keeps the most recent span, i.e. the newest turn (t2), never the
	// oldest one: a long session must not lose its latest activity.
	w := getJSON(t, h, "/api/v1/traces?session_id=ses-a&limit=1")
	if w.Code != http.StatusOK {
		t.Fatalf("limit=1 status = %d", w.Code)
	}
	var limited api.TraceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &limited); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(limited.Turns) != 1 || limited.Turns[0].Summary.TraceID != "t2" {
		t.Fatalf("limit=1 turns = %+v, want the newest turn only", limited.Turns)
	}
}

func TestTracesEmptyForStoreWithoutSpans(t *testing.T) {
	s, _, _ := newTraceServer(t)
	w := getJSON(t, s.Handler(), "/api/v1/traces?session_id=ses-missing")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp api.TraceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 0 {
		t.Fatalf("turns = %+v, want empty", resp.Turns)
	}
}

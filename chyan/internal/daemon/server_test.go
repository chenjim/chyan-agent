package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/llm"
	"chyan.tech/chyan/internal/store"
)

// TestMain forces the offline token counter before any test runs an agent
// loop, so tests never trigger the tiktoken BPE download (hermetic CI).
func TestMain(m *testing.M) {
	llm.ForceHeuristicForTest()
	os.Exit(m.Run())
}

// stubBackend scripts LLM replies for daemon tests.
type stubBackend struct{ reply string }

func (s *stubBackend) Chat(_ context.Context, _ core.Request) (core.Response, error) {
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: s.reply}}}}, nil
}

func (s *stubBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := s.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: s.reply})
	}
	return r, err
}

// stubGate records resolved tickets (test-friendly ApprovalGate fake).
type stubGate struct {
	mu       sync.Mutex
	tickets  []api.ApprovalTicket
	resolved map[string]string
}

func (g *stubGate) Pending() []api.ApprovalTicket {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]api.ApprovalTicket(nil), g.tickets...)
}

func (g *stubGate) Resolve(id, decision, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resolved == nil {
		g.resolved = map[string]string{}
	}
	g.resolved[id] = decision
	return nil
}

func newTestServer(t *testing.T) (*Server, *app.App, store.Store) {
	return newTestServerWithGate(t, &stubGate{})
}

// newTestServerWithGate builds a test server with a custom ApprovalGate.
func newTestServerWithGate(t *testing.T, gate ApprovalGate) (*Server, *app.App, store.Store) {
	t.Helper()
	home := t.TempDir()
	workDir := t.TempDir()
	st := store.NewMemory()
	a, err := app.New(app.Options{
		SkipExtensions: true,
		Home:           home, MemoryRoot: t.TempDir(), WorkDir: workDir,
		Backend: &stubBackend{reply: "stub reply"}, Store: st,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{
		Home: home, App: a, Store: st, Token: "test-token", Gate: gate})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, a, st
}

func doJSON(t *testing.T, method, url string, body any, token string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, url, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	return w, req
}

func TestCreateAndPollTask(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{Prompt: "hello"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create task status = %d body=%s", w.Code, w.Body.String())
	}
	var task api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.ID == "" {
		t.Fatal("task id missing")
	}

	// Poll until done (the stub reply is synchronous).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w, req = doJSON(t, http.MethodGet, "/api/v1/tasks/"+task.ID, nil, "test-token")
		h.ServeHTTP(w, req)
		var got api.Task
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status == "done" {
			if got.Reply != "stub reply" {
				t.Errorf("reply = %q", got.Reply)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not complete in time")
}

func TestCreateTaskValidation(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{Prompt: "  "}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt status = %d", w.Code)
	}
	// Bad JSON body.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer test-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad json status = %d", w.Code)
	}
}

func TestTokenAuth(t *testing.T) {
	s, _, _ := newTestServer(t)
	handler := s.authMiddleware(s.Handler())

	// Missing token.
	w, req := doJSON(t, http.MethodGet, "/api/v1/status", nil, "")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", w.Code)
	}
	// Wrong token.
	w, req = doJSON(t, http.MethodGet, "/api/v1/status", nil, "wrong")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", w.Code)
	}
	// Correct token.
	w, req = doJSON(t, http.MethodGet, "/api/v1/status", nil, "test-token")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("good token status = %d", w.Code)
	}
}

// TestAuthMiddlewareServesPWAShell: non-/api/ paths (the embedded PWA shell)
// stay open so a browser can load the app before any token is attached;
// data calls under /api/ remain guarded.
func TestAuthMiddlewareServesPWAShell(t *testing.T) {
	s, _, _ := newTestServer(t)
	handler := s.authMiddleware(s.Handler())

	for _, path := range []string{"/", "/index.html", "/assets/app.js"} {
		w, req := doJSON(t, http.MethodGet, path, nil, "")
		handler.ServeHTTP(w, req)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("%s must serve the shell without auth, got 401", path)
		}
	}
	// The API stays guarded even when the shell is open.
	w, req := doJSON(t, http.MethodGet, "/api/v1/tasks", nil, "")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/ without token = %d, want 401", w.Code)
	}
}

func TestListAndGetTask(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodGet, "/api/v1/tasks", nil, "test-token")
	h.ServeHTTP(w, req)
	var tasks []api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("initial tasks = %d", len(tasks))
	}
	// Unknown task -> 404.
	w, req = doJSON(t, http.MethodGet, "/api/v1/tasks/ghost", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown task status = %d", w.Code)
	}
}

func TestSessionsEndpoints(t *testing.T) {
	s, _, st := newTestServer(t)
	h := s.Handler()
	// Seed one session with a message.
	if err := st.CreateSession(context.Background(), store.Session{ID: "s1", Title: "t", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMessage(context.Background(), store.Message{
		ID: "m1", SessionID: "s1", Role: "user",
		Content: marshal(`{"content":[{"Type":"text","Text":"hi"}]}`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w, req := doJSON(t, http.MethodGet, "/api/v1/sessions", nil, "test-token")
	h.ServeHTTP(w, req)
	var list []api.SessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "s1" {
		t.Fatalf("sessions = %+v", list)
	}

	w, req = doJSON(t, http.MethodGet, "/api/v1/sessions/s1", nil, "test-token")
	h.ServeHTTP(w, req)
	var ses api.Session
	if err := json.Unmarshal(w.Body.Bytes(), &ses); err != nil {
		t.Fatal(err)
	}
	if len(ses.Messages) != 1 || ses.Messages[0].Text != "hi" {
		t.Fatalf("session messages = %+v", ses.Messages)
	}

	// Unknown session -> 404.
	w, req = doJSON(t, http.MethodGet, "/api/v1/sessions/ghost", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown session status = %d", w.Code)
	}
}

func marshal(s string) []byte { return []byte(s) }

func TestApprovalsEndpoints(t *testing.T) {
	s, _, _ := newTestServer(t)
	gate := s.gate.(*stubGate)
	h := s.Handler()

	w, req := doJSON(t, http.MethodGet, "/api/v1/approvals", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list approvals status = %d", w.Code)
	}

	// Approve one ticket.
	w, req = doJSON(t, http.MethodPost, "/api/v1/approvals/t1",
		api.ApprovalDecision{Decision: "approve"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve status = %d body=%s", w.Code, w.Body.String())
	}

	// Invalid decision.
	w, req = doJSON(t, http.MethodPost, "/api/v1/approvals/t1",
		api.ApprovalDecision{Decision: "maybe"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid decision status = %d", w.Code)
	}
	if gate.resolved["t1"] != "approve" {
		t.Errorf("resolved = %v", gate.resolved)
	}
}

func TestFilesEndpoints(t *testing.T) {
	s, a, _ := newTestServer(t)
	h := s.Handler()
	// Seed a file in the work dir.
	if err := os.WriteFile(filepath.Join(a.WorkDir(), "hello.txt"), []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, req := doJSON(t, http.MethodGet, "/api/v1/files?path=", nil, "test-token")
	h.ServeHTTP(w, req)
	var nodes []api.FileNode
	if err := json.Unmarshal(w.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range nodes {
		if n.Name == "hello.txt" && !n.IsDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("file tree = %+v", nodes)
	}

	w, req = doJSON(t, http.MethodGet, "/api/v1/files/content?path=hello.txt", nil, "test-token")
	h.ServeHTTP(w, req)
	var content api.FileContent
	if err := json.Unmarshal(w.Body.Bytes(), &content); err != nil {
		t.Fatal(err)
	}
	if content.Content != "hello world" {
		t.Errorf("content = %q", content.Content)
	}

	// An oversized file is previewed at the byte cap and flagged as truncated.
	big := strings.Repeat("x", (256<<10)+10)
	if err := os.WriteFile(filepath.Join(a.WorkDir(), "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	w, req = doJSON(t, http.MethodGet, "/api/v1/files/content?path=big.txt", nil, "test-token")
	h.ServeHTTP(w, req)
	var preview api.FileContent
	if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Trunc {
		t.Errorf("Trunc = false, want true for %d-byte file", len(big))
	}
	if len(preview.Content) != 256<<10 {
		t.Errorf("preview length = %d, want %d", len(preview.Content), 256<<10)
	}

	// A missing file is a 400, not a panic or an empty preview.
	w, req = doJSON(t, http.MethodGet, "/api/v1/files/content?path=missing.txt", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing file status = %d, want 400", w.Code)
	}

	// Path traversal must be rejected.
	w, req = doJSON(t, http.MethodGet, "/api/v1/files/content?path=../../etc/passwd", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("path traversal must be rejected")
	}

	// A symlink inside the work dir must not expose a file outside it.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(a.WorkDir(), "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	w, req = doJSON(t, http.MethodGet, "/api/v1/files/content?path=link.txt", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("symlink escape status = %d, want 403", w.Code)
	}
}

// TestCreateTaskBodyLimit: an oversized JSON body is refused instead of
// buffering unbounded memory.
func TestCreateTaskBodyLimit(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	big := `{"prompt":"` + strings.Repeat("x", maxBodyBytes+1024) + `"}`
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", strings.NewReader(big), "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", w.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	w, req := doJSON(t, http.MethodGet, "/api/v1/status", nil, "test-token")
	h.ServeHTTP(w, req)
	var st api.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Version == "" {
		t.Error("status must carry a version")
	}
}

func TestSSEStreamDeliversEvents(t *testing.T) {
	s, _, _ := newTestServer(t)
	// Publish one event and verify the SSE frame shape via the hub.
	ch, cancel := s.hub.subscribe()
	defer cancel()
	s.hub.publish(Event{Name: api.EventTaskDone, Data: api.Task{ID: "t1", Status: "done"}})
	select {
	case e := <-ch:
		if e.Name != api.EventTaskDone {
			t.Errorf("event name = %q", e.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("SSE subscriber missed the event")
	}
}

// sseRecorder is an http.ResponseWriter that also implements http.Flusher so
// serveSSE can stream; it captures the frames written before the request is
// cancelled.
type sseRecorder struct {
	hdr     http.Header
	buf     bytes.Buffer
	flushed int
}

func (r *sseRecorder) Header() http.Header {
	if r.hdr == nil {
		r.hdr = http.Header{}
	}
	return r.hdr
}
func (r *sseRecorder) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *sseRecorder) WriteHeader(int)             {}
func (r *sseRecorder) Flush()                      { r.flushed++ }

func TestServeSSERealStream(t *testing.T) {
	s, _, _ := newTestServer(t)
	rec := &sseRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		s.serveSSEAuthed(rec, req)
		close(done)
	}()

	// Publish after the handler is registered.
	time.Sleep(50 * time.Millisecond)
	s.hub.publish(Event{Name: api.EventTaskStarted, Data: api.Task{ID: "t2", Status: "running"}})
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveSSE did not return after cancellation")
	}
	out := rec.buf.String()
	if !strings.Contains(out, "event: "+api.EventTaskStarted) || !strings.Contains(out, `"id":"t2"`) {
		t.Errorf("SSE frame = %q", out)
	}
	if rec.flushed == 0 {
		t.Error("serveSSE must flush frames")
	}
}

func TestServeSSERejectsNonFlusher(t *testing.T) {
	s, _, _ := newTestServer(t)
	// A ResponseRecorder without Flatcher support is not reachable through
	// httptest (it implements Flush), so exercise the guard directly with a
	// bare writer.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	s.serveSSEAuthed(noflushWriter{}, req)
}

// noflushWriter is an http.ResponseWriter without http.Flusher.
type noflushWriter struct{ hdr http.Header }

func (w noflushWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w noflushWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w noflushWriter) WriteHeader(int)             {}

func TestFailTaskPath(t *testing.T) {
	s, _, _ := newTestServer(t)
	// Seed a task then fail it explicitly.
	s.tasksMu.Lock()
	s.tasks["f1"] = &taskState{task: api.Task{ID: "f1", Status: "running"}}
	s.tasksMu.Unlock()
	s.failTask("f1", "sess-1", errBoom)
	tk, ok := s.getTask("f1")
	if !ok || tk.Status != "failed" || tk.Err == "" {
		t.Errorf("failed task = %+v (exists=%v)", tk, ok)
	}
	// A failed run still binds its conversation so the transcript can show it.
	if tk.SessionID != "sess-1" {
		t.Errorf("session id = %q, want sess-1", tk.SessionID)
	}
	// Failing an unknown task is a no-op.
	s.failTask("ghost", "", errBoom)
}

var errBoom = context.DeadlineExceeded

func TestNormalizeTaskError(t *testing.T) {
	got := normalizeTaskError(core.ErrApprovalTimeout, time.Minute)
	if got == nil || !strings.Contains(got.Error(), "审批超时") {
		t.Fatalf("approval timeout = %v, want a user-facing timeout message", got)
	}
	got = normalizeTaskError(context.DeadlineExceeded, time.Minute)
	if got == nil || !strings.Contains(got.Error(), "timed out after 1m0s") {
		t.Fatalf("task timeout = %v", got)
	}
	other := errors.New("boom")
	if err := normalizeTaskError(other, time.Minute); !errors.Is(err, other) {
		t.Fatalf("unknown error = %v, want passthrough", err)
	}
}

func TestStartBindsListeners(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() { _ = s.Start(ctx) }()
	// The unix socket must appear under Home.
	sockPath := filepath.Join(s.sockDir, "chyan.sock")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("unix socket was not created")
}

func TestStartWithoutTokenSkipsRemote(t *testing.T) {
	home := t.TempDir()
	a, err := app.New(app.Options{
		SkipExtensions: true,
		Home:           home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: &stubBackend{reply: "x"}, Store: store.NewMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s, err := New(Options{
		Home: home, App: a, Store: store.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() { _ = s.Start(ctx) }()
	// Local socket still comes up; remote is disabled without a token.
	sockPath := filepath.Join(s.sockDir, "chyan.sock")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			if s.srvHTTP != nil {
				t.Error("remote listener must be nil without a token")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("unix socket was not created")
}

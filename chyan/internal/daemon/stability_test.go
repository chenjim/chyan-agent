// Daemon stability (P6-R7): queue cap, per-task timeout, restart recovery.
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// poolBackend blocks up to max concurrent Chat calls until release is closed,
// then answers instantly. max records the observed peak.
type poolBackend struct {
	mu      sync.Mutex
	cur     int
	max     int
	started chan struct{}
	release chan struct{}
}

func newPoolBackend() *poolBackend {
	return &poolBackend{started: make(chan struct{}, 32), release: make(chan struct{})}
}

func (b *poolBackend) Chat(ctx context.Context, _ core.Request) (core.Response, error) {
	b.mu.Lock()
	b.cur++
	if b.cur > b.max {
		b.max = b.cur
	}
	b.mu.Unlock()

	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		b.done()
		return core.Response{}, ctx.Err()
	}
	b.done()
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: "ok"}}}}, nil
}

func (b *poolBackend) done() {
	b.mu.Lock()
	b.cur--
	b.mu.Unlock()
}

func (b *poolBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: "ok"})
	}
	return r, err
}

func (b *poolBackend) peak() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max
}

// newStabilityServer builds a daemon with explicit limits.
func newStabilityServer(t *testing.T, backend core.LLMBackend, maxTasks int, timeout time.Duration) (*Server, store.Store) {
	t.Helper()
	home := t.TempDir()
	st := store.NewMemory()
	a, err := app.New(app.Options{
		SkipExtensions: true, SkipGate: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: backend, Store: st,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{
		Home: home, App: a, Store: st,
		MaxConcurrentTasks: maxTasks, TaskTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, st
}

func createTask(t *testing.T, h http.Handler) api.Task {
	t.Helper()
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{Prompt: "work"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create task status = %d body=%s", w.Code, w.Body.String())
	}
	var task api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	return task
}

// A new dispatch resolves its conversation before returning, so the client can
// navigate straight to the transcript without waiting for the run to finish.
func TestCreateTaskBindsNewSession(t *testing.T) {
	s, st := newStabilityServer(t, newPoolBackend(), 1, time.Minute)
	task := createTask(t, s.Handler())
	if task.SessionID == "" {
		t.Fatal("new task should carry its session id")
	}
	if _, err := st.GetSession(context.Background(), task.SessionID); err != nil {
		t.Fatalf("session %q not persisted: %v", task.SessionID, err)
	}
}

// Streaming deltas carry their session id so a client can route them to the
// open transcript instead of guessing which task is streaming.
func TestTaskOutputStreamCarriesSession(t *testing.T) {
	s, _ := newStabilityServer(t, &stubBackend{reply: "streamed"}, 1, time.Minute)
	events, unsub := s.hub.subscribe()
	defer unsub()
	task := createTask(t, s.Handler())
	deadline := time.After(5 * time.Second)
	var seen []string
	for {
		select {
		case ev := <-events:
			seen = append(seen, ev.Name)
			if ev.Name != api.EventTaskOutput {
				continue
			}
			data, ok := ev.Data.(map[string]any)
			if !ok || data["session_id"] != task.SessionID {
				continue
			}
			if text, _ := data["text"].(string); text == "streamed" {
				return
			}
		case <-deadline:
			t.Fatalf("no streamed task.output for session %q; seen=%v", task.SessionID, seen)
		}
	}
}

// Retry replays the session's last user turn instead of appending a second
// copy, so a failed attempt does not duplicate history.
func TestRetryReplaysLastUserTurn(t *testing.T) {
	s, st := newStabilityServer(t, &stubBackend{reply: "retried"}, 1, time.Minute)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.CreateSession(ctx, store.Session{
		ID: "s-retry", Title: "retry", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMessage(ctx, store.Message{
		ID: "m1", SessionID: "s-retry", Role: "user",
		Content: []byte(`[{"type":"text","text":"hi"}]`), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "hi", SessionID: "s-retry", Retry: true}, "test-token")
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d body=%s", w.Code, w.Body.String())
	}
	var created api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if done := waitTaskDone(t, s, created.ID); done.Status != "done" {
		t.Fatalf("retry task = %+v", done)
	}
	msgs, err := s.app.ReadSession(ctx, "s-retry", 100)
	if err != nil {
		t.Fatal(err)
	}
	users, assistants := 0, 0
	for _, m := range msgs {
		switch m.Role {
		case core.RoleUser:
			users++
		case core.RoleAssistant:
			if core.FlattenText(m) != "" {
				assistants++
			}
		}
	}
	if users != 1 || assistants == 0 {
		t.Fatalf("retry duplicated history: users=%d assistants=%d", users, assistants)
	}
}

// TestConcurrentTaskCapQueues checks the slot cap: extra dispatches wait as
// pending and never run in parallel beyond the limit.
func TestConcurrentTaskCapQueues(t *testing.T) {
	backend := newPoolBackend()
	s, _ := newStabilityServer(t, backend, 2, time.Minute)
	h := s.Handler()

	var created []api.Task
	for i := 0; i < 5; i++ {
		created = append(created, createTask(t, h))
	}

	// Two tasks must reach the LLM; the rest stay queued.
	for i := 0; i < 2; i++ {
		select {
		case <-backend.started:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d tasks reached the backend", i)
		}
	}
	select {
	case <-backend.started:
		t.Fatal("a third task started despite cap=2")
	case <-time.After(150 * time.Millisecond):
	}

	w, req := doJSON(t, http.MethodGet, "/api/v1/tasks", nil, "test-token")
	h.ServeHTTP(w, req)
	var live []api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &live); err != nil {
		t.Fatal(err)
	}
	running, pending := 0, 0
	for _, task := range live {
		switch task.Status {
		case "running":
			running++
		case "pending":
			pending++
		}
	}
	if running != 2 || pending != 3 {
		t.Fatalf("running=%d pending=%d, want 2/3", running, pending)
	}

	close(backend.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := 0
		for _, c := range created {
			if task, ok := s.getTask(c.ID); ok && task.Status == "done" {
				done++
			}
		}
		if done == len(created) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d tasks finished", done, len(created))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if peak := backend.peak(); peak > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", peak)
	}
}

// TestTaskTimeoutFailsTask checks the per-task budget starts when the task
// runs and surfaces a readable timeout error.
func TestTaskTimeoutFailsTask(t *testing.T) {
	backend := newPoolBackend() // never released
	s, st := newStabilityServer(t, backend, 2, 80*time.Millisecond)
	h := s.Handler()

	task := createTask(t, h)
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, ok := s.getTask(task.ID)
		if ok && got.Status == "failed" {
			if !strings.Contains(got.Err, "timed out after 80ms") {
				t.Fatalf("err = %q", got.Err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task status = %+v, want failed", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := st.ListTasks(context.Background(), "failed")
	if err != nil || len(rows) != 1 {
		t.Fatalf("store failed tasks = %+v, %v", rows, err)
	}
}

// TestShutdownInterruptsRunningTask checks SIGTERM semantics: listeners stop,
// the in-flight dispatch is marked interrupted, Start returns within the
// bounded drain.
func TestShutdownInterruptsRunningTask(t *testing.T) {
	backend := newPoolBackend() // never released: only shutdown stops it
	s, _ := newStabilityServer(t, backend, 2, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- s.Start(ctx) }()

	sockPath := filepath.Join(s.sockDir, "chyan.sock")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	task := createTask(t, s.Handler())
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("task never reached the backend")
	}
	cancel()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("Start did not return after shutdown")
	}

	got, ok := s.getTask(task.ID)
	if !ok || got.Status != "interrupted" {
		t.Fatalf("task after shutdown = %+v (exists=%v)", got, ok)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket %s should be removed, err=%v", sockPath, err)
	}
}

// TestRecoverInterruptedTasks marks stale dispatch rows and leaves cron alone.
func TestRecoverInterruptedTasks(t *testing.T) {
	s, st := newStabilityServer(t, newPoolBackend(), 2, time.Minute)
	ctx := context.Background()
	now := time.Now().UTC()
	seed := []store.Task{
		{ID: "run1", Kind: "dispatch", Status: "running", CreatedAt: now},
		{ID: "run2", Kind: "dispatch", Status: "pending", CreatedAt: now},
		{ID: "cron1", Kind: "cron", Status: "running", Schedule: "* * * * *", CreatedAt: now},
		{ID: "done1", Kind: "dispatch", Status: "done", CreatedAt: now},
	}
	for _, task := range seed {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatalf("seed %s: %v", task.ID, err)
		}
	}

	s.recoverInterruptedTasks(ctx)

	want := map[string]string{"run1": "interrupted", "run2": "interrupted", "cron1": "running", "done1": "done"}
	all, err := st.ListTasks(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range all {
		if task.Status != want[task.ID] {
			t.Errorf("task %s status = %q, want %q", task.ID, task.Status, want[task.ID])
		}
	}
}

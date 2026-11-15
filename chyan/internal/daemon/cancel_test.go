// Cancel endpoint: POST /api/v1/tasks/{id}/cancel interrupts the run,
// records interrupted on the task and the session, and is idempotent.
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// blockingBackend holds every LLM call until the run context is cancelled.
type blockingBackend struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingBackend) Chat(ctx context.Context, _ core.Request) (core.Response, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return core.Response{}, ctx.Err()
}

func (b *blockingBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: "late"})
	}
	return r, err
}

// newCancelServer builds a daemon whose LLM blocks until interrupted.
func newCancelServer(t *testing.T) (*Server, store.Store, *blockingBackend) {
	t.Helper()
	home := t.TempDir()
	st := store.NewMemory()
	backend := &blockingBackend{started: make(chan struct{})}
	a, err := app.New(app.Options{
		SkipExtensions: true, SkipGate: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: backend, Store: st,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{Home: home, App: a, Store: st, Token: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, st, backend
}

// pollTask polls GET /api/v1/tasks/{id} until want matches.
func pollTask(t *testing.T, h http.Handler, id, want string) api.Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		w, req := doJSON(t, http.MethodGet, "/api/v1/tasks/"+id, nil, "test-token")
		h.ServeHTTP(w, req)
		var task api.Task
		if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
			t.Fatalf("decode task: %v (body %s)", err, w.Body.String())
		}
		if task.Status == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s status = %q, want %q", id, task.Status, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCancelTaskInterruptsAndMarksStatus(t *testing.T) {
	s, st, backend := newCancelServer(t)
	h := s.Handler()

	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{Prompt: "long task"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	var created api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("task never reached the LLM")
	}

	w, req = doJSON(t, http.MethodPost, "/api/v1/tasks/"+created.ID+"/cancel", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d body=%s", w.Code, w.Body.String())
	}

	task := pollTask(t, h, created.ID, "interrupted")
	if task.SessionID == "" {
		t.Fatal("interrupted task lost its session id")
	}

	// The store row and the session carry the interrupted state.
	rows, err := st.ListTasks(context.Background(), "interrupted")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(rows) == 0 || rows[0].ID != created.ID {
		t.Fatalf("store tasks = %+v", rows)
	}
	sess, err := st.GetSession(context.Background(), task.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Status != store.SessionStatusInterrupted {
		t.Fatalf("session status = %q", sess.Status)
	}

	// Repeated cancel is idempotent and keeps the terminal state.
	w, req = doJSON(t, http.MethodPost, "/api/v1/tasks/"+created.ID+"/cancel", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("second cancel status = %d body=%s", w.Code, w.Body.String())
	}
	var again api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if again.Status != "interrupted" {
		t.Fatalf("second cancel status = %q", again.Status)
	}

	// Unknown ids are 404.
	w, req = doJSON(t, http.MethodPost, "/api/v1/tasks/ghost/cancel", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cancel ghost status = %d", w.Code)
	}
}

// Interrupt semantics: a cancelled turn stops the LLM call, keeps the
// session, and records interrupted status (FR-1).
package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// gateBackend blocks LLM calls until the run context is cancelled, like an
// aborted HTTP request. Flipping block=false makes later turns succeed.
type gateBackend struct {
	mu      sync.Mutex
	block   bool
	started chan struct{}
	once    sync.Once
}

func (b *gateBackend) setBlock(v bool) {
	b.mu.Lock()
	b.block = v
	b.mu.Unlock()
}

func (b *gateBackend) Chat(ctx context.Context, _ core.Request) (core.Response, error) {
	b.mu.Lock()
	block := b.block
	b.mu.Unlock()
	if !block {
		return core.Response{Message: core.Message{Role: core.RoleAssistant,
			Content: []core.Part{{Type: "text", Text: "ok"}}}}, nil
	}
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return core.Response{}, ctx.Err()
}

func (b *gateBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: "ok"})
	}
	return r, err
}

func newInterruptApp(t *testing.T, backend core.LLMBackend) (*App, store.Store) {
	t.Helper()
	st := store.NewMemory()
	a, err := New(Options{
		SkipExtensions: true, SkipGate: true,
		Home: t.TempDir(), MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: backend, Store: st,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, st
}

func TestRunOptsInterruptMarksSessionInterrupted(t *testing.T) {
	backend := &gateBackend{block: true, started: make(chan struct{})}
	a, st := newInterruptApp(t, backend)

	var (
		mu        sync.Mutex
		interrupt func()
	)
	ready := make(chan struct{})
	done := make(chan error, 1)
	var sid string
	go func() {
		_, id, err := a.RunOpts(context.Background(), "", "long task", RunOptions{
			OnInterruptReady: func(fn func()) {
				mu.Lock()
				interrupt = fn
				mu.Unlock()
				close(ready)
			},
		})
		sid = id
		done <- err
	}()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("run never exposed its interrupt function")
	}
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("LLM call never started")
	}

	mu.Lock()
	interrupt()
	mu.Unlock()

	select {
	case err := <-done:
		if !errors.Is(err, core.ErrInterrupted) {
			t.Fatalf("run error = %v, want ErrInterrupted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt did not stop the run")
	}

	if sid == "" {
		t.Fatal("session id missing after interrupt")
	}
	sess, err := st.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Status != store.SessionStatusInterrupted {
		t.Fatalf("session status = %q, want %q", sess.Status, store.SessionStatusInterrupted)
	}
	// The user message of the cancelled turn is still persisted.
	msgs, err := st.ListMessages(context.Background(), sid, 10, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("cancelled turn lost its user message")
	}

	// A later successful turn resets the status to active.
	backend.setBlock(false)
	if _, _, err := a.RunOpts(context.Background(), sid, "retry", RunOptions{}); err != nil {
		t.Fatalf("retry run: %v", err)
	}
	sess, err = st.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("GetSession after retry: %v", err)
	}
	if sess.Status != store.SessionStatusActive {
		t.Fatalf("session status after retry = %q, want %q", sess.Status, store.SessionStatusActive)
	}
}

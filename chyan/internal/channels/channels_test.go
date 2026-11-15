package channels

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeChannel is a controllable Channel for registry tests.
type fakeChannel struct {
	name       string
	connectErr error
	sendErr    error

	mu        sync.Mutex
	sendCount int
	closed    bool
	sends     []Outgoing

	updates chan Message
}

func newFakeChannel(name string) *fakeChannel {
	return &fakeChannel{name: name, updates: make(chan Message, 8)}
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Connect(ctx context.Context) error { return f.connectErr }

func (f *fakeChannel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeChannel) Send(ctx context.Context, out Outgoing) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCount++
	f.sends = append(f.sends, out)
	return f.sendErr
}

func (f *fakeChannel) Updates() <-chan Message { return f.updates }

func (f *fakeChannel) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendCount
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newFakeChannel("a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(newFakeChannel("a")); err == nil {
		t.Fatal("expected duplicate name error")
	}
}

func TestRegistryRegisterAfterClose(t *testing.T) {
	r := NewRegistry()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(newFakeChannel("a")); err == nil {
		t.Fatal("expected error registering on closed registry")
	}
}

func TestRegistryStartEmpty(t *testing.T) {
	r := NewRegistry()
	if err := r.Start(context.Background()); err == nil {
		t.Fatal("expected error starting with no channels")
	}
}

// One channel failing Send must not block or prevent delivery on the other.
func TestRegistryBroadcastDegradedSend(t *testing.T) {
	bad := newFakeChannel("bad")
	bad.sendErr = errors.New("boom")
	good := newFakeChannel("good")
	r := NewRegistry()
	if err := r.Register(bad); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(good); err != nil {
		t.Fatal(err)
	}
	err := r.Broadcast(context.Background(), Outgoing{ChatID: "c1", Text: "hi"})
	if err == nil {
		t.Fatal("expected broadcast error from bad channel")
	}
	if good.sentCount() != 1 {
		t.Fatalf("good channel sends = %d, want 1", good.sentCount())
	}
	if bad.sentCount() != 1 {
		t.Fatalf("bad channel sends = %d, want 1 (attempted)", bad.sentCount())
	}
}

func TestRegistrySendUnknown(t *testing.T) {
	r := NewRegistry()
	if err := r.Send(context.Background(), "nope", Outgoing{Text: "x"}); err == nil {
		t.Fatal("expected unknown channel error")
	}
}

func TestRegistryUpdatesStream(t *testing.T) {
	a := newFakeChannel("a")
	b := newFakeChannel("b")
	r := NewRegistry()
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(b); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.updates <- Message{ChatID: "c1", Text: "from a"}
	b.updates <- Message{ChatID: "c2", Text: "from b"}

	got := map[string]string{}
	up := r.Updates()
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case tm := <-up:
			got[tm.Channel] = tm.Message.Text
		case <-deadline:
			t.Fatalf("timed out, got %v", got)
		}
	}
	if got["a"] != "from a" || got["b"] != "from b" {
		t.Fatalf("got %v", got)
	}
}

// A channel whose Connect fails is degraded; the other keeps working.
func TestRegistryConnectFailureDegradesOne(t *testing.T) {
	bad := newFakeChannel("bad")
	bad.connectErr = errors.New("no network")
	good := newFakeChannel("good")
	r := NewRegistry()
	if err := r.Register(bad); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(good); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	good.updates <- Message{Text: "still alive"}
	select {
	case tm := <-r.Updates():
		if tm.Channel != "good" || tm.Message.Text != "still alive" {
			t.Fatalf("got %+v", tm)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no update from good channel")
	}
}

func TestRegistryCloseIsIdempotent(t *testing.T) {
	a := newFakeChannel("a")
	r := NewRegistry()
	if err := r.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if !closed {
		t.Fatal("channel not closed by registry")
	}
	// merged stream must close
	up := r.Updates()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-up:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("updates stream did not close")
		}
	}
}

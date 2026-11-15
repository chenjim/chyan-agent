package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/channels"
	"chyan.tech/chyan/internal/core"
)

// routerBackend answers router (tool-less) requests and worker (tooled)
// requests differently, and records every request for assertions.
type routerBackend struct {
	mu     sync.Mutex
	reqs   []core.Request
	router string
	worker string
	fail   bool
}

func (b *routerBackend) Chat(_ context.Context, req core.Request) (core.Response, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	b.mu.Unlock()
	if isRouterRequest(req) {
		if b.fail {
			return core.Response{}, errors.New("router unavailable")
		}
		return textReply(b.router), nil
	}
	if len(req.Tools) > 0 {
		return textReply(b.worker), nil
	}
	return textReply("ok"), nil // title generation and other cheap calls
}

func (b *routerBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	res, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: core.FlattenText(res.Message)})
	}
	return res, err
}

func textReply(text string) core.Response {
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: text}}}}
}

// isRouterRequest identifies the router turn by its system prompt.
func isRouterRequest(req core.Request) bool {
	for _, m := range req.Messages {
		if m.Role != core.RoleSystem {
			continue
		}
		for _, p := range m.Content {
			if strings.Contains(p.Text, "调度 Router") {
				return true
			}
		}
	}
	return false
}

// lastUserText flattens the final user message of one request.
func lastUserText(req core.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == core.RoleUser {
			return core.FlattenText(req.Messages[i])
		}
	}
	return ""
}

// routerCalls returns the recorded router request session ids.
func (b *routerBackend) routerSessions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, req := range b.reqs {
		if isRouterRequest(req) {
			out = append(out, req.SessionID)
		}
	}
	return out
}

// workerPrompts returns the prompts of tooled worker turns.
func (b *routerBackend) workerPrompts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, req := range b.reqs {
		if isRouterRequest(req) || len(req.Tools) == 0 {
			continue
		}
		out = append(out, lastUserText(req))
	}
	return out
}

func TestInboundRouterRewritesColdStart(t *testing.T) {
	t.Setenv(RouterEnv, "1")
	backend := &routerBackend{router: "REWRITE-A", worker: "worker done"}
	s, ch := newInboundHarness(t, backend, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_ok"}, MinInterval: time.Nanosecond,
	})
	ctx := context.Background()

	// First thread: cold start goes through the router.
	ch.updatesC <- channels.Message{ID: "m1", ChatID: "oc_ok", From: "u1", Text: "first request"}
	ch.waitReplyMatch(t, func(s string) bool { return strings.Contains(s, "worker done") })

	// Second thread on the same channel: also a cold start, but the router
	// session must continue (cross-thread continuity).
	ch.updatesC <- channels.Message{ID: "m2", ChatID: "oc_ok", ThreadID: "t2", From: "u1", Text: "second request"}
	ch.waitReplyMatch(t, func(s string) bool { return strings.Contains(s, "worker done") })

	sessions := backend.routerSessions()
	if len(sessions) != 2 {
		t.Fatalf("router turns = %d, want 2 (one per cold-start thread)", len(sessions))
	}
	if sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("router session not continuous: %q vs %q", sessions[0], sessions[1])
	}
	raw, err := s.store.GetKV(ctx, imRouterKey("feishu"))
	if err != nil || string(raw) != sessions[0] {
		t.Fatalf("router session mapping = %q (%v), want %q", raw, err, sessions[0])
	}
	prompts := backend.workerPrompts()
	if len(prompts) != 2 {
		t.Fatalf("worker turns = %d, want 2", len(prompts))
	}
	for _, p := range prompts {
		if !strings.Contains(p, "[router]") || !strings.Contains(p, "REWRITE-A") {
			t.Fatalf("worker prompt missing router instruction:\n%s", p)
		}
	}
}

func TestInboundRouterDisabledByDefault(t *testing.T) {
	t.Setenv(RouterEnv, "")
	backend := &routerBackend{router: "REWRITE-A", worker: "worker done"}
	s, ch := newInboundHarness(t, backend, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_ok"}, MinInterval: time.Nanosecond,
	})
	ch.updatesC <- channels.Message{ID: "m1", ChatID: "oc_ok", From: "u1", Text: "hello"}
	ch.waitReplyMatch(t, func(s string) bool { return strings.Contains(s, "worker done") })

	if n := len(backend.routerSessions()); n != 0 {
		t.Fatalf("router ran %d times while disabled", n)
	}
	if _, err := s.store.GetKV(context.Background(), imRouterKey("feishu")); err == nil {
		t.Fatal("router session persisted while disabled")
	}
}

func TestInboundRouterFailureKeepsOriginalPrompt(t *testing.T) {
	t.Setenv(RouterEnv, "1")
	backend := &routerBackend{router: "unused", worker: "worker done", fail: true}
	_, ch := newInboundHarness(t, backend, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_ok"}, MinInterval: time.Nanosecond,
	})
	ch.updatesC <- channels.Message{ID: "m1", ChatID: "oc_ok", From: "u1", Text: "original request"}
	ch.waitReplyMatch(t, func(s string) bool { return strings.Contains(s, "worker done") })

	prompts := backend.workerPrompts()
	if len(prompts) != 1 || strings.Contains(prompts[0], "[router]") {
		t.Fatalf("worker prompt after router failure = %q", prompts)
	}
}

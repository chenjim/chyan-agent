// Inbound IM dispatch: whitelist, session mapping, /new, loop guard.
package daemon

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/channels"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// inboundFake is a channel that records replies and carries an inbound policy.
type inboundFake struct {
	name     string
	policy   channels.InboundPolicy
	updatesC chan channels.Message
	sentC    chan channels.Outgoing
	mu       sync.Mutex
	sent     []channels.Outgoing
	img      []byte
	imgMime  string
	imgErr   error
}

func (c *inboundFake) Name() string                  { return c.name }
func (c *inboundFake) Connect(context.Context) error { return nil }
func (c *inboundFake) Close() error                  { return nil }
func (c *inboundFake) Updates() <-chan channels.Message {
	return c.updatesC
}
func (c *inboundFake) InboundPolicy() channels.InboundPolicy { return c.policy }
func (c *inboundFake) FetchImage(context.Context, string, string) ([]byte, string, error) {
	return c.img, c.imgMime, c.imgErr
}
func (c *inboundFake) Send(_ context.Context, out channels.Outgoing) error {
	c.mu.Lock()
	c.sent = append(c.sent, out)
	c.mu.Unlock()
	select {
	case c.sentC <- out:
	default:
	}
	return nil
}

func (c *inboundFake) replies() []channels.Outgoing {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]channels.Outgoing(nil), c.sent...)
}

// waitReply waits for the next reply and returns its text.
func (c *inboundFake) waitReply(t *testing.T) string {
	t.Helper()
	select {
	case out := <-c.sentC:
		return out.Text
	case <-time.After(3 * time.Second):
		t.Fatal("no reply from the agent")
		return ""
	}
}

func TestInboundDispatchWhitelistAndSessions(t *testing.T) {
	ch := &inboundFake{
		name:     "feishu",
		updatesC: make(chan channels.Message, 8),
		sentC:    make(chan channels.Outgoing, 8),
		policy: channels.InboundPolicy{
			Enabled: true, AllowedChats: []string{"oc_ok"}, MinInterval: time.Nanosecond,
		},
	}
	reg := channels.NewRegistry()
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	a, err := app.New(app.Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: &stubBackend{reply: "agent reply"}, Store: store.NewMemory(),
		Channels: reg,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{Home: home, App: a, Store: app.StoreOf(a)})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("registry start: %v", err)
	}
	go s.bridgeChannelUpdates(ctx)

	ch.updatesC <- channels.Message{ID: "m1", ChatID: "oc_ok", From: "u1", Text: " hello "}
	if got := ch.waitReply(t); !strings.Contains(got, "agent reply") {
		t.Fatalf("first reply = %q", got)
	}
	raw, err := s.store.GetKV(ctx, imSessionKey("feishu:oc_ok"))
	if err != nil || len(raw) == 0 {
		t.Fatalf("session mapping missing: %q, %v", raw, err)
	}
	firstSession := string(raw)

	// Second message continues the same session.
	ch.updatesC <- channels.Message{ID: "m2", ChatID: "oc_ok", From: "u1", Text: "again"}
	ch.waitReplyMatch(t, func(s string) bool { return strings.Contains(s, "agent reply") })
	raw, _ = s.store.GetKV(ctx, imSessionKey("feishu:oc_ok"))
	if string(raw) != firstSession {
		t.Fatalf("session changed: %q -> %q", firstSession, raw)
	}

	// Unauthorized chat is silently dropped (no reply).
	ch.updatesC <- channels.Message{ID: "m3", ChatID: "oc_bad", From: "u9", Text: "hi"}
	// A bot message in an allowed chat is ignored (loop guard).
	ch.updatesC <- channels.Message{ID: "m4", ChatID: "oc_ok", From: "u1", FromBot: true, Text: "own reply"}
	time.Sleep(150 * time.Millisecond)
	if replies := ch.replies(); len(replies) != 2 {
		t.Fatalf("replies after unauthorized/bot = %d, want 2 (%+v)", len(replies), replies)
	}

	// /new drops the mapping and acknowledges.
	ch.updatesC <- channels.Message{ID: "m5", ChatID: "oc_ok", From: "u1", Text: "/new"}
	if got := ch.waitReplyMatch(t, func(s string) bool { return strings.Contains(s, "已开启新会话") }); !strings.Contains(got, "已开启新会话") {
		t.Fatalf("/new reply = %q", got)
	}
	if raw, _ := s.store.GetKV(ctx, imSessionKey("feishu:oc_ok")); len(raw) != 0 {
		t.Fatalf("/new kept mapping %q", raw)
	}
}

func TestInboundPolicyDeniesEverythingWithoutWhitelist(t *testing.T) {
	p := channels.InboundPolicy{Enabled: true}
	if p.Authorized("oc", "u") {
		t.Fatal("empty whitelist must deny")
	}
	p.AllowedUsers = []string{"u1"}
	if !p.Authorized("oc", "u1") || p.Authorized("oc", "u2") {
		t.Fatal("user whitelist mismatch")
	}
	p.AllowedChats = []string{"*"}
	if !p.Authorized("any", "u2") {
		t.Fatal("wildcard chat should allow")
	}
}

func TestInboundPolicyLimits(t *testing.T) {
	p := channels.InboundPolicy{Enabled: true}
	maxChars, interval := p.Limits()
	if maxChars != channels.DefaultMaxChars || interval != channels.DefaultMinInterval {
		t.Fatalf("limits = %d, %v", maxChars, interval)
	}
}

// TestBridgeDecisionRequiresAuthorizedChat: an approval decision is only
// trusted from a whitelisted chat on the approval channel; anyone else must
// not be able to approve pending agent work.
func TestBridgeDecisionRequiresAuthorizedChat(t *testing.T) {
	ch := &inboundFake{
		name:     "feishu",
		updatesC: make(chan channels.Message, 8),
		sentC:    make(chan channels.Outgoing, 8),
		policy: channels.InboundPolicy{
			Enabled: true, AllowedChats: []string{"oc_ok"}, MinInterval: time.Nanosecond,
		},
	}
	reg := channels.NewRegistry()
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	a, err := app.New(app.Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: &stubBackend{reply: "ok"}, Store: store.NewMemory(),
		Channels: reg,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	hub := NewApprovalHub(nil)
	s, err := New(Options{Home: home, App: a, Store: app.StoreOf(a), Gate: hub})
	if err != nil {
		t.Fatal(err)
	}
	if s.imApprover == nil {
		t.Fatal("IM approver must be wired for a hub gate with channels")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	go s.bridgeChannelUpdates(ctx)

	id, wait, err := hub.Begin(core.ApprovalRequest{Action: "bash", Resource: "ls"})
	if err != nil {
		t.Fatal(err)
	}
	// Unauthorized chat: the ticket must stay pending.
	ch.updatesC <- channels.Message{ID: "d1", ChatID: "oc_bad", From: "u9", Text: "approve " + id}
	time.Sleep(150 * time.Millisecond)
	if p := hub.Pending(); len(p) != 1 {
		t.Fatalf("unauthorized decision resolved the ticket: %+v", p)
	}
	// Authorized chat on the approval channel resolves it.
	ch.updatesC <- channels.Message{ID: "d2", ChatID: "oc_ok", From: "u1", Text: "approve " + id}
	wctx, wcancel := context.WithTimeout(ctx, 2*time.Second)
	defer wcancel()
	approved, err := wait(wctx)
	if err != nil || !approved {
		t.Fatalf("authorized decision = %v, %v", approved, err)
	}
}

// TestInboundImageAttachment: an image-only message downloads the attachment,
// uses the default prompt and reaches the LLM with image bytes (FR-10).
func TestInboundImageAttachment(t *testing.T) {
	pngData, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	ch := &inboundFake{
		name:     "feishu",
		updatesC: make(chan channels.Message, 4),
		sentC:    make(chan channels.Outgoing, 4),
		policy: channels.InboundPolicy{
			Enabled: true, AllowedChats: []string{"oc_ok"}, MinInterval: time.Nanosecond,
		},
		img: pngData, imgMime: "image/png",
	}
	reg := channels.NewRegistry()
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	rec := &imageRecorder{}
	a, err := app.New(app.Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: rec, Store: store.NewMemory(), Channels: reg,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{Home: home, App: a, Store: app.StoreOf(a)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	go s.bridgeChannelUpdates(ctx)

	ch.updatesC <- channels.Message{ID: "img1", ChatID: "oc_ok", From: "u1", ImageKey: "img_key"}
	if got := ch.waitReply(t); !strings.Contains(got, "ok") && !strings.Contains(got, "seen") {
		t.Fatalf("reply = %q", got)
	}
	if !rec.sawImage() {
		t.Fatal("inbound image did not reach the LLM")
	}
}

// ------------------------------------------------- P0: queue + reply policy

// promptBackend records user prompts and optionally blocks before answering,
// so tests can observe per-chat queueing and turn-boundary merging.
type promptBackend struct {
	mu      sync.Mutex
	prompts []string
	started chan struct{}
	release chan struct{}
	reply   string
}

func (b *promptBackend) Chat(ctx context.Context, req core.Request) (core.Response, error) {
	text := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != core.RoleUser {
			continue
		}
		var parts []string
		for _, p := range m.Content {
			if p.Type == "text" {
				parts = append(parts, p.Text)
			}
		}
		text = strings.Join(parts, "\n")
		break
	}
	b.mu.Lock()
	b.prompts = append(b.prompts, text)
	b.mu.Unlock()
	if b.started != nil {
		select {
		case b.started <- struct{}{}:
		default:
		}
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return core.Response{}, ctx.Err()
		}
	}
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: b.reply}}}}, nil
}

func (b *promptBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: b.reply})
	}
	return r, err
}

func (b *promptBackend) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.prompts...)
}

// newInboundHarness wires one fake channel, a channel registry and the
// inbound bridge. The watchdog is disabled so tests are deterministic.
func newInboundHarness(t *testing.T, backend core.LLMBackend, policy channels.InboundPolicy) (*Server, *inboundFake) {
	t.Helper()
	ch := &inboundFake{
		name: "feishu", updatesC: make(chan channels.Message, 32),
		sentC: make(chan channels.Outgoing, 64), policy: policy,
	}
	reg := channels.NewRegistry()
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	a, err := app.New(app.Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: backend, Store: store.NewMemory(), Channels: reg,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{
		Home: home, App: a, Store: app.StoreOf(a),
		MaxConcurrentTasks: 4, TaskTimeout: 2 * time.Minute,
		WatchdogInterval: -time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := reg.Start(ctx); err != nil {
		t.Fatal(err)
	}
	go s.bridgeChannelUpdates(ctx)
	return s, ch
}

// waitReplyMatch reads replies until one satisfies match (acks are skipped).
func (c *inboundFake) waitReplyMatch(t *testing.T, match func(string) bool) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case out := <-c.sentC:
			if match(out.Text) {
				return out.Text
			}
		case <-deadline:
			t.Fatal("no matching reply from the agent")
			return ""
		}
	}
}

// waitOutgoing returns the next outbound message (any content).
func (c *inboundFake) waitOutgoing(t *testing.T) channels.Outgoing {
	t.Helper()
	select {
	case out := <-c.sentC:
		return out
	case <-time.After(3 * time.Second):
		t.Fatal("no outbound message")
		return channels.Outgoing{}
	}
}

// TestInboundQueueMergesMessages: while one turn is running, new messages are
// acknowledged immediately and merged into a single follow-up turn instead of
// one full run per message (full-duplex minimum, P0).
func TestInboundQueueMergesMessages(t *testing.T) {
	gate := make(chan struct{})
	pb := &promptBackend{started: make(chan struct{}, 8), release: gate, reply: "done"}
	_, ch := newInboundHarness(t, pb, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_q"}, MinInterval: time.Nanosecond,
	})

	ch.updatesC <- channels.Message{ID: "q1", ChatID: "oc_q", From: "u1", Text: "first"}
	select {
	case <-pb.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn never started")
	}
	ch.updatesC <- channels.Message{ID: "q2", ChatID: "oc_q", From: "u1", Text: "second"}
	ch.updatesC <- channels.Message{ID: "q3", ChatID: "oc_q", From: "u1", Text: "third"}
	ack := ch.waitReplyMatch(t, func(s string) bool {
		return strings.Contains(s, "排队中") || strings.Contains(s, "处理中")
	})
	if !strings.Contains(ack, "已收到") {
		t.Fatalf("busy ack = %q", ack)
	}

	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		merged := false
		for _, p := range pb.snapshot() {
			if strings.Contains(p, "second") && strings.Contains(p, "third") {
				merged = true
			}
		}
		if merged {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued messages never merged: %q", pb.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestInboundQueueFullDrops: a chat flood is bounded; overflow messages get a
// visible drop reply instead of unbounded goroutines.
func TestInboundQueueFullDrops(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	pb := &promptBackend{started: make(chan struct{}, 8), release: gate, reply: "done"}
	_, ch := newInboundHarness(t, pb, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_q"}, MinInterval: time.Nanosecond,
	})
	ch.updatesC <- channels.Message{ID: "flood0", ChatID: "oc_q", From: "u1", Text: "first"}
	select {
	case <-pb.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn never started")
	}
	for i := 0; i < maxInboundQueue+2; i++ {
		ch.updatesC <- channels.Message{
			ID: "flood" + strings.Repeat("x", i+1), ChatID: "oc_q", From: "u1", Text: "more",
		}
	}
	got := ch.waitReplyMatch(t, func(s string) bool { return strings.Contains(s, "排队已满") })
	if got != ackQueueFull {
		t.Fatalf("queue-full reply = %q", got)
	}
}

// TestInboundReplyClampedWithPWALink: long agent output is cut at the channel
// limit and points at the full PWA session.
func TestInboundReplyClampedWithPWALink(t *testing.T) {
	t.Setenv(PublicURLEnv, "https://pwa.example")
	long := strings.Repeat("这是一段很长的回复内容。\n", 200)
	s, ch := newInboundHarness(t, &stubBackend{reply: long}, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_q"}, MinInterval: time.Nanosecond,
		MaxReplyChars: 60,
	})
	ch.updatesC <- channels.Message{ID: "long1", ChatID: "oc_q", From: "u1", Text: "please report"}
	got := ch.waitReplyMatch(t, func(text string) bool { return strings.Contains(text, "已截断") })
	if !strings.Contains(got, "完整内容：https://pwa.example/#/sessions/") {
		t.Fatalf("clamped reply lacks the PWA link: %q", got)
	}
	if runes := []rune(got); len(runes) > 60+80 {
		t.Fatalf("clamped reply too long (%d runes): %q", len(runes), got)
	}
	raw, _ := s.store.GetKV(context.Background(), imSessionKey("feishu:oc_q"))
	if len(raw) == 0 {
		t.Fatal("session mapping missing after a clamped reply")
	}
}

// TestInboundIntentFilter: chatter is bridged but not executed; directed
// requests run.
func TestInboundIntentFilter(t *testing.T) {
	_, ch := newInboundHarness(t, &stubBackend{reply: "agent reply"}, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_q"}, MinInterval: time.Nanosecond,
		IntentFilter: true,
	})
	ch.updatesC <- channels.Message{ID: "chat1", ChatID: "oc_q", From: "u1", Text: "今天天气不错"}
	time.Sleep(200 * time.Millisecond)
	if replies := ch.replies(); len(replies) != 0 {
		t.Fatalf("plain chatter started a run: %+v", replies)
	}
	ch.updatesC <- channels.Message{ID: "ask1", ChatID: "oc_q", From: "u1", Text: "请总结一下 README"}
	got := ch.waitReplyMatch(t, func(s string) bool { return strings.Contains(s, "agent reply") })
	if got == "" {
		t.Fatal("directed request did not run")
	}
}

// TestInboundThreadSessions: the same chat with different provider threads
// runs two independent sessions, and a thread keeps its session across turns
// (P1 thread→worker upgrade).
func TestInboundThreadSessions(t *testing.T) {
	ctx := context.Background()
	s, ch := newInboundHarness(t, &stubBackend{reply: "agent reply"}, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_t"}, MinInterval: time.Nanosecond,
	})
	isReply := func(v string) bool { return strings.Contains(v, "agent reply") }

	ch.updatesC <- channels.Message{ID: "t1a", ChatID: "oc_t", From: "u1", ThreadID: "t1", Text: "first"}
	ch.waitReplyMatch(t, isReply)
	raw, _ := s.store.GetKV(ctx, imSessionKey("feishu:oc_t:t:t1"))
	if len(raw) == 0 {
		t.Fatal("thread t1 has no session mapping")
	}
	sid1 := string(raw)

	ch.updatesC <- channels.Message{ID: "t2a", ChatID: "oc_t", From: "u1", ThreadID: "t2", Text: "second"}
	ch.waitReplyMatch(t, isReply)
	raw2, _ := s.store.GetKV(ctx, imSessionKey("feishu:oc_t:t:t2"))
	if len(raw2) == 0 || string(raw2) == sid1 {
		t.Fatalf("thread t2 must get its own session, got %q vs %q", raw2, sid1)
	}

	ch.updatesC <- channels.Message{ID: "t1b", ChatID: "oc_t", From: "u1", ThreadID: "t1", Text: "again"}
	ch.waitReplyMatch(t, isReply)
	raw, _ = s.store.GetKV(ctx, imSessionKey("feishu:oc_t:t:t1"))
	if string(raw) != sid1 {
		t.Fatalf("thread t1 session changed: %q -> %q", sid1, raw)
	}
	// A thread without an id keeps the old chat-level key.
	if _, err := s.store.GetKV(ctx, imSessionKey("feishu:oc_t")); err == nil {
		t.Fatal("threaded messages must not write the chat-level mapping")
	}

	// Replies are threaded.
	found := false
	for _, out := range ch.replies() {
		if out.ThreadID == "t1" && strings.Contains(out.Text, "agent reply") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no threaded reply: %+v", ch.replies())
	}
}

// TestDeliverThreadAndSession: background notices go straight to the thread
// that subscribed (P1 direct delivery), not through the inbound queue.
func TestDeliverThreadAndSession(t *testing.T) {
	ctx := context.Background()
	s, ch := newInboundHarness(t, &stubBackend{reply: "agent reply"}, channels.InboundPolicy{
		Enabled: true, AllowedChats: []string{"oc_d"}, MinInterval: time.Nanosecond,
	})
	ch.updatesC <- channels.Message{ID: "d1", ChatID: "oc_d", From: "u1", ThreadID: "topic1", Text: "hello"}
	ch.waitReplyMatch(t, func(v string) bool { return strings.Contains(v, "agent reply") })

	if !s.deliverThread(ctx, "feishu:oc_d:t:topic1", "task done") {
		t.Fatal("deliverThread did not find the subscription")
	}
	out := ch.waitOutgoing(t)
	if out.ThreadID != "topic1" || out.ChatID != "oc_d" || out.Text != "task done" {
		t.Fatalf("deliverThread outgoing = %+v", out)
	}

	raw, _ := s.store.GetKV(ctx, imSessionKey("feishu:oc_d:t:topic1"))
	if len(raw) == 0 {
		t.Fatal("session mapping missing")
	}
	if !s.deliverSession(ctx, string(raw), "session notice") {
		t.Fatal("deliverSession did not find the owner")
	}
	out = ch.waitOutgoing(t)
	if out.ThreadID != "topic1" || out.Text != "session notice" {
		t.Fatalf("deliverSession outgoing = %+v", out)
	}

	if s.deliverThread(ctx, "feishu:oc_unknown:t:none", "x") {
		t.Fatal("unknown thread must not report delivery")
	}
	if s.deliverSession(ctx, "", "x") {
		t.Fatal("empty session must not report delivery")
	}
}

func TestIntentMatchedRules(t *testing.T) {
	cases := []struct {
		msg  channels.Message
		want bool
	}{
		{channels.Message{Text: "/new"}, true},
		{channels.Message{Text: "这个怎么用？"}, true},
		{channels.Message{Text: "@_user_1 看一下"}, true},
		{channels.Message{Text: "please check the build"}, true},
		{channels.Message{Text: "帮我修一下"}, true},
		{channels.Message{Text: "好的收到"}, false},
		{channels.Message{ImageKey: "img"}, true},
	}
	for _, c := range cases {
		if got := intentMatched(c.msg); got != c.want {
			t.Errorf("intentMatched(%q) = %v, want %v", c.msg.Text, got, c.want)
		}
	}
}

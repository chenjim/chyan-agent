package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/channels"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// recordingChannel captures Send calls (stubChannel does not record).
type recordingChannel struct {
	name     string
	updatesC chan channels.Message
	mu       sync.Mutex
	sent     []channels.Outgoing
}

func (c *recordingChannel) Name() string                  { return c.name }
func (c *recordingChannel) Connect(context.Context) error { return nil }
func (c *recordingChannel) Close() error                  { return nil }
func (c *recordingChannel) Send(_ context.Context, out channels.Outgoing) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, out)
	return nil
}
func (c *recordingChannel) Updates() <-chan channels.Message { return c.updatesC }
func (c *recordingChannel) sentMessages() []channels.Outgoing {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]channels.Outgoing(nil), c.sent...)
}

// newIMApproverForTest wires a hub to a registry carrying one recording
// feishu channel.
func newIMApproverForTest(t *testing.T, chatID string) (*IMApprover, *ApprovalHub, *recordingChannel) {
	t.Helper()
	rec := &recordingChannel{name: "feishu", updatesC: make(chan channels.Message, 8)}
	reg := channels.NewRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatal(err)
	}
	hub := NewApprovalHub(nil)
	return NewIMApprover(hub, reg, chatID), hub, rec
}

// waitForPending blocks until the hub shows n tickets, then returns them.
func waitForPending(t *testing.T, h *ApprovalHub, n int) []api.ApprovalTicket {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := h.Pending(); len(p) == n {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pending never reached %d: %+v", n, h.Pending())
	return nil
}

// TestIMApproverApprovePushesTicket: Approve registers the ticket and pushes
// one IM message carrying the id and the diff, then blocks for the decision.
func TestIMApproverApprovePushesTicket(t *testing.T) {
	ima, hub, rec := newIMApproverForTest(t, "oc_1")
	req := core.ApprovalRequest{
		Action: "bash", Resource: "rm -rf x", Detail: "delete tree", Diff: "rm -rf x",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := startApprove(ctx, ima, req)
	ticket := waitForPending(t, hub, 1)[0]

	msgs := rec.sentMessages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.ChatID != "oc_1" {
		t.Errorf("chat id = %q", m.ChatID)
	}
	for _, want := range []string{"[审批]", "bash", "rm -rf x", "delete tree", ticket.ID, "裁决"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("message missing %q:\n%s", want, m.Text)
		}
	}

	if !ima.HandleDecision("approve " + ticket.ID) {
		t.Fatal("HandleDecision must consume the decision")
	}
	select {
	case r := <-res:
		if r.err != nil || !r.approved {
			t.Fatalf("Approve = (%v, %v), want approved", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after the IM decision")
	}
	if len(hub.Pending()) != 0 {
		t.Error("resolved ticket must leave pending")
	}
}

// TestIMApproverFullFlowDenyReason: deny with a reason resolves the blocked
// Approve to false and the reason reaches the audit event.
func TestIMApproverFullFlowDenyReason(t *testing.T) {
	var mu sync.Mutex
	var done api.ApprovalDecision
	hub := NewApprovalHub(func(e Event) {
		if e.Name == api.EventApprovalDone {
			mu.Lock()
			done = e.Data.(api.ApprovalDecision)
			mu.Unlock()
		}
	})
	rec := &recordingChannel{name: "feishu", updatesC: make(chan channels.Message, 8)}
	reg := channels.NewRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatal(err)
	}
	ima := NewIMApprover(hub, reg, "oc_1")

	res := startApprove(context.Background(), ima,
		core.ApprovalRequest{Action: "write", Resource: "a.txt", Diff: "+secret"})
	id := waitForPending(t, hub, 1)[0].ID

	if !ima.HandleDecision("deny " + id + " too risky") {
		t.Fatal("deny command must be consumed")
	}
	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("Approve: %v", r.err)
		}
		if r.approved {
			t.Error("denied request must return false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if done.Decision != "deny" || done.Reason != "too risky" {
		t.Errorf("audit = %+v, want deny/why", done)
	}
}

// TestIMApproverHandleDecisionParsing: command grammar — approve/deny with or
// without a reason, case-insensitive, leading whitespace; other text passes.
func TestIMApproverHandleDecisionParsing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verb      string // decision verb as typed
		reason    string // reason appended
		wantProve bool   // Approve result after the command
	}{
		{name: "approve plain", verb: "approve", wantProve: true},
		{name: "deny with reason", verb: "deny", reason: "not today"},
		{name: "uppercase verb", verb: "APPROVE", wantProve: true},
		{name: "mixed case verb", verb: "Deny", reason: "nope"},
		{name: "leading whitespace", verb: "   approve", wantProve: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ima, hub, _ := newIMApproverForTest(t, "oc_1")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			res := startApprove(ctx, ima, core.ApprovalRequest{Action: "bash", Resource: "x"})
			id := waitForPending(t, hub, 1)[0].ID

			text := tc.verb + " " + id
			if tc.reason != "" {
				text += " " + tc.reason
			}
			if got := ima.HandleDecision(text); !got {
				t.Fatalf("HandleDecision(%q) = false, want true", text)
			}
			select {
			case r := <-res:
				if r.err != nil {
					t.Fatalf("Approve: %v", r.err)
				}
				if r.approved != tc.wantProve {
					t.Errorf("approved = %v, want %v", r.approved, tc.wantProve)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Approve did not return")
			}
		})
	}

	// Non-decision text passes through untouched and keeps the ticket.
	for _, text := range []string{
		"hello there",  // plain conversation
		"approve",      // verb without id
		"maybe apr1",   // unknown verb
		"",             // empty
		"   ",          // blank
		"deny apr1 no", // decision-shaped: consumed even for unknown ids
	} {
		ima, hub, _ := newIMApproverForTest(t, "oc_1")
		ctx, cancel := context.WithCancel(context.Background())
		startApprove(ctx, ima, core.ApprovalRequest{Action: "bash", Resource: "x"})
		waitForPending(t, hub, 1)

		consumed := ima.HandleDecision(text)
		want := strings.HasPrefix(strings.TrimSpace(strings.ToLower(text)), "approve ") ||
			strings.HasPrefix(strings.TrimSpace(strings.ToLower(text)), "deny ")
		if consumed != want {
			t.Errorf("HandleDecision(%q) = %v, want %v", text, consumed, want)
		}
		if !consumed && len(hub.Pending()) != 1 {
			t.Errorf("HandleDecision(%q) must keep the ticket pending", text)
		}
		cancel()
	}
}

// TestIMApproverCtxCancelCleanup: a cancelled wait removes the ticket.
func TestIMApproverCtxCancelCleanup(t *testing.T) {
	ima, hub, _ := newIMApproverForTest(t, "oc_1")
	ctx, cancel := context.WithCancel(context.Background())
	res := startApprove(ctx, ima, core.ApprovalRequest{Action: "write", Resource: "f"})

	waitForPending(t, hub, 1)
	cancel()
	select {
	case r := <-res:
		if r.err == nil || r.approved {
			t.Fatalf("cancelled Approve = (%v, %v), want error", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after cancel")
	}
	if len(hub.Pending()) != 0 {
		t.Error("cancelled ticket must leave pending")
	}
}

// TestIMApproverNoChatIDDegrades: no chat id means no IM push, but the ticket
// is still created and resolvable over SSE/HTTP (hub API).
func TestIMApproverNoChatIDDegrades(t *testing.T) {
	var mu sync.Mutex
	var names []string
	hub := NewApprovalHub(func(e Event) {
		mu.Lock()
		names = append(names, e.Name)
		mu.Unlock()
	})
	rec := &recordingChannel{name: "feishu", updatesC: make(chan channels.Message, 8)}
	reg := channels.NewRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatal(err)
	}
	ima := NewIMApprover(hub, reg, "")

	res := startApprove(context.Background(), ima, core.ApprovalRequest{Action: "bash", Resource: "x"})
	id := waitForPending(t, hub, 1)[0].ID

	if got := rec.sentMessages(); len(got) != 0 {
		t.Errorf("no chat id must not push IM, sent %+v", got)
	}
	if err := hub.Resolve(id, "approve", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	select {
	case r := <-res:
		if r.err != nil || !r.approved {
			t.Fatalf("Approve = (%v, %v), want approved", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(names) == 0 || names[0] != api.EventApprovalNew {
		t.Errorf("notify = %v, want approval.requested first", names)
	}
}

// TestIMApproverPushFailureDegrades: a failing IM push never blocks approval;
// the ticket stays resolvable from the decision reply.
func TestIMApproverPushFailureDegrades(t *testing.T) {
	other := &stubChannel{name: "telegram", updatesC: make(chan channels.Message, 1)}
	reg := channels.NewRegistry()
	if err := reg.Register(other); err != nil {
		t.Fatal(err)
	}
	hub := NewApprovalHub(nil)
	ima := NewIMApprover(hub, reg, "oc_1") // no feishu channel: Send fails

	res := startApprove(context.Background(), ima, core.ApprovalRequest{Action: "bash", Resource: "x"})
	id := waitForPending(t, hub, 1)[0].ID
	if !ima.HandleDecision("approve " + id) {
		t.Fatal("decision must be consumed even after a failed push")
	}
	select {
	case r := <-res:
		if r.err != nil || !r.approved {
			t.Fatalf("Approve = (%v, %v), want approved", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return")
	}
}

// newIMServer builds a production-shaped server: real ApprovalHub gate and a
// channel registry on the app (Options.Gate / Options.App as in main.go).
func newIMServer(t *testing.T) (*Server, *ApprovalHub, *stubChannel) {
	t.Helper()
	feishu := &stubChannel{name: "feishu", updatesC: make(chan channels.Message, 8)}
	reg := channels.NewRegistry()
	if err := reg.Register(feishu); err != nil {
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
	s, err := New(Options{
		Home: home, App: a, Store: app.StoreOf(a), Token: "test-token", Gate: hub})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, hub, feishu
}

// TestServerWiresIMApprover: New auto-wraps the hub when the app has channels
// (chat id from the env), and skips wrapping otherwise.
func TestServerWiresIMApprover(t *testing.T) {
	t.Setenv(ApprovalChatIDEnv, "oc_env")
	s, _, _ := newIMServer(t)
	if s.imApprover == nil {
		t.Fatal("hub + channels must auto-wire the IM approver")
	}
	if s.imApprover.defaultChat != "oc_env" {
		t.Errorf("defaultChat = %q, want oc_env", s.imApprover.defaultChat)
	}

	// A non-hub gate never wraps.
	s2, _ := newTestServerWithChannel(t, &stubChannel{name: "feishu", updatesC: make(chan channels.Message, 1)})
	if s2.imApprover != nil {
		t.Error("stubGate must not wire the IM approver")
	}
	// A hub gate without channels never wraps.
	s3, _, _ := newTestServerWithGate(t, NewApprovalHub(nil))
	if s3.imApprover != nil {
		t.Error("no channels must not wire the IM approver")
	}
}

// TestBridgeDecisionNotConversation: decision replies resolve the ticket and
// never surface as message.received; plain chat still bridges to SSE.
func TestBridgeDecisionNotConversation(t *testing.T) {
	t.Setenv(ApprovalChatIDEnv, "oc_env")
	s, hub, fs := newIMServer(t)
	// Decisions are only trusted from a whitelisted inbound chat (the security
	// fix); the stub channel carries no policy, so wire one explicitly.
	s.inbound = newInboundState(s, map[string]channels.InboundPolicy{
		"feishu": {Enabled: true, AllowedChats: []string{"oc_1"}},
	})
	ch, cancel := s.hub.subscribe()
	defer cancel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	if err := s.app.Channels().Start(ctx); err != nil {
		t.Fatal(err)
	}
	go s.bridgeChannelUpdates(ctx)

	// One pending approval resolved through the IM reply.
	res := startApprove(context.Background(), hub, core.ApprovalRequest{Action: "bash", Resource: "x"})
	id := waitForPending(t, hub, 1)[0].ID
	fs.updatesC <- channels.Message{ChatID: "oc_1", From: "u1", Text: "approve " + id}
	select {
	case r := <-res:
		if r.err != nil || !r.approved {
			t.Fatalf("Approve = (%v, %v), want approved", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decision reply never resolved the ticket")
	}

	// The decision is not conversation: no message.received for it. Only the
	// plain chat bridges; approval.* events (ticket lifecycle) are expected.
	fs.updatesC <- channels.Message{ChatID: "oc_1", From: "u1", Text: "hello"}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-ch:
			switch ev.Name {
			case api.EventApprovalNew, api.EventApprovalDone:
				continue // ticket lifecycle, not conversation
			case api.EventMessage:
				body := ev.Data.(map[string]any)
				if body["text"] != "hello" {
					t.Fatalf("bridged %v, decision must not become message.received", body["text"])
				}
				return // plain chat bridged; decision stayed out
			default:
				t.Fatalf("unexpected event %q", ev.Name)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("plain chat never bridged to SSE")
		}
	}
	t.Fatal("plain chat never bridged to SSE")
}

// TestBridgeBotDecisionRejected: a bot reply in a whitelisted chat must never
// resolve an approval ticket (only humans decide); the human can still do so.
func TestBridgeBotDecisionRejected(t *testing.T) {
	t.Setenv(ApprovalChatIDEnv, "oc_env")
	s, hub, fs := newIMServer(t)
	s.inbound = newInboundState(s, map[string]channels.InboundPolicy{
		"feishu": {Enabled: true, AllowedChats: []string{"oc_1"}},
	})
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	if err := s.app.Channels().Start(ctx); err != nil {
		t.Fatal(err)
	}
	go s.bridgeChannelUpdates(ctx)

	res := startApprove(context.Background(), hub, core.ApprovalRequest{Action: "bash", Resource: "x"})
	id := waitForPending(t, hub, 1)[0].ID
	fs.updatesC <- channels.Message{ChatID: "oc_1", From: "u1", FromBot: true, Text: "approve " + id}
	select {
	case r := <-res:
		t.Fatalf("bot decision resolved the ticket: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if got := len(hub.Pending()); got != 1 {
		t.Fatalf("pending = %d, want 1 after a bot decision", got)
	}
	// A human reply still resolves it.
	fs.updatesC <- channels.Message{ChatID: "oc_1", From: "u1", Text: "approve " + id}
	select {
	case r := <-res:
		if r.err != nil || !r.approved {
			t.Fatalf("human decision = (%v, %v), want approved", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("human decision never resolved the ticket")
	}
}

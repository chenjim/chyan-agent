package daemon

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/channels"
	"chyan.tech/chyan/internal/store"
)

// stubChannel implements channels.Channel; webhook-capable variant records
// the webhook hit.
type stubChannel struct {
	name     string
	gotPath  string
	updatesC chan channels.Message
}

func (c *stubChannel) Name() string                                  { return c.name }
func (c *stubChannel) Connect(context.Context) error                 { return nil }
func (c *stubChannel) Close() error                                  { return nil }
func (c *stubChannel) Send(context.Context, channels.Outgoing) error { return nil }
func (c *stubChannel) Updates() <-chan channels.Message              { return c.updatesC }
func (c *stubChannel) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	c.gotPath = r.URL.Path
	w.WriteHeader(http.StatusOK)
}

// newTestServerWithChannel builds a server whose app carries a fake channel
// registry (Options.Channels injection).
func newTestServerWithChannel(t *testing.T, chans ...channels.Channel) (*Server, *stubChannel) {
	t.Helper()
	reg := channels.NewRegistry()
	var feishu *stubChannel
	for _, c := range chans {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
		if sc, ok := c.(*stubChannel); ok && sc.name == "feishu" {
			feishu = sc
		}
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
	s, err := New(Options{Home: home, App: a, Store: app.StoreOf(a), Token: "test-token", Gate: &stubGate{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, feishu
}

// TestFeishuWebhookMounted: the endpoint reaches the feishu adapter.
func TestFeishuWebhookMounted(t *testing.T) {
	fs := &stubChannel{name: "feishu", updatesC: make(chan channels.Message, 1)}
	s, got := newTestServerWithChannel(t, fs)

	w, req := doJSON(t, http.MethodPost, "/api/v1/channels/feishu/webhook",
		map[string]any{"challenge": "x"}, "test-token")
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook status = %d", w.Code)
	}
	if got == nil || got.gotPath == "" {
		t.Error("webhook must reach the feishu adapter")
	}

	// Without a feishu channel: 404.
	s2, _, _ := newTestServer(t)
	w, req = doJSON(t, http.MethodPost, "/api/v1/channels/feishu/webhook", nil, "test-token")
	s2.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("no feishu channel status = %d, want 404", w.Code)
	}
}

// TestBridgePublishesMessages: inbound channel updates become SSE events.
func TestBridgePublishesMessages(t *testing.T) {
	fs := &stubChannel{name: "feishu", updatesC: make(chan channels.Message, 8)}
	s, _ := newTestServerWithChannel(t, fs)

	// Subscribe before the bridge runs, like an SSE client would.
	ch, cancel := s.hub.subscribe()
	defer cancel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	// Registry.Start runs the per-channel relay into the merged stream.
	if err := s.app.Channels().Start(ctx); err != nil {
		t.Fatal(err)
	}
	go s.bridgeChannelUpdates(ctx)

	fs.updatesC <- channels.Message{ChatID: "oc_1", From: "u1", Text: "hello", At: time.Now()}

	select {
	case ev := <-ch:
		if ev.Name != api.EventMessage {
			t.Fatalf("event = %q, want %q", ev.Name, api.EventMessage)
		}
		body, ok := ev.Data.(map[string]any)
		if !ok {
			t.Fatalf("data = %#v", ev.Data)
		}
		if body["text"] != "hello" || body["chat_id"] != "oc_1" {
			t.Errorf("payload = %#v", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("message.received never published")
	}
}

// TestBridgeNoChannelsIsNoop: a server without channels never panics.
func TestBridgeNoChannelsIsNoop(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.bridgeChannelUpdates(ctx) // returns immediately, no channel
}

// TestMessageEventShape keeps the PWA payload contract stable.
func TestMessageEventShape(t *testing.T) {
	got := messageEvent(channels.TaggedMessage{
		Channel: "feishu",
		Message: channels.Message{ChatID: "oc_9", From: "u", Text: "t"},
	})
	for _, k := range []string{"channel", "chat_id", "from", "text", "at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("payload missing %q: %#v", k, got)
		}
	}
	if !strings.Contains("feishu", got["channel"].(string)) {
		t.Errorf("channel = %v", got["channel"])
	}
}

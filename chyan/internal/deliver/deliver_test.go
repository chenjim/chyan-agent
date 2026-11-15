package deliver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/channels"
)

// fakeChannel records sends and echoes the channel name.
type fakeChannel struct {
	name   string
	chatID string
	text   string
	err    error
}

func (c *fakeChannel) Name() string                  { return c.name }
func (c *fakeChannel) Connect(context.Context) error { return nil }
func (c *fakeChannel) Close() error                  { return nil }
func (c *fakeChannel) Updates() <-chan channels.Message {
	return make(chan channels.Message)
}
func (c *fakeChannel) Send(_ context.Context, out channels.Outgoing) error {
	if c.err != nil {
		return c.err
	}
	c.chatID, c.text = out.ChatID, out.Text
	return nil
}

func newRouter(t *testing.T, chans ...channels.Channel) *Router {
	t.Helper()
	reg := channels.NewRegistry()
	for _, c := range chans {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	return NewRouter(reg)
}

// TestRouteParsesTarget: "name:chatID" routes to that channel with the chat
// id; bare "name" routes with an empty chat id (adapter default).
func TestRouteParsesTarget(t *testing.T) {
	fs := &fakeChannel{name: "feishu"}
	tg := &fakeChannel{name: "telegram"}
	r := newRouter(t, fs, tg)

	if err := r.Route(context.Background(), "feishu:oc_abc", "hello"); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if fs.chatID != "oc_abc" || fs.text != "hello" {
		t.Errorf("feishu got chat=%q text=%q", fs.chatID, fs.text)
	}
	if tg.text != "" {
		t.Errorf("telegram must not receive: %q", tg.text)
	}

	if err := r.Route(context.Background(), "telegram", "hi"); err != nil {
		t.Fatalf("Route bare: %v", err)
	}
	if tg.chatID != "" || tg.text != "hi" {
		t.Errorf("telegram got chat=%q text=%q", tg.chatID, tg.text)
	}
}

// TestRouteErrors: unknown channel, empty target, adapter failure, no
// registry all surface as errors.
func TestRouteErrors(t *testing.T) {
	r := newRouter(t, &fakeChannel{name: "feishu"})
	if err := r.Route(context.Background(), "ghost:x", "t"); err == nil {
		t.Error("unknown channel must error")
	}
	if err := r.Route(context.Background(), "", "t"); err == nil {
		t.Error("empty target must error")
	}
	if err := r.Route(context.Background(), ":chat", "t"); err == nil {
		t.Error("empty channel name must error")
	}
	bad := &fakeChannel{name: "feishu", err: errors.New("api down")}
	rBad := newRouter(t, bad)
	if err := rBad.Route(context.Background(), "feishu:x", "t"); err == nil {
		t.Error("adapter failure must surface")
	}
	var nilRouter *Router
	if err := nilRouter.Route(context.Background(), "feishu:x", "t"); err == nil {
		t.Error("nil router must error")
	}
	if err := NewRouter(nil).Route(context.Background(), "feishu:x", "t"); err == nil {
		t.Error("nil registry must error")
	}
	if err := r.Route(context.Background(), "   ", "t"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("blank target err = %v", err)
	}
}

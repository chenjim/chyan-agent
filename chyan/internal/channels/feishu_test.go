package channels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const feishuMsgEvent = `{
  "schema": "2.0",
  "header": {"event_id": "e1", "event_type": "im.message.receive_v1"},
  "event": {
    "sender": {"sender_id": {"open_id": "ou_user1"}},
    "message": {
      "message_id": "m1",
      "chat_id": "oc_chat1",
      "content": "{\"text\":\"hello chyan\"}"
    }
  }
}`

func TestFeishuWebhookParsesMessage(t *testing.T) {
	f := NewFeishu(FeishuConfig{})
	req := httptest.NewRequest(http.MethodPost, "/webhook", stringsReader(feishuMsgEvent))
	w := httptest.NewRecorder()
	f.HandleWebhook(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	select {
	case m := <-f.Updates():
		if m.ID != "m1" || m.ChatID != "oc_chat1" || m.From != "ou_user1" || m.Text != "hello chyan" {
			t.Fatalf("got %+v", m)
		}
		if m.Raw == nil {
			t.Fatal("Raw not populated")
		}
	case <-time.After(time.Second):
		t.Fatal("no message delivered")
	}
}

func TestFeishuWebhookURLVerification(t *testing.T) {
	f := NewFeishu(FeishuConfig{})
	body := `{"type":"url_verification","challenge":"tok123"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", stringsReader(body))
	w := httptest.NewRecorder()
	f.HandleWebhook(w, req)
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["challenge"] != "tok123" {
		t.Fatalf("challenge = %q", resp["challenge"])
	}
}

func TestFeishuWebhookBadJSON(t *testing.T) {
	f := NewFeishu(FeishuConfig{})
	req := httptest.NewRequest(http.MethodPost, "/webhook", stringsReader("{not json"))
	w := httptest.NewRecorder()
	f.HandleWebhook(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestFeishuWebhookNonMessageEvent(t *testing.T) {
	f := NewFeishu(FeishuConfig{})
	body := `{"schema":"2.0","header":{"event_type":"im.chat.updated_v1"},"event":{"a":1}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", stringsReader(body))
	w := httptest.NewRecorder()
	f.HandleWebhook(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	select {
	case m := <-f.Updates():
		t.Fatalf("unexpected message %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestFeishuWebhookMethodNotAllowed(t *testing.T) {
	f := NewFeishu(FeishuConfig{})
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	f.HandleWebhook(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestFeishuWebhookSignature(t *testing.T) {
	const token = "verify-token"
	f := NewFeishu(FeishuConfig{WebhookToken: token})

	// unsigned request must be rejected
	req := httptest.NewRequest(http.MethodPost, "/webhook", stringsReader(feishuMsgEvent))
	w := httptest.NewRecorder()
	f.HandleWebhook(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unsigned status = %d, want 403", w.Code)
	}

	// correctly signed request passes
	req = httptest.NewRequest(http.MethodPost, "/webhook", stringsReader(feishuMsgEvent))
	req.Header.Set("X-Lark-Request-Timestamp", "1700000000")
	req.Header.Set("X-Lark-Request-Nonce", "n1")
	req.Header.Set("X-Lark-Signature", computeFeishuSignature(token, "1700000000", "n1", feishuMsgEvent))
	w = httptest.NewRecorder()
	f.HandleWebhook(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("signed status = %d, body = %s", w.Code, w.Body.String())
	}
}

// Send must first fetch tenant_access_token, then post the message; the token
// must be cached so the second Send reuses it (2 requests for 2 sends).
func TestFeishuSendFetchesTokenAndCaches(t *testing.T) {
	var mu sync.Mutex
	tokenCalls, msgCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenCalls++
			if r.Method != http.MethodPost {
				t.Errorf("token method = %s", r.Method)
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["app_id"] != "cli_x" || body["app_secret"] != "sec-y" {
				t.Errorf("token body = %v", body)
			}
			w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"t-abc","expire":7200}`))
		case "/open-apis/im/v1/messages":
			msgCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer t-abc" {
				t.Errorf("auth header = %q", got)
			}
			if got := r.URL.Query().Get("receive_id_type"); got != "open_id" {
				t.Errorf("receive_id_type = %q", got)
			}
			var body struct {
				ReceiveID string `json:"receive_id"`
				MsgType   string `json:"msg_type"`
				Content   string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ReceiveID != "oc_1" || body.MsgType != "text" {
				t.Errorf("msg body = %+v", body)
			}
			var content map[string]string
			_ = json.Unmarshal([]byte(body.Content), &content)
			if content["text"] != "ping" {
				t.Errorf("content = %v", content)
			}
			w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := NewFeishu(FeishuConfig{
		AppID:     "cli_x",
		AppSecret: "sec-y",
		BaseURL:   srv.URL,
	})
	ctx := context.Background()
	if err := f.Send(ctx, Outgoing{ChatID: "oc_1", Text: "ping"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Send(ctx, Outgoing{ChatID: "oc_1", Text: "ping"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1 (cached)", tokenCalls)
	}
	if msgCalls != 2 {
		t.Fatalf("message calls = %d, want 2", msgCalls)
	}
}

func TestFeishuSendTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":99991663,"msg":"app_id invalid"}`))
	}))
	defer srv.Close()
	f := NewFeishu(FeishuConfig{AppID: "bad", AppSecret: "bad", BaseURL: srv.URL})
	if err := f.Send(context.Background(), Outgoing{ChatID: "c", Text: "x"}); err == nil {
		t.Fatal("expected token error")
	}
}

func TestFeishuSendAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			w.Write([]byte(`{"code":0,"tenant_access_token":"t"}`))
		default:
			w.Write([]byte(`{"code":230001,"msg":"bot not in chat"}`))
		}
	}))
	defer srv.Close()
	f := NewFeishu(FeishuConfig{AppID: "a", AppSecret: "b", BaseURL: srv.URL})
	err := f.Send(context.Background(), Outgoing{ChatID: "c", Text: "x"})
	if err == nil {
		t.Fatal("expected send error")
	}
}

func TestFeishuSendNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // immediately closed -> connection refused
	f := NewFeishu(FeishuConfig{AppID: "a", AppSecret: "b", BaseURL: srv.URL})
	if err := f.Send(context.Background(), Outgoing{ChatID: "c", Text: "x"}); err == nil {
		t.Fatal("expected network error")
	}
}

func TestFeishuCloseIdempotent(t *testing.T) {
	f := NewFeishu(FeishuConfig{})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-f.Updates(); ok {
		t.Fatal("updates should be closed")
	}
}

func TestFeishuWebhookAfterClose(t *testing.T) {
	f := NewFeishu(FeishuConfig{})
	_ = f.Close()
	req := httptest.NewRequest(http.MethodPost, "/webhook", stringsReader(feishuMsgEvent))
	w := httptest.NewRecorder()
	f.HandleWebhook(w, req) // must not panic or block
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestFeishuConnectNoop(t *testing.T) {
	f := NewFeishu(FeishuConfig{})
	if err := f.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Name() != "feishu" {
		t.Fatalf("name = %q", f.Name())
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

// computeFeishuSignature replicates the platform signature for tests.
func computeFeishuSignature(token, timestamp, nonce, body string) string {
	h := sha256.New()
	h.Write([]byte(timestamp + nonce + token))
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

func TestFeishuWebhookMarksAppMessages(t *testing.T) {
	body := `{
  "schema": "2.0",
  "header": {"event_id": "e2", "event_type": "im.message.receive_v1"},
  "event": {
    "sender": {"sender_type": "app", "sender_id": {"open_id": "ou_bot"}},
    "message": {"message_id": "m2", "chat_id": "oc_chat1", "content": "{\"text\":\"echo\"}"}
  }
}`
	f := NewFeishu(FeishuConfig{})
	req := httptest.NewRequest(http.MethodPost, "/webhook", stringsReader(body))
	w := httptest.NewRecorder()
	f.HandleWebhook(w, req)
	select {
	case m := <-f.Updates():
		if !m.FromBot {
			t.Fatalf("app message not marked FromBot: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("no update")
	}
}

func TestFeishuInboundPolicy(t *testing.T) {
	p := InboundPolicy{Enabled: true, AllowedChats: []string{"oc_chat1"}}
	f := NewFeishu(FeishuConfig{Inbound: p})
	if got := f.InboundPolicy(); !got.Enabled || len(got.AllowedChats) != 1 {
		t.Fatalf("policy = %+v", got)
	}
}

// TestFeishuFetchImage downloads an inbound image attachment (FR-10).
func TestFeishuFetchImage(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"tok","expire":7200}`))
		case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages/m1/resources/img_key"):
			if r.URL.Query().Get("type") != "image" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("auth = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write(png)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := NewFeishu(FeishuConfig{AppID: "id", AppSecret: "sec", BaseURL: srv.URL})
	data, mime, err := f.FetchImage(context.Background(), "m1", "img_key")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(data) != string(png) || mime != "image/png" {
		t.Fatalf("data = %d bytes, mime = %q", len(data), mime)
	}
}

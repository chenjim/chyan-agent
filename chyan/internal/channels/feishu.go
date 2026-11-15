// Feishu channel adapter: webhook inbound (im.message.receive_v1) plus
// open.feishu.cn HTTP outbound with tenant_access_token caching.
package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"chyan.tech/chyan/internal/core"
)

// DefaultFeishuBaseURL is the Feishu open platform endpoint.
const DefaultFeishuBaseURL = "https://open.feishu.cn"

// FeishuConfig configures the feishu adapter. AppSecret is expected to arrive
// already resolved from a "secret:" reference (internal/secret); no credential
// is ever stored in code. WebhookToken enables the HMAC-SHA256 signature check
// of the X-Lark-Signature header when non-empty (empty disables verification).
type FeishuConfig struct {
	AppID         string
	AppSecret     string
	WebhookToken  string
	BaseURL       string        // defaults to DefaultFeishuBaseURL, injectable for tests
	HTTPClient    *http.Client  // optional
	ReceiveIDType string        // "open_id"(default) | "user_id" | "union_id" | "chat_id"
	TokenTTL      time.Duration // conservative cached token lifetime, default 5 min
	// Inbound controls whether webhook messages may start agent runs.
	Inbound InboundPolicy
}

// Feishu is the feishu Channel implementation. Inbound messages arrive via
// HandleWebhook (the caller mounts it on its HTTP server); outbound messages
// go through the im/v1/messages open API.
type Feishu struct {
	cfg FeishuConfig
	hc  *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time

	updates chan Message
	closed  bool
}

var _ Channel = (*Feishu)(nil)

// NewFeishu returns a feishu channel with the given config.
func NewFeishu(cfg FeishuConfig) *Feishu {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultFeishuBaseURL
	}
	if cfg.ReceiveIDType == "" {
		cfg.ReceiveIDType = "open_id"
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 5 * time.Minute
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Feishu{cfg: cfg, hc: hc, updates: make(chan Message, 64)}
}

// Name implements Channel.
func (f *Feishu) Name() string { return "feishu" }

// Connect implements Channel. There is no persistent link; inbound traffic is
// pushed by the platform through HandleWebhook, so this is a no-op.
func (f *Feishu) Connect(ctx context.Context) error { return nil }

// Close stops the inbound stream. Safe to call twice.
func (f *Feishu) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.updates)
	return nil
}

// Updates implements Channel.
func (f *Feishu) Updates() <-chan Message { return f.updates }

// HandleWebhook parses a Feishu event callback (im.message.receive_v1) and
// pushes the message into the updates stream. It answers url_verification
// challenges and otherwise acknowledges with code "0".
func (f *Feishu) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if f.cfg.WebhookToken != "" {
		sig := r.Header.Get("X-Lark-Signature")
		ts := r.Header.Get("X-Lark-Request-Timestamp")
		nonce := r.Header.Get("X-Lark-Request-Nonce")
		if !feishuSignatureOK(f.cfg.WebhookToken, ts, nonce, body, sig) {
			http.Error(w, "bad signature", http.StatusForbidden)
			return
		}
	}
	var evt feishuEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if evt.Type == "url_verification" {
		writeJSON(w, map[string]string{"challenge": evt.Challenge})
		return
	}
	msg, ok := parseFeishuEvent(&evt)
	if !ok {
		writeJSON(w, map[string]string{"code": "0"}) // acknowledged but not a message
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		writeJSON(w, map[string]string{"code": "0"})
		return
	}
	f.updates <- msg
	f.mu.Unlock()
	writeJSON(w, map[string]string{"code": "0"})
}

// FetchImage downloads an inbound image attachment by message id + image key
// (FR-10). The response is capped at the shared image limit + 1.
func (f *Feishu) FetchImage(ctx context.Context, messageID, imageKey string) ([]byte, string, error) {
	token, err := f.tenantAccessToken(ctx)
	if err != nil {
		return nil, "", err
	}
	url := fmt.Sprintf("%s/open-apis/im/v1/messages/%s/resources/%s?type=image",
		f.cfg.BaseURL, url.PathEscape(messageID), url.PathEscape(imageKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("channels/feishu: fetch image: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(core.MaxImageBytes)+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > core.MaxImageBytes {
		return nil, "", fmt.Errorf("channels/feishu: image over %d bytes", core.MaxImageBytes)
	}
	mime := core.DetectImageMIME(data)
	if mime == "" {
		return nil, "", fmt.Errorf("channels/feishu: unsupported image format")
	}
	return data, mime, nil
}

// InboundPolicy exposes the configured inbound policy (FR-7).
func (f *Feishu) InboundPolicy() InboundPolicy { return f.cfg.Inbound }

// Send implements Channel: fetch (or reuse) a tenant_access_token, then post
// the message to im/v1/messages.
func (f *Feishu) Send(ctx context.Context, out Outgoing) error {
	token, err := f.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	path := "/open-apis/im/v1/messages?receive_id_type=" + f.cfg.ReceiveIDType
	payload := map[string]any{
		"receive_id": out.ChatID,
		"msg_type":   "text",
		"content":    mustJSON(map[string]string{"text": out.Text}),
	}
	// A threaded reply goes through the message reply endpoint so it lands in
	// the originating thread instead of starting a new chat-level message.
	if out.ThreadID != "" {
		path = "/open-apis/im/v1/messages/" + url.PathEscape(out.ThreadID) + "/reply"
		payload = map[string]any{
			"msg_type":        "text",
			"content":         mustJSON(map[string]string{"text": out.Text}),
			"reply_in_thread": true,
		}
	}
	status, raw, err := f.post(ctx, path, token, payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("channels/feishu: send message: status %d: %s", status, truncate(string(raw), 200))
	}
	var resp feishuAPIResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("channels/feishu: send message: bad response: %w", err)
	}
	if resp.Code != 0 {
		return fmt.Errorf("channels/feishu: send message: code %d: %s", resp.Code, resp.Msg)
	}
	return nil
}

// tenantAccessToken returns the cached token or fetches a fresh one via the
// app_id/app_secret auth endpoint. The token is cached for TokenTTL (shorter
// than the server-side validity).
func (f *Feishu) tenantAccessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	if f.token != "" && time.Now().Before(f.expiry) {
		t := f.token
		f.mu.Unlock()
		return t, nil
	}
	f.mu.Unlock()

	status, raw, err := f.post(ctx, "/open-apis/auth/v3/tenant_access_token/internal", "", map[string]string{
		"app_id":     f.cfg.AppID,
		"app_secret": f.cfg.AppSecret,
	})
	if err != nil {
		return "", fmt.Errorf("channels/feishu: get token: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("channels/feishu: get token: status %d: %s", status, truncate(string(raw), 200))
	}
	var tok feishuTokenResponse
	if err := json.Unmarshal(raw, &tok); err != nil {
		return "", fmt.Errorf("channels/feishu: get token: bad response: %w", err)
	}
	if tok.Code != 0 || tok.Token == "" {
		return "", fmt.Errorf("channels/feishu: get token: code %d: %s", tok.Code, tok.Msg)
	}
	f.mu.Lock()
	f.token = tok.Token
	f.expiry = time.Now().Add(f.cfg.TokenTTL)
	f.mu.Unlock()
	return tok.Token, nil
}

// post sends a JSON request; bearerToken may be empty. Returns status and body.
func (f *Feishu) post(ctx context.Context, path, bearerToken string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// --- feishu wire types ---

// feishuEvent covers both the v2 event envelope and url_verification bodies.
type feishuEvent struct {
	Header    *feishuHeader   `json:"header"`
	Event     json.RawMessage `json:"event"`
	Type      string          `json:"type"`      // url_verification
	Challenge string          `json:"challenge"` // url_verification
}

type feishuHeader struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
}

// parseFeishuEvent extracts a Message from an im.message.receive_v1 event.
func parseFeishuEvent(evt *feishuEvent) (Message, bool) {
	if evt == nil {
		return Message{}, false
	}
	var e struct {
		Sender struct {
			SenderType string `json:"sender_type"` // "user" | "app"
			SenderID   struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
		} `json:"sender"`
		Message struct {
			MessageID string `json:"message_id"`
			ChatID    string `json:"chat_id"`
			ThreadID  string `json:"thread_id"`
			RootID    string `json:"root_id"`
			Content   string `json:"content"` // JSON-encoded {"text": "..."}
		} `json:"message"`
	}
	if err := json.Unmarshal(evt.Event, &e); err != nil {
		return Message{}, false
	}
	if e.Message.MessageID == "" && e.Message.ChatID == "" {
		return Message{}, false
	}
	var content struct {
		Text     string `json:"text"`
		ImageKey string `json:"image_key"`
	}
	_ = json.Unmarshal([]byte(e.Message.Content), &content)
	// root_id is both the stable thread identity and a message id usable by
	// the reply endpoint; thread_id alone is not addressable for replies.
	threadID := e.Message.RootID
	if threadID == "" {
		threadID = e.Message.ThreadID
	}
	return Message{
		ID:       e.Message.MessageID,
		ChatID:   e.Message.ChatID,
		From:     e.Sender.SenderID.OpenID,
		Text:     content.Text,
		ImageKey: content.ImageKey,
		ThreadID: threadID,
		FromBot:  e.Sender.SenderType == "app",
		Raw:      evt,
	}, true
}

type feishuAPIResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

type feishuTokenResponse struct {
	Code  int    `json:"code"`
	Msg   string `json:"msg"`
	Token string `json:"tenant_access_token"`
}

// feishuSignatureOK verifies the webhook signature: sha256(timestamp+nonce+
// token, body) hex-encoded, compared against X-Lark-Signature.
func feishuSignatureOK(token, timestamp, nonce string, body []byte, want string) bool {
	if want == "" {
		return false
	}
	h := sha256.New()
	h.Write([]byte(timestamp + nonce + token))
	h.Write(body)
	got := hex.EncodeToString(h.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

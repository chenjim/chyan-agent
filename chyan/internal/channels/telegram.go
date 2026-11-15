// Telegram channel adapter: Bot API long polling inbound (getUpdates) and
// sendMessage outbound.
package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"chyan.tech/chyan/internal/core"
)

// DefaultTelegramBaseURL is the Telegram Bot API endpoint.
const DefaultTelegramBaseURL = "https://api.telegram.org"

// TelegramConfig configures the telegram adapter. Token is expected to arrive
// already resolved from a "secret:" reference (internal/secret); no credential
// is ever stored in code. BaseURL is injectable for tests.
type TelegramConfig struct {
	Token       string
	BaseURL     string        // defaults to DefaultTelegramBaseURL
	HTTPClient  *http.Client  // optional; Timeout must exceed PollTimeout
	PollTimeout time.Duration // long-poll wait per getUpdates call, default 25s
	// Inbound controls whether polled messages may start agent runs.
	Inbound InboundPolicy
}

// InboundPolicy exposes the configured inbound policy (FR-7).
func (t *Telegram) InboundPolicy() InboundPolicy { return t.cfg.Inbound }

// Telegram is the telegram Channel implementation. Inbound messages are
// collected by long polling in a background goroutine started by Connect.
type Telegram struct {
	cfg TelegramConfig
	hc  *http.Client

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	offset int64
	closed bool

	updates chan Message
}

var _ Channel = (*Telegram)(nil)

// NewTelegram returns a telegram channel with the given config.
func NewTelegram(cfg TelegramConfig) *Telegram {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultTelegramBaseURL
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 25 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.PollTimeout + 10*time.Second}
	}
	return &Telegram{cfg: cfg, hc: hc, updates: make(chan Message, 64), done: make(chan struct{})}
}

// Name implements Channel.
func (t *Telegram) Name() string { return "telegram" }

// Connect starts the long-polling loop. Poll errors (network, bad response)
// degrade the loop into a short wait and retry, without blocking other
// channels; they do not fail Connect itself.
func (t *Telegram) Connect(ctx context.Context) error {
	t.mu.Lock()
	if t.cancel != nil {
		t.mu.Unlock()
		return nil // already started
	}
	pctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.mu.Unlock()
	go t.pollLoop(pctx)
	return nil
}

// Close stops polling and closes the updates stream. Safe to call twice.
func (t *Telegram) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		<-t.done
	}
	close(t.updates)
	return nil
}

// Updates implements Channel.
func (t *Telegram) Updates() <-chan Message { return t.updates }

// pollLoop long-polls getUpdates until the context ends. Each fetched update
// advances the offset so messages are delivered at most once.
func (t *Telegram) pollLoop(ctx context.Context) {
	defer close(t.done)
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := t.getUpdates(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		for _, u := range updates {
			t.mu.Lock()
			if int64(u.UpdateID) >= t.offset {
				t.offset = int64(u.UpdateID) + 1
			}
			closed := t.closed
			t.mu.Unlock()
			if u.Message == nil || closed {
				continue
			}
			msg := messageFromUpdate(u)
			select {
			case t.updates <- msg:
			case <-ctx.Done():
				return
			}
		}
	}
}

// getUpdates fetches pending updates with a long poll.
func (t *Telegram) getUpdates(ctx context.Context) ([]tgUpdate, error) {
	t.mu.Lock()
	offset := t.offset
	t.mu.Unlock()
	payload := map[string]any{
		"offset":  offset,
		"timeout": int(t.cfg.PollTimeout / time.Second),
	}
	raw, err := t.api(ctx, "getUpdates", payload)
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("channels/telegram: getUpdates: bad response: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("channels/telegram: getUpdates: not ok")
	}
	return resp.Result, nil
}

// Send implements Channel: POST sendMessage.
func (t *Telegram) Send(ctx context.Context, out Outgoing) error {
	payload := map[string]any{"text": out.Text}
	if id, err := strconv.ParseInt(out.ChatID, 10, 64); err == nil {
		payload["chat_id"] = id
	} else {
		payload["chat_id"] = out.ChatID
	}
	// Forum topics route by message_thread_id.
	if out.ThreadID != "" {
		if tid, err := strconv.ParseInt(out.ThreadID, 10, 64); err == nil {
			payload["message_thread_id"] = tid
		}
	}
	raw, err := t.api(ctx, "sendMessage", payload)
	if err != nil {
		return err
	}
	var resp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("channels/telegram: sendMessage: bad response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("channels/telegram: sendMessage: %s", resp.Description)
	}
	return nil
}

// api POSTs a Bot API method as JSON and returns the response body.
func (t *Telegram) api(ctx context.Context, method string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/bot%s/%s", t.cfg.BaseURL, t.cfg.Token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("channels/telegram: %s: status %d: %s", method, resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// messageFromUpdate maps one polled update to a channel Message. Photo
// messages carry the largest size's file id; text falls back to the caption.
func messageFromUpdate(u tgUpdate) Message {
	m := u.Message
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	imageKey, best := "", 0
	for _, p := range m.Photo {
		if p.FileID == "" {
			continue
		}
		if area := p.Width * p.Height; area >= best {
			imageKey, best = p.FileID, area
		}
	}
	threadID := ""
	if m.MessageThreadID != 0 {
		threadID = strconv.FormatInt(m.MessageThreadID, 10)
	}
	return Message{
		ID:       strconv.FormatInt(m.MessageID, 10),
		ChatID:   strconv.FormatInt(m.Chat.ID, 10),
		From:     strconv.FormatInt(m.From.ID, 10),
		Text:     text,
		ImageKey: imageKey,
		ThreadID: threadID,
		FromBot:  m.From.IsBot,
		At:       time.Unix(m.Date, 0),
		Raw:      u,
	}
}

// FetchImage downloads an inbound photo through getFile (FR-10).
func (t *Telegram) FetchImage(ctx context.Context, _messageID, fileID string) ([]byte, string, error) {
	raw, err := t.api(ctx, "getFile", map[string]string{"file_id": fileID})
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", fmt.Errorf("channels/telegram: getFile: bad response: %w", err)
	}
	if !resp.OK || resp.Result.FilePath == "" {
		return nil, "", fmt.Errorf("channels/telegram: getFile: no file_path")
	}
	fileURL := fmt.Sprintf("%s/file/bot%s/%s", strings.TrimRight(t.cfg.BaseURL, "/"), t.cfg.Token, resp.Result.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, "", err
	}
	res, err := t.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("channels/telegram: download file: status %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(core.MaxImageBytes)+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > core.MaxImageBytes {
		return nil, "", fmt.Errorf("channels/telegram: image over %d bytes", core.MaxImageBytes)
	}
	mime := core.DetectImageMIME(data)
	if mime == "" {
		return nil, "", fmt.Errorf("channels/telegram: unsupported image format")
	}
	return data, mime, nil
}

// --- telegram wire types ---

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	MessageID       int64         `json:"message_id"`
	Date            int64         `json:"date"`
	Text            string        `json:"text"`
	Caption         string        `json:"caption"`
	Photo           []tgPhotoSize `json:"photo"`
	From            tgUser        `json:"from"`
	Chat            tgChat        `json:"chat"`
	MessageThreadID int64         `json:"message_thread_id"`
}

type tgPhotoSize struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type tgUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

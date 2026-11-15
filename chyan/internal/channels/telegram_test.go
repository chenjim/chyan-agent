package channels

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramGetUpdatesAndSend(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottok/getUpdates":
			calls.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["timeout"] == nil {
				t.Errorf("getUpdates body = %v", body)
			}
			w.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"message_id":5,"date":1700000000,"text":"hi bot","from":{"id":42,"is_bot":false},"chat":{"id":77}}}]}`))
		case "/bottok/sendMessage":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["chat_id"] != float64(77) {
				t.Errorf("chat_id = %v", body["chat_id"])
			}
			if body["text"] != "pong" {
				t.Errorf("text = %v", body["text"])
			}
			w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tg := NewTelegram(TelegramConfig{Token: "tok", BaseURL: srv.URL, PollTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-tg.Updates():
		if m.ID != "5" || m.ChatID != "77" || m.From != "42" || m.Text != "hi bot" || m.FromBot {
			t.Fatalf("got %+v", m)
		}
		if m.At.IsZero() {
			t.Fatal("At not set")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no update polled")
	}
	if err := tg.Send(ctx, Outgoing{ChatID: "77", Text: "pong"}); err != nil {
		t.Fatal(err)
	}
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
}

// The poll loop must advance the offset so a message is delivered once.
func TestTelegramOffsetAdvances(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body struct {
			Offset int64 `json:"offset"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch n {
		case 1:
			w.Write([]byte(`{"ok":true,"result":[{"update_id":100,"message":{"message_id":1,"date":1,"text":"a","from":{"id":1},"chat":{"id":1}}}]}`))
		default:
			if body.Offset != 101 {
				t.Errorf("call %d offset = %d, want 101", n, body.Offset)
			}
			time.Sleep(50 * time.Millisecond) // keep long poll short but blocking
			w.Write([]byte(`{"ok":true,"result":[]}`))
		}
	}))
	defer srv.Close()

	tg := NewTelegram(TelegramConfig{Token: "tok", BaseURL: srv.URL, PollTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tg.Updates():
	case <-time.After(3 * time.Second):
		t.Fatal("no update")
	}
	deadline := time.After(3 * time.Second)
	for calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("offset never checked, calls = %d", calls.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
}

// Network errors must not kill the poll loop; it retries after a short wait.
func TestTelegramPollRecoversFromNetworkError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"ok":true,"result":[{"update_id":2,"message":{"message_id":2,"date":1,"text":"ok","from":{"id":2},"chat":{"id":2}}}]}`))
	}))
	defer srv.Close()

	tg := NewTelegram(TelegramConfig{Token: "tok", BaseURL: srv.URL, PollTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-tg.Updates():
		if m.Text != "ok" {
			t.Fatalf("got %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not recover")
	}
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramSendAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	defer srv.Close()
	tg := NewTelegram(TelegramConfig{Token: "tok", BaseURL: srv.URL})
	err := tg.Send(context.Background(), Outgoing{ChatID: "1", Text: "x"})
	if err == nil {
		t.Fatal("expected api error")
	}
}

func TestTelegramSendNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	tg := NewTelegram(TelegramConfig{Token: "tok", BaseURL: srv.URL})
	if err := tg.Send(context.Background(), Outgoing{ChatID: "1", Text: "x"}); err == nil {
		t.Fatal("expected network error")
	}
}

func TestTelegramSendBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nonsense"))
	}))
	defer srv.Close()
	tg := NewTelegram(TelegramConfig{Token: "tok", BaseURL: srv.URL})
	if err := tg.Send(context.Background(), Outgoing{ChatID: "1", Text: "x"}); err == nil {
		t.Fatal("expected bad response error")
	}
}

func TestTelegramCloseIdempotentAndNotStarted(t *testing.T) {
	tg := NewTelegram(TelegramConfig{Token: "tok"})
	if tg.Name() != "telegram" {
		t.Fatalf("name = %q", tg.Name())
	}
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-tg.Updates(); ok {
		t.Fatal("updates should be closed")
	}
}

// Close before any Connect must still close the updates stream.
func TestTelegramConnectTwiceIsNoop(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()
	tg := NewTelegram(TelegramConfig{Token: "tok", BaseURL: srv.URL, PollTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tg.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTelegramPhotoParsing picks the largest photo size and falls back to the
// caption for text (FR-10).
func TestTelegramPhotoParsing(t *testing.T) {
	msg := messageFromUpdate(tgUpdate{Message: &tgMessage{
		MessageID: 7, Date: 1700000000, Caption: "看图",
		From: tgUser{ID: 42}, Chat: tgChat{ID: 77},
		Photo: []tgPhotoSize{
			{FileID: "small", Width: 90, Height: 90},
			{FileID: "large", Width: 1280, Height: 720},
		},
	}})
	if msg.ImageKey != "large" || msg.Text != "看图" || msg.ChatID != "77" {
		t.Fatalf("message = %+v", msg)
	}
}

// TestTelegramFetchImage downloads a photo via getFile (FR-10).
func TestTelegramFetchImage(t *testing.T) {
	jpg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF") // jpeg magic is enough
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottok/getFile":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["file_id"] != "file_1" {
				t.Errorf("file_id = %v", body["file_id"])
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"file_1","file_path":"photos/file_1.jpg"}}`))
		case "/file/bottok/photos/file_1.jpg":
			_, _ = w.Write(jpg)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tg := NewTelegram(TelegramConfig{Token: "tok", BaseURL: srv.URL})
	data, mime, err := tg.FetchImage(context.Background(), "7", "file_1")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(data) != string(jpg) || mime != "image/jpeg" {
		t.Fatalf("data = %d bytes, mime = %q", len(data), mime)
	}
}

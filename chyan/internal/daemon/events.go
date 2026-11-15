// Package daemon: SSE event hub. One hub per server; publishers broadcast
// typed events, listeners receive them as SSE frames.
package daemon

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Event is one SSE frame: named JSON payload (api event names).
type Event struct {
	Name string
	Data any
}

// subscriber is one connected SSE client's buffered channel.
type subscriber struct {
	ch     chan Event
	closed bool
}

// hub fans events out to connected SSE clients. Slow clients are dropped
// rather than blocking publishers (a phone that stops reading loses the
// stream, not the agent turn).
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newHub() *hub {
	return &hub{subs: map[*subscriber]struct{}{}}
}

// subscribe registers a listener; the returned func unsubscribes.
func (h *hub) subscribe() (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, 64)}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub.ch, func() {
		h.mu.Lock()
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		delete(h.subs, sub)
		h.mu.Unlock()
	}
}

// publish delivers e to every subscriber, dropping for slow ones.
func (h *hub) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- e:
		default: // slow consumer: drop rather than block the agent
		}
	}
}

// serveSSE streams hub events to one HTTP client as server-sent events.
func (h *hub) serveSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, cancel := h.subscribe()
	defer cancel()

	// Keep-alive comment every 25s so proxies don't idle-drop the stream.
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	// A client that stops reading blocks Write forever and pins this
	// goroutine; a per-write deadline bounds it (the handler then returns).
	rc := http.NewResponseController(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(e.Data)
			if err != nil {
				continue
			}
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			if _, err := w.Write([]byte("event: " + e.Name + "\ndata: " + string(payload) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// sseWriteTimeout bounds one event write to a slow SSE consumer.
const sseWriteTimeout = 10 * time.Second

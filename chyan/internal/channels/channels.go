// Package channels defines the generic message-channel primitive shared by
// IM adapters (feishu, telegram) and, later, Agent-to-Agent messaging (D18).
package channels

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Message is one inbound message from a channel.
type Message struct {
	ID     string    // provider message id, may be empty
	ChatID string    // provider chat/conversation id
	From   string    // provider sender id
	Text   string    // plain text body
	At     time.Time // provider receive time, zero if unknown
	// ImageKey is the provider's image reference for image messages (feishu);
	// adapters that expose FetchImage can resolve it to bytes.
	ImageKey string
	// ThreadID is the provider topic/thread id (feishu thread_id/root_id,
	// telegram message_thread_id). Empty means the chat itself is the thread.
	// Each thread gets its own worker session (P1: thread→worker upgrade).
	ThreadID string
	// FromBot marks messages sent by a bot/app identity (including this
	// agent's own replies echoed back): inbound dispatch must ignore them.
	FromBot bool
	Raw     any // provider event payload, for adapters/consumers that need it
}

// InboundPolicy decides whether an inbound IM message may start an agent run
// (FR-7). The zero value disables inbound dispatch.
type InboundPolicy struct {
	Enabled bool
	// AllowedChats / AllowedUsers are exact ids; "*" matches everything.
	// Both empty means "deny all": enabling inbound without a whitelist is a
	// configuration error, never an open relay.
	AllowedChats []string
	AllowedUsers []string
	// MaxChars caps the message length (runes); 0 = DefaultMaxChars.
	MaxChars int
	// MinInterval rate-limits one chat; 0 = DefaultMinInterval.
	MinInterval time.Duration
	// MaxReplyChars caps one outbound IM reply (runes); 0 = DefaultMaxReplyChars.
	// Longer agent output is truncated and the full text stays in the PWA.
	MaxReplyChars int
	// IntentFilter enables the deterministic second-stage filter: only
	// messages that look directed at the agent (mention, slash command,
	// question, explicit request cue) start a full run. Plain chatter is
	// still published to SSE but never costs an LLM turn.
	IntentFilter bool
}

// Defaults applied when an enabled policy leaves the fields at zero.
const (
	DefaultMaxChars      = 4000
	DefaultMinInterval   = 2 * time.Second
	DefaultMaxReplyChars = 800
)

// Authorized reports whether chat/from may start a run.
func (p InboundPolicy) Authorized(chatID, from string) bool {
	return matchID(p.AllowedChats, chatID) || matchID(p.AllowedUsers, from)
}

func matchID(list []string, id string) bool {
	if id == "" {
		return false
	}
	for _, v := range list {
		if v == "*" || v == id {
			return true
		}
	}
	return false
}

// Limits returns the effective caps.
func (p InboundPolicy) Limits() (maxChars int, minInterval time.Duration) {
	maxChars, minInterval = p.MaxChars, p.MinInterval
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	if minInterval <= 0 {
		minInterval = DefaultMinInterval
	}
	return maxChars, minInterval
}

// ReplyLimit returns the effective outbound reply cap in runes.
func (p InboundPolicy) ReplyLimit() int {
	if p.MaxReplyChars > 0 {
		return p.MaxReplyChars
	}
	return DefaultMaxReplyChars
}

// ClampText truncates text to maxChars runes, preferring the last newline in
// the window so a cut never splits a sentence mid-line. It reports whether
// truncation happened so callers can attach a full-content link.
func ClampText(text string, maxChars int) (string, bool) {
	if maxChars <= 0 {
		maxChars = DefaultMaxReplyChars
	}
	if utf8.RuneCountInString(text) <= maxChars {
		return text, false
	}
	runes := []rune(text)
	cut, lastNL := maxChars, -1
	for i := 0; i < maxChars; i++ {
		if runes[i] == '\n' {
			lastNL = i
		}
	}
	if lastNL >= maxChars/2 {
		cut = lastNL
	}
	return strings.TrimRight(string(runes[:cut]), "\n") + "\n…（已截断）", true
}

// inboundCapable is implemented by adapters that carry an inbound policy.
type inboundCapable interface {
	InboundPolicy() InboundPolicy
}

// Outgoing is one outbound message request. Extra adapter-specific fields
// (e.g. FeishuConfig receive_id_type) live in adapter configs, not here.
type Outgoing struct {
	ChatID string // provider chat/conversation id
	Text   string // plain text body
	// ThreadID is the provider thread the reply belongs to (feishu root
	// message id, telegram message_thread_id). Empty replies in the chat itself.
	ThreadID string
}

// Channel is the generic message-channel primitive. IM adapters implement it
// today; A2A messaging is expected to reuse the same interface later (D18).
type Channel interface {
	Name() string
	Connect(ctx context.Context) error
	Close() error
	Send(ctx context.Context, out Outgoing) error
	Updates() <-chan Message
}

// Registry runs several channels together. Send failures on one channel never
// block the others (per-channel degradation, same principle as 4.8); inbound
// updates are merged into one stream tagged with the source channel name.
type Registry struct {
	mu       sync.RWMutex
	channels map[string]Channel
	updates  chan tagged
	closed   bool
	done     chan struct{}
}

type tagged struct {
	name string
	msg  Message
}

// TaggedMessage is a Message tagged with the name of the channel it came from.
type TaggedMessage struct {
	Channel string
	Message Message
}

// InboundPolicies returns the policy of every channel that supports inbound
// dispatch (channels without a policy are omitted).
func (r *Registry) InboundPolicies() map[string]InboundPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]InboundPolicy)
	for name, c := range r.channels {
		if p, ok := c.(inboundCapable); ok {
			out[name] = p.InboundPolicy()
		}
	}
	return out
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		channels: make(map[string]Channel),
		updates:  make(chan tagged, 64),
		done:     make(chan struct{}),
	}
}

// Register adds a channel. Names must be unique.
func (r *Registry) Register(c Channel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("channels: registry closed")
	}
	if _, ok := r.channels[c.Name()]; ok {
		return fmt.Errorf("channels: duplicate channel %q", c.Name())
	}
	r.channels[c.Name()] = c
	return nil
}

// Start connects every registered channel in parallel, keeps each channel's
// updates flowing, and degrades only the channel that fails. A Connect error
// degrades just that channel; the registry still starts the rest. Start
// returns once every Connect attempt has finished.
func (r *Registry) Start(ctx context.Context) error {
	r.mu.RLock()
	anyCh := len(r.channels) > 0
	for _, c := range r.channels {
		go r.run(ctx, c)
	}
	r.mu.RUnlock()
	if !anyCh {
		return errors.New("channels: no channels registered")
	}
	return nil
}

// run supervises one channel: connect, then relay updates into the merged
// stream until the channel closes or the context ends.
func (r *Registry) run(ctx context.Context, c Channel) {
	if err := c.Connect(ctx); err != nil {
		return // degrade this channel only
	}
	defer func() { _ = c.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		case m, ok := <-c.Updates():
			if !ok {
				return
			}
			r.push(ctx, c.Name(), m)
		}
	}
}

func (r *Registry) push(ctx context.Context, name string, m Message) {
	select {
	case r.updates <- tagged{name: name, msg: m}:
	case <-ctx.Done():
	case <-r.done:
	}
}

// Get returns one registered channel by name (daemon webhook mounting).
func (r *Registry) Get(name string) (Channel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.channels[name]
	return c, ok
}

// Updates returns the merged inbound stream from all channels. The stream
// closes after Close is called.
func (r *Registry) Updates() <-chan TaggedMessage {
	out := make(chan TaggedMessage)
	go func() {
		defer close(out)
		for {
			select {
			case t, ok := <-r.updates:
				if !ok {
					return
				}
				select {
				case out <- TaggedMessage{Channel: t.name, Message: t.msg}:
				case <-r.done:
					return
				}
			case <-r.done:
				return
			}
		}
	}()
	return out
}

// Send delivers out through the named channel.
func (r *Registry) Send(ctx context.Context, name string, out Outgoing) error {
	r.mu.RLock()
	c, ok := r.channels[name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("channels: unknown channel %q", name)
	}
	return c.Send(ctx, out)
}

// Broadcast sends out to every channel and reports the first error; it always
// attempts every channel so one failure cannot block the rest.
func (r *Registry) Broadcast(ctx context.Context, out Outgoing) error {
	r.mu.RLock()
	targets := make([]Channel, 0, len(r.channels))
	for _, c := range r.channels {
		targets = append(targets, c)
	}
	r.mu.RUnlock()
	var firstErr error
	for _, c := range targets {
		if err := c.Send(ctx, out); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("channels: %s: %w", c.Name(), err)
		}
	}
	return firstErr
}

// Close shuts down the registry and closes every channel. Safe to call twice.
func (r *Registry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.done)
	targets := make([]Channel, 0, len(r.channels))
	for _, c := range r.channels {
		targets = append(targets, c)
	}
	r.mu.Unlock()
	var firstErr error
	for _, c := range targets {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

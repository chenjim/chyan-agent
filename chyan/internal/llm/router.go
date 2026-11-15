package llm

import (
	"context"
	"fmt"
	"log"
	"sync"

	"chyan.tech/chyan/internal/core"
)

// ModelSpec describes one provider-model pair and its capabilities. The
// adapter layer (architecture 4.7) owns per-model differences: streaming
// support, window size, pricing — so the agent loop never branches on them.
type ModelSpec struct {
	Provider     string // routing key into the provider table
	Model        string // model id sent to the provider
	Window       int    // resolved context window in tokens (0 = resolve on demand)
	Streaming    bool   // provider-model supports SSE token streaming
	CostPer1KIn  float64
	CostPer1KOut float64
	// CostPer1KCached is the discounted rate for prompt tokens served from
	// cache; 0 means the provider charges them like fresh input.
	CostPer1KCached float64
}

// Client is the minimal backend contract the router needs; OpenAIClient
// satisfies it via core.LLM + core.Streamer.
type Client interface {
	core.LLM
	Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error)
	Name() string
}

// Router routes requests across provider-model pairs with a fallback chain
// and capability negotiation (streaming preferred, Chat otherwise).
type Router struct {
	mu sync.RWMutex

	clients map[string]Client // provider name -> client
	specs   []ModelSpec       // preference order; first is primary
	windows *WindowConfig     // resolves Window when a spec leaves it 0
}

// NewRouter builds a router from ordered specs and their clients. specs[0]
// is the primary model; later entries are fallbacks.
func NewRouter(specs []ModelSpec, clients map[string]Client, windows *WindowConfig) *Router {
	return &Router{specs: specs, clients: clients, windows: windows}
}

// primary returns the highest-priority spec (first with a live client).
func (r *Router) resolve() (ModelSpec, Client, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.Specs() {
		if c, ok := r.clients[s.Provider]; ok {
			return s, c, nil
		}
	}
	return ModelSpec{}, nil, fmt.Errorf("llm: no usable provider in router")
}

// Specs returns the model specs in preference order.
func (r *Router) Specs() []ModelSpec {
	return r.specs
}

// Window resolves the context window for a model spec (spec.Window if set,
// else the layered window table).
func (r *Router) Window(s ModelSpec) int {
	if s.Window > 0 {
		return s.Window
	}
	if r.windows != nil {
		n, _ := r.windows.ResolveContextLimit(s.Model)
		return n
	}
	return DefaultFallbackLimit
}

// forSpec returns the request adjusted for one spec. On the primary entry an
// explicit req.Model still pins the model (caller override, e.g. the review
// model). Fallback entries always use their own spec model: reusing the
// primary's model id against another provider is a guaranteed 4xx. A copy so
// iterations never leak the previous spec's model.
func forSpec(s ModelSpec, req core.Request, fallback bool) core.Request {
	if !fallback && req.Model != "" {
		return req
	}
	req.Model = s.Model
	return req
}

// Chat routes a non-streaming completion, falling back through the chain on
// transient errors.
func (r *Router) Chat(ctx context.Context, req core.Request) (core.Response, error) {
	var lastErr error
	var attempts int // attempts already burned by failed providers
	for i, s := range r.Specs() {
		c, ok := r.clients[s.Provider]
		if !ok {
			continue
		}
		resp, err := c.Chat(ctx, forSpec(s, req, i > 0))
		if err == nil {
			resp.Attempts += attempts
			return r.applyCost(s, resp), nil
		}
		attempts += resp.Attempts
		lastErr = err
		if !retryable(err) || i == len(r.specs)-1 {
			return core.Response{Attempts: attempts}, err
		}
		log.Printf("llm: provider %s failed (%v), falling back", s.Provider, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("llm: router has no providers")
	}
	return core.Response{Attempts: attempts}, lastErr
}

// Stream routes a streaming completion when the chosen model supports it;
// otherwise it degrades to Chat (capability negotiation). Deltas are emitted
// live for TUI/PWA output. A spec that fails before emitting any delta falls
// back to the next one; after the first delta the response is already visible
// to the user, so the failure surfaces instead of duplicating output.
func (r *Router) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	var lastErr error
	var emitted bool
	var attempts int // attempts already burned by failed providers
	wrap := onDelta
	if onDelta != nil {
		wrap = func(d core.Delta) {
			if d.Text != "" || d.Reasoning != "" {
				emitted = true
			}
			onDelta(d)
		}
	}
	specs := r.Specs()
	for i, s := range specs {
		c, ok := r.clients[s.Provider]
		if !ok {
			continue
		}
		sreq := forSpec(s, req, i > 0)
		var resp core.Response
		var err error
		if s.Streaming {
			resp, err = c.Stream(ctx, sreq, wrap)
		} else {
			// Degraded to a non-streaming call: replay its text once so the
			// UI still shows the answer instead of an empty stream.
			resp, err = c.Chat(ctx, sreq)
			if err == nil {
				replayDeltas(resp.Message, wrap)
			}
		}
		if err == nil {
			resp.Attempts += attempts
			return r.applyCost(s, resp), nil
		}
		attempts += resp.Attempts
		lastErr = err
		if emitted || !retryable(err) || i == len(specs)-1 {
			return core.Response{Attempts: attempts}, err
		}
		log.Printf("llm: provider %s failed (%v), falling back", s.Provider, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("llm: router has no providers")
	}
	return core.Response{Attempts: attempts}, lastErr
}

// replayDeltas emits a finished message as one delta per part, in order, so a
// non-streaming fallback reaches a streaming UI the same way a live stream does.
func replayDeltas(msg core.Message, onDelta func(core.Delta)) {
	if onDelta == nil {
		return
	}
	for _, p := range msg.Content {
		switch p.Type {
		case "reasoning":
			if p.Text != "" {
				onDelta(core.Delta{Reasoning: p.Text})
			}
		case "text":
			if p.Text != "" {
				onDelta(core.Delta{Text: p.Text})
			}
		}
	}
}

// applyCost fills cost accounting from the spec's pricing (usage tokens are
// set by the client). Cached prompt tokens are a subset of TokensIn and bill
// at the cached rate when one is configured.
func (r *Router) applyCost(s ModelSpec, resp core.Response) core.Response {
	in := float64(resp.TokensIn) - float64(resp.CacheReadTokens)
	if in < 0 {
		in = 0
	}
	cachedRate := s.CostPer1KCached
	if cachedRate == 0 {
		cachedRate = s.CostPer1KIn
	}
	resp.CostUSD = in/1000*s.CostPer1KIn +
		float64(resp.CacheReadTokens)/1000*cachedRate +
		float64(resp.TokensOut)/1000*s.CostPer1KOut
	if resp.Model == "" {
		resp.Model = s.Model
	}
	return resp
}

// supportsStreaming reports whether the primary model can stream. The agent
// loop uses this to pick Stream vs Chat without importing provider details.
func (r *Router) supportsStreaming() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.specs {
		if _, ok := r.clients[s.Provider]; ok {
			return s.Streaming
		}
	}
	return false
}

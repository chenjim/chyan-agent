package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"chyan.tech/chyan/internal/core"
)

// newTestServer runs an OpenAI-compatible mock and returns its URL.
func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestChatParsesTextAndUsage(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("model = %q", req.Model)
		}
		if r.Header.Get("Authorization") != "Bearer key123" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "test-model",
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "hello there"},
			}},
			"usage": map[string]any{
				"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
				"prompt_tokens_details":     map[string]any{"cached_tokens": 8},
				"completion_tokens_details": map[string]any{"reasoning_tokens": 2},
			},
		})
	})
	c := NewOpenAIClient(Provider{Name: "p", BaseURL: srv.URL, APIKey: "key123"}, "test-model")
	resp, err := c.Chat(context.Background(), core.Request{
		Messages: []core.Message{textMessage(core.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content[0].Text != "hello there" {
		t.Errorf("content = %q", resp.Message.Content[0].Text)
	}
	if resp.TokensIn != 10 || resp.TokensOut != 5 {
		t.Errorf("tokens = %d/%d", resp.TokensIn, resp.TokensOut)
	}
	if resp.CacheReadTokens != 8 || resp.ReasoningTokens != 2 {
		t.Errorf("usage details = cache %d reasoning %d", resp.CacheReadTokens, resp.ReasoningTokens)
	}
	if resp.Model != "test-model" {
		t.Errorf("model = %q", resp.Model)
	}
}

// TestChatRejectsMalformedToolArguments: a model reply with broken JSON
// arguments must fail the call, never run the tool with empty input.
func TestChatRejectsMalformedToolArguments(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "m",
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "call1",
						"type": "function",
						"function": map[string]any{
							"name":      "bash",
							"arguments": `{"command":`,
						},
					}},
				},
			}},
		})
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	if _, err := c.Chat(context.Background(), core.Request{}); err == nil ||
		!strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("malformed arguments error = %v", err)
	}
}

// TestStreamRejectsMalformedToolArguments covers the streaming aggregation path.
func TestStreamRejectsMalformedToolArguments(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\"," +
				"\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\"}}]}}]}\n\n" +
				"data: [DONE]\n\n"))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	if _, err := c.Stream(context.Background(), core.Request{}, nil); err == nil ||
		!strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("malformed arguments error = %v", err)
	}
}

// TestStreamRetriesBeforeFirstDelta: a 5xx before any output retries instead of
// failing the turn outright.
func TestStreamRetriesBeforeFirstDelta(t *testing.T) {
	var calls atomic.Int32
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL, MaxRetries: 1}, "m")
	var got strings.Builder
	resp, err := c.Stream(context.Background(), core.Request{}, func(d core.Delta) { got.WriteString(d.Text) })
	if err != nil {
		t.Fatalf("stream retry failed: %v", err)
	}
	if got.String() != "ok" {
		t.Errorf("deltas = %q", got.String())
	}
	if resp.Message.Content[0].Text != "ok" {
		t.Errorf("content = %+v", resp.Message.Content)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

// TestStreamAttemptsSurviveFailure: a retry-exhausted stream still reports how
// many transport attempts it made, so the trace can attribute the failure.
func TestStreamAttemptsSurviveFailure(t *testing.T) {
	var calls atomic.Int32
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL, MaxRetries: 1}, "m")
	resp, err := c.Stream(context.Background(), core.Request{}, nil)
	if err == nil {
		t.Fatal("stream should fail")
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
	if resp.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", resp.Attempts)
	}
}

// TestStreamTTFTForToolCalls: a response with only tool-call deltas still
// records time-to-first-token (no visible text was emitted).
func TestStreamTTFTForToolCalls(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\"," +
				"\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"a\\\"}\"}}]}}]}\n\n" +
				"data: [DONE]\n\n"))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	resp, err := c.Stream(context.Background(), core.Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TTFT <= 0 {
		t.Errorf("ttft = %s, want > 0", resp.TTFT)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.Message.ToolCalls))
	}
}

// TestChatAttemptsSurviveFailure covers the non-streaming retry path.
func TestChatAttemptsSurviveFailure(t *testing.T) {
	var calls atomic.Int32
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL, MaxRetries: 1}, "m")
	resp, err := c.Chat(context.Background(), core.Request{})
	if err == nil {
		t.Fatal("chat should fail")
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
	if resp.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", resp.Attempts)
	}
}

func TestChatParsesToolCalls(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "m",
			"choices": []map[string]any{{
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{{
						"id":   "call1",
						"type": "function",
						"function": map[string]any{
							"name":      "bash",
							"arguments": `{"cmd":"ls"}`,
						},
					}},
				},
			}},
		})
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	resp, err := c.Chat(context.Background(), core.Request{Messages: []core.Message{textMessage(core.RoleUser, "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Name != "bash" || tc.ID != "call1" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Input["cmd"] != "ls" {
		t.Errorf("input = %v", tc.Input)
	}
}

func TestChatRetriesOn429(t *testing.T) {
	var calls int32
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("slow down"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL, MaxRetries: 3}, "m")
	resp, err := c.Chat(context.Background(), core.Request{Messages: []core.Message{textMessage(core.RoleUser, "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content[0].Text != "ok" {
		t.Errorf("content = %q", resp.Message.Content[0].Text)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2 (one retry)", calls)
	}
	if resp.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", resp.Attempts)
	}
}

func TestChatNonRetryableStatus(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad key"))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL, MaxRetries: 3}, "m")
	_, err := c.Chat(context.Background(), core.Request{})
	if err == nil {
		t.Fatal("401 must fail without retry")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestChatMalformedJSON(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	if _, err := c.Chat(context.Background(), core.Request{}); err == nil {
		t.Fatal("malformed JSON must error")
	}
}

func TestChatEmptyChoices(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "m", "choices": []map[string]any{}})
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	resp, err := c.Chat(context.Background(), core.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 0 || len(resp.Message.Content) != 0 {
		t.Errorf("empty choices should yield empty message: %+v", resp.Message)
	}
}

const sseBody = "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"cm\"}}]}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"d\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
	"data: [DONE]\n\n"

// TestStreamParsesUsage: streaming requests must ask for include_usage and
// surface cached/reasoning token details; without the option providers send
// no usage at all and sessions record zero tokens.
func TestStreamParsesUsage(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
			t.Errorf("stream_options.include_usage missing: %+v", req.StreamOptions)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20," +
				"\"prompt_tokens_details\":{\"cached_tokens\":80}," +
				"\"completion_tokens_details\":{\"reasoning_tokens\":7}}}\n\n" +
				"data: [DONE]\n\n"))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	resp, err := c.Stream(context.Background(), core.Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TokensIn != 100 || resp.TokensOut != 20 {
		t.Errorf("tokens = %d/%d", resp.TokensIn, resp.TokensOut)
	}
	if resp.CacheReadTokens != 80 || resp.ReasoningTokens != 7 {
		t.Errorf("usage details = cache %d reasoning %d", resp.CacheReadTokens, resp.ReasoningTokens)
	}
}

func TestStreamAggregatesDeltas(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	var deltas []string
	resp, err := c.Stream(context.Background(), core.Request{Messages: []core.Message{textMessage(core.RoleUser, "x")}},
		func(d core.Delta) { deltas = append(deltas, d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(deltas, ""); got != "Hello" {
		t.Errorf("deltas = %q", got)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Text != "Hello" {
		t.Errorf("assembled text = %+v", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Name != "bash" || tc.Input["cmd"] != "ls" {
		t.Errorf("aggregated tool call = %+v", tc)
	}
}

func TestStreamToleratesJunkLines(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": keepalive\n\ndata: {broken json\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	resp, err := c.Stream(context.Background(), core.Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Text != "ok" {
		t.Errorf("content = %+v", resp.Message.Content)
	}
}

// TestStreamReasoningDeltas: reasoning_content chunks must be forwarded via
// Delta.Reasoning and stored as a "reasoning" part (they used to be dropped,
// leaving the UI silent through long thinking phases).
func TestStreamReasoningDeltas(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"一想\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"答案\"}}]}\n\n" +
				"data: [DONE]\n\n"))
	})
	c := NewOpenAIClient(Provider{BaseURL: srv.URL}, "m")
	var gotReasoning, gotText []string
	resp, err := c.Stream(context.Background(), core.Request{}, func(d core.Delta) {
		if d.Reasoning != "" {
			gotReasoning = append(gotReasoning, d.Reasoning)
		}
		if d.Text != "" {
			gotText = append(gotText, d.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(gotReasoning, ""); got != "想一想" {
		t.Errorf("reasoning deltas = %q", got)
	}
	if got := strings.Join(gotText, ""); got != "答案" {
		t.Errorf("text deltas = %q", got)
	}
	if len(resp.Message.Content) != 2 {
		t.Fatalf("content parts = %d, want reasoning+text", len(resp.Message.Content))
	}
	if resp.Message.Content[0].Type != "reasoning" || resp.Message.Content[0].Text != "想一想" {
		t.Errorf("reasoning part = %+v", resp.Message.Content[0])
	}
	if resp.Message.Content[1].Type != "text" || resp.Message.Content[1].Text != "答案" {
		t.Errorf("text part = %+v", resp.Message.Content[1])
	}
}

// TestToOpenAIMessagesReasoningRoundTrip: reasoning parts must round-trip as
// the reasoning_content field (thinking-mode backends 400 without it) and must
// not leak into the visible content.
func TestToOpenAIMessagesReasoningRoundTrip(t *testing.T) {
	msgs := []core.Message{{
		Role: core.RoleAssistant,
		Content: []core.Part{
			{Type: "reasoning", Text: "想一想"},
			{Type: "text", Text: "答案"},
		},
	}}
	out := toOpenAIMessages(msgs)
	if len(out) != 1 {
		t.Fatalf("msgs = %d", len(out))
	}
	if out[0].ReasoningContent != "想一想" {
		t.Errorf("reasoning_content = %q", out[0].ReasoningContent)
	}
	if out[0].Content != "答案" {
		t.Errorf("content = %q (reasoning leaked?)", out[0].Content)
	}
}

func TestRouterFallbackChain(t *testing.T) {
	var calls int32
	dead := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	live := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "live-model",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "from live"}}},
		})
	})
	clients := map[string]Client{
		"dead": NewOpenAIClient(Provider{Name: "dead", BaseURL: dead.URL, MaxRetries: 0}, "dm"),
		"live": NewOpenAIClient(Provider{Name: "live", BaseURL: live.URL}, "lm"),
	}
	r := NewRouter([]ModelSpec{{Provider: "dead", Model: "dm"}, {Provider: "live", Model: "lm"}}, clients, nil)
	resp, err := r.Chat(context.Background(), core.Request{Messages: []core.Message{textMessage(core.RoleUser, "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content[0].Text != "from live" {
		t.Errorf("content = %q", resp.Message.Content[0].Text)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Error("dead provider should have been tried first")
	}
	if resp.Attempts < 2 {
		t.Errorf("attempts = %d, want at least 2 (dead + live)", resp.Attempts)
	}
}

func TestRouterAllProvidersFail(t *testing.T) {
	dead := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	clients := map[string]Client{
		"a": NewOpenAIClient(Provider{Name: "a", BaseURL: dead.URL, MaxRetries: 0}, "m"),
	}
	r := NewRouter([]ModelSpec{{Provider: "a", Model: "m"}}, clients, nil)
	resp, err := r.Chat(context.Background(), core.Request{})
	if err == nil {
		t.Fatal("all-failing router must error")
	}
	// MaxRetries=0 falls back to the default two retries (three attempts).
	if resp.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", resp.Attempts)
	}
}

func TestRouterNoProviders(t *testing.T) {
	r := NewRouter(nil, map[string]Client{}, nil)
	if _, _, err := r.resolve(); err == nil {
		t.Fatal("empty router must error")
	}
	if _, err := r.Chat(context.Background(), core.Request{}); err == nil {
		t.Fatal("empty router chat must error")
	}
}

func TestRouterStreamingFallbackToChat(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "non-stream"}}},
		})
	})
	clients := map[string]Client{"p": NewOpenAIClient(Provider{Name: "p", BaseURL: srv.URL}, "m")}
	r := NewRouter([]ModelSpec{{Provider: "p", Model: "m", Streaming: false}}, clients, nil)
	resp, err := r.Stream(context.Background(), core.Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content[0].Text != "non-stream" {
		t.Errorf("degraded content = %q", resp.Message.Content[0].Text)
	}
	if r.supportsStreaming() {
		t.Error("non-streaming spec must report unsupported")
	}
}

// TestRouterDegradedStreamReplaysDeltas: when the spec cannot stream, the
// non-streaming answer is still replayed to the UI as deltas.
func TestRouterDegradedStreamReplaysDeltas(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "non-stream"}}},
		})
	})
	clients := map[string]Client{"p": NewOpenAIClient(Provider{Name: "p", BaseURL: srv.URL}, "m")}
	r := NewRouter([]ModelSpec{{Provider: "p", Model: "m", Streaming: false}}, clients, nil)
	var got strings.Builder
	if _, err := r.Stream(context.Background(), core.Request{}, func(d core.Delta) { got.WriteString(d.Text) }); err != nil {
		t.Fatal(err)
	}
	if got.String() != "non-stream" {
		t.Errorf("replayed deltas = %q", got.String())
	}
}

func TestRouterCostAccounting(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "x"}}},
			"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 2000},
		})
	})
	clients := map[string]Client{"p": NewOpenAIClient(Provider{Name: "p", BaseURL: srv.URL}, "m")}
	spec := ModelSpec{Provider: "p", Model: "m", CostPer1KIn: 1.0, CostPer1KOut: 2.0}
	r := NewRouter([]ModelSpec{spec}, clients, nil)
	resp, err := r.Chat(context.Background(), core.Request{})
	if err != nil {
		t.Fatal(err)
	}
	want := 1000.0/1000*1.0 + 2000.0/1000*2.0 // 5.0
	if resp.CostUSD != want {
		t.Errorf("cost = %f, want %f", resp.CostUSD, want)
	}
}

// TestRouterCostCacheDiscount: cached prompt tokens are a subset of the
// prompt size and must bill at the cached rate instead of the full input
// rate, or a cache-heavy run looks many times more expensive than it is.
func TestRouterCostCacheDiscount(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "x"}}},
			"usage": map[string]any{
				"prompt_tokens": 1000, "completion_tokens": 0,
				"prompt_tokens_details": map[string]any{"cached_tokens": 800},
			},
		})
	})
	clients := map[string]Client{"p": NewOpenAIClient(Provider{Name: "p", BaseURL: srv.URL}, "m")}
	spec := ModelSpec{Provider: "p", Model: "m", CostPer1KIn: 1.0, CostPer1KOut: 2.0, CostPer1KCached: 0.1}
	r := NewRouter([]ModelSpec{spec}, clients, nil)
	resp, err := r.Chat(context.Background(), core.Request{})
	if err != nil {
		t.Fatal(err)
	}
	want := 200.0/1000*1.0 + 800.0/1000*0.1 // 0.28
	if diff := resp.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %f, want %f", resp.CostUSD, want)
	}
}

// TestProviderHeaders: configured headers must reach the wire. Gateways that
// demand a session/routing header reject requests without it, and an explicit
// Authorization header overrides the API-key default.
func TestProviderHeaders(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-opencode-session"); got != "sess-1" {
			t.Errorf("session header = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Token abc" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	c := NewOpenAIClient(Provider{
		Name: "p", BaseURL: srv.URL, APIKey: "sk-default",
		Headers: map[string]string{
			"x-opencode-session": "sess-1",
			"Authorization":      "Token abc",
		},
	}, "m")
	if _, err := c.Chat(context.Background(), core.Request{}); err != nil {
		t.Fatal(err)
	}
}

// TestAutoSessionAndUserAgent: every request is attributable to chyan — the
// UA is fixed and the session header carries "chyan-<session id>" (prefix
// only when the call has no session). Configured headers still override.
func TestAutoSessionAndUserAgent(t *testing.T) {
	var uas, sessions []string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		uas = append(uas, r.Header.Get("User-Agent"))
		sessions = append(sessions, r.Header.Get("x-opencode-session"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	c := NewOpenAIClient(Provider{Name: "p", BaseURL: srv.URL}, "m")
	if _, err := c.Chat(context.Background(), core.Request{SessionID: "ses123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), core.Request{}); err != nil {
		t.Fatal(err)
	}
	wantSessions := []string{"chyan-ses123", "chyan"}
	for i := range wantSessions {
		if uas[i] != "chyan agent" {
			t.Errorf("user agent = %q, want chyan agent", uas[i])
		}
		if sessions[i] != wantSessions[i] {
			t.Errorf("session header = %q, want %q", sessions[i], wantSessions[i])
		}
	}

	// A configured header replaces the auto value.
	var overridden string
	srv2 := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		overridden = r.Header.Get("x-opencode-session")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "m",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	c2 := NewOpenAIClient(Provider{Name: "p", BaseURL: srv2.URL,
		Headers: map[string]string{"x-opencode-session": "static-session"}}, "m")
	if _, err := c2.Chat(context.Background(), core.Request{SessionID: "ses123"}); err != nil {
		t.Fatal(err)
	}
	if overridden != "static-session" {
		t.Errorf("override = %q", overridden)
	}
}

func TestRouterWindowResolution(t *testing.T) {
	clients := map[string]Client{}
	r := NewRouter([]ModelSpec{{Provider: "p", Model: "gpt-4o-mini", Window: 0}}, clients, &WindowConfig{})
	if n := r.Window(r.specs[0]); n != 128000 {
		t.Errorf("window = %d, want 128000 from builtin table", n)
	}
	pinned := ModelSpec{Provider: "p", Model: "x", Window: 9999}
	if n := r.Window(pinned); n != 9999 {
		t.Errorf("pinned window = %d", n)
	}
}

func TestRetryable(t *testing.T) {
	if !retryable(&statusError{code: 500}) || !retryable(&statusError{code: 429}) {
		t.Error("5xx/429 should retry")
	}
	if retryable(&statusError{code: 401}) {
		t.Error("401 should not retry")
	}
	if retryable(context.Canceled) {
		t.Error("cancellation should not retry")
	}
	if !retryable(errors.New("connection reset")) {
		t.Error("network errors should retry")
	}
}

// textMessage is a small helper for request payloads in tests.
func textMessage(role core.Role, text string) core.Message {
	return core.Message{Role: role, Content: []core.Part{{Type: "text", Text: text}}}
}

// TestRouterPerSpecModel pins each chain entry to its own model id: the
// fallback request must not reuse the primary's model (forSpec regression).
func TestRouterPerSpecModel(t *testing.T) {
	var gotPrimary, gotFallback string
	dead := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotPrimary = req.Model
		w.WriteHeader(http.StatusInternalServerError)
	})
	live := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotFallback = req.Model
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   req.Model,
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	clients := map[string]Client{
		"dead": NewOpenAIClient(Provider{Name: "dead", BaseURL: dead.URL, MaxRetries: 0}, "model-a"),
		"live": NewOpenAIClient(Provider{Name: "live", BaseURL: live.URL, MaxRetries: 0}, "model-b"),
	}
	r := NewRouter([]ModelSpec{
		{Provider: "dead", Model: "model-a"},
		{Provider: "live", Model: "model-b"},
	}, clients, nil)
	if _, err := r.Chat(context.Background(), core.Request{Messages: []core.Message{textMessage(core.RoleUser, "x")}}); err != nil {
		t.Fatal(err)
	}
	if gotPrimary != "model-a" {
		t.Errorf("primary model = %q, want model-a", gotPrimary)
	}
	if gotFallback != "model-b" {
		t.Errorf("fallback model = %q, want model-b (must not inherit model-a)", gotFallback)
	}
}

// TestRouterFallbackOverridesExplicitModel: an explicit req.Model is honored
// by the primary entry only; a fallback spec must send its own model id, or
// the other provider rejects the unknown model.
func TestRouterFallbackOverridesExplicitModel(t *testing.T) {
	var gotPrimary, gotFallback string
	dead := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotPrimary = req.Model
		w.WriteHeader(http.StatusInternalServerError)
	})
	live := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotFallback = req.Model
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   req.Model,
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	clients := map[string]Client{
		"dead": NewOpenAIClient(Provider{Name: "dead", BaseURL: dead.URL, MaxRetries: 0}, "model-a"),
		"live": NewOpenAIClient(Provider{Name: "live", BaseURL: live.URL, MaxRetries: 0}, "model-b"),
	}
	r := NewRouter([]ModelSpec{
		{Provider: "dead", Model: "model-a"},
		{Provider: "live", Model: "model-b"},
	}, clients, nil)

	req := core.Request{Model: "custom-model", Messages: []core.Message{textMessage(core.RoleUser, "x")}}
	if _, err := r.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if gotPrimary != "custom-model" {
		t.Errorf("primary model = %q, want custom-model", gotPrimary)
	}
	if gotFallback != "model-b" {
		t.Errorf("fallback model = %q, want model-b (explicit pin must not cross providers)", gotFallback)
	}

	// Streaming path applies the same rule.
	gotFallback = ""
	streamRouter := NewRouter([]ModelSpec{
		{Provider: "dead", Model: "model-a", Streaming: true},
		{Provider: "live", Model: "model-b"},
	}, clients, nil)
	if _, err := streamRouter.Stream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if gotFallback != "model-b" {
		t.Errorf("stream fallback model = %q, want model-b", gotFallback)
	}
}

// TestRouterStreamFallsBackWhenNoDelta: a streaming primary failing before
// any delta must fall back to the next spec in the chain.
func TestRouterStreamFallsBackWhenNoDelta(t *testing.T) {
	dead := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	live := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "model-b",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "from live"}}},
		})
	})
	clients := map[string]Client{
		"dead": NewOpenAIClient(Provider{Name: "dead", BaseURL: dead.URL, MaxRetries: 0}, "model-a"),
		"live": NewOpenAIClient(Provider{Name: "live", BaseURL: live.URL, MaxRetries: 0}, "model-b"),
	}
	r := NewRouter([]ModelSpec{
		{Provider: "dead", Model: "model-a", Streaming: true},
		{Provider: "live", Model: "model-b", Streaming: false},
	}, clients, nil)
	resp, err := r.Stream(context.Background(), core.Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content[0].Text != "from live" {
		t.Errorf("content = %q", resp.Message.Content[0].Text)
	}
}

// TestRouterStreamNoFallbackAfterDelta: once a delta reached the user the
// response is already visible, so a later failure must not restart on another
// provider (duplicate output).
func TestRouterStreamNoFallbackAfterDelta(t *testing.T) {
	dead := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Broken SSE: a data line then a connection drop mid-stream.
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	})
	live := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "model-b",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "from live"}}},
		})
	})
	clients := map[string]Client{
		"dead": NewOpenAIClient(Provider{Name: "dead", BaseURL: dead.URL, MaxRetries: 0}, "model-a"),
		"live": NewOpenAIClient(Provider{Name: "live", BaseURL: live.URL, MaxRetries: 0}, "model-b"),
	}
	r := NewRouter([]ModelSpec{
		{Provider: "dead", Model: "model-a", Streaming: true},
		{Provider: "live", Model: "model-b", Streaming: false},
	}, clients, nil)
	var emitted []string
	_, err := r.Stream(context.Background(), core.Request{}, func(d core.Delta) {
		if d.Text != "" {
			emitted = append(emitted, d.Text)
		}
	})
	if err == nil {
		t.Fatal("mid-stream failure after delta must surface, not silently succeed")
	}
	if len(emitted) == 0 {
		t.Error("the dead server's partial delta must have been emitted before the failure")
	}
}

// TestToOpenAIMessagesImageContent: images become content-part arrays with
// base64 data URLs; a missing payload degrades to a text note (never silently
// dropped, FR-10).
func TestToOpenAIMessagesImageContent(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	msgs := []core.Message{{
		Role: core.RoleUser,
		Content: []core.Part{
			{Type: "text", Text: "what is this?"},
			{Type: "image", Image: &core.Image{MIME: "image/png", Data: png, Path: "/tmp/x.png"}},
		},
	}}
	out := toOpenAIMessages(msgs)
	parts, ok := out[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v", out[0].Content)
	}
	textPart, _ := parts[0].(map[string]any)
	if textPart["text"] != "what is this?" {
		t.Errorf("text part = %#v", parts[0])
	}
	imgPart, _ := parts[1].(map[string]any)
	urlPart, _ := imgPart["image_url"].(map[string]any)
	url, _ := urlPart["url"].(string)
	if imgPart["type"] != "image_url" || !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("image part = %#v", imgPart)
	}

	// Missing bytes: keep a visible pointer instead of dropping the part.
	missing := toOpenAIMessages([]core.Message{{
		Role:    core.RoleUser,
		Content: []core.Part{{Type: "image", Image: &core.Image{MIME: "image/png", Path: "/gone.png"}}},
	}})
	parts, _ = missing[0].Content.([]any)
	if len(parts) != 1 {
		t.Fatalf("missing content = %#v", missing[0].Content)
	}
	note, _ := parts[0].(map[string]any)
	if note["type"] != "text" || !strings.Contains(fmt.Sprint(note["text"]), "/gone.png") {
		t.Fatalf("missing note = %#v", note)
	}
}

// TestVisionRejectionHint: a 400 on an image request explains the likely cause
// instead of surfacing a bare provider error (FR-10).
func TestVisionRejectionHint(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid content type: image_url"}}`))
	})
	c := NewOpenAIClient(Provider{Name: "p", BaseURL: srv.URL, MaxRetries: 0}, "m")
	req := core.Request{
		Model: "m",
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.Part{
			{Type: "text", Text: "look"},
			{Type: "image", Image: &core.Image{MIME: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")}},
		}}},
	}
	_, err := c.Chat(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "may not support vision") {
		t.Fatalf("error = %v", err)
	}

	// Text-only failures keep the raw provider error.
	_, err = c.Chat(context.Background(), core.Request{
		Model:    "m",
		Messages: []core.Message{{Role: core.RoleUser, Content: []core.Part{{Type: "text", Text: "hi"}}}},
	})
	if err == nil || strings.Contains(err.Error(), "may not support vision") {
		t.Fatalf("text error = %v", err)
	}
}

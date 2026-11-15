package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"chyan.tech/chyan/internal/core"
)

// userAgent identifies chyan to gateways. Fixed rather than version-stamped:
// the header is for attribution, and a binary upgrade should not change it.
const userAgent = "chyan agent"

// sessionHeader is the gateway header that groups requests into one session.
// chyan always sends it so traffic is attributable even for calls with no
// session (permission review, title generation before a session exists).
const sessionHeader = "x-opencode-session"

// Provider configures one OpenAI-compatible backend endpoint.
type Provider struct {
	Name       string            // routing key
	BaseURL    string            // e.g. https://api.openai.com/v1
	APIKey     string            // already resolved from secret/<name>
	Headers    map[string]string // extra request headers (values already resolved)
	Models     []string
	Timeout    time.Duration
	MaxRetries int
}

// openaiRequest is the chat completions payload (messages + tools).
type openaiRequest struct {
	Model    string       `json:"model"`
	Messages []openaiMsg  `json:"messages"`
	Tools    []openaiTool `json:"tools,omitempty"`
	Stream   bool         `json:"stream,omitempty"`
	// StreamOptions asks the provider to append a usage chunk; without it an
	// OpenAI-compatible stream carries no token counts at all.
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
	// ReasoningEffort caps thinking on reasoning models ("none" disables
	// it); omitted when empty so providers keep their default.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiTool struct {
	Type     string         `json:"type"`
	Function openaiFunction `json:"function"`
}

type openaiFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type openaiMsg struct {
	Role string `json:"role"`
	// Content is a string for text turns and a content-part array when the
	// message carries images (OpenAI-compatible multimodal form).
	Content any `json:"content,omitempty"`
	// ReasoningContent is the thinking-mode trace. DeepSeek-style backends
	// require it echoed back on assistant turns ("the reasoning_content in
	// the thinking mode must be passed back"), so it must round-trip through
	// history instead of being merged into Content.
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiResponse struct {
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			Reasoning string           `json:"reasoning_content"`
			ToolCalls []openaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage openaiUsage `json:"usage"`
	Model string      `json:"model"`
}

// openaiUsage is the token accounting block shared by chat and stream chunks.
// Cached prompt tokens are billed cheaper; reasoning tokens are part of the
// completion but useful to separate from visible output.
type openaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// OpenAIClient is an OpenAI-compatible chat backend. It implements core.LLM
// and, when the endpoint supports SSE streaming, core.Streamer.
type OpenAIClient struct {
	provider Provider
	http     *http.Client
	// streamHTTP serves streaming reads without a whole-request deadline:
	// reasoning models can think for minutes, and http.Client.Timeout would
	// cut the body mid-stream. The wait for response headers stays bounded.
	streamHTTP *http.Client
	model      string // default model when req.Model is empty
}

// NewOpenAIClient builds a client for one provider/model pair.
func NewOpenAIClient(p Provider, defaultModel string) *OpenAIClient {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &OpenAIClient{
		provider: p,
		http:     &http.Client{Timeout: timeout},
		streamHTTP: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: timeout,
		}},
		model: defaultModel,
	}
}

// Name exposes the routing key.
func (c *OpenAIClient) Name() string { return c.provider.Name }

// Chat sends a non-streaming chat completion (core.LLM).
func (c *OpenAIClient) Chat(ctx context.Context, req core.Request) (core.Response, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}
	payload := openaiRequest{
		Model:           model,
		Messages:        toOpenAIMessages(req.Messages),
		Tools:           toOpenAITools(req.Tools),
		ReasoningEffort: req.ReasoningEffort,
	}
	var out openaiResponse
	attempts, err := c.do(ctx, "/chat/completions", payload, &out, req.SessionID)
	if err != nil {
		// Keep the attempt count on failure: the times a call retried before
		// giving up is exactly what a trace needs to explain the failure.
		return core.Response{Attempts: attempts}, visionHint(req, err)
	}
	resp, err := fromOpenAIResponse(out)
	if err != nil {
		return core.Response{}, err
	}
	resp.Attempts = attempts
	return resp, nil
}

// Stream sends a streaming chat completion, emitting deltas as they arrive
// (core.Streamer). Tool-call deltas are aggregated and reported once complete
// in the returned Response. Transient failures retry while nothing has reached
// the caller yet; once a delta is out, retrying would duplicate visible text.
func (c *OpenAIClient) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}
	payload := openaiRequest{
		Model:           model,
		Messages:        toOpenAIMessages(req.Messages),
		Tools:           toOpenAITools(req.Tools),
		Stream:          true,
		StreamOptions:   &openaiStreamOptions{IncludeUsage: true},
		ReasoningEffort: req.ReasoningEffort,
	}
	attempts := c.provider.MaxRetries
	if attempts <= 0 {
		attempts = 2
	}
	var lastErr error
	for i := 0; i <= attempts; i++ {
		emitted := false
		start := time.Now()
		wrap := onDelta
		if onDelta != nil {
			wrap = func(d core.Delta) {
				if d.Text != "" || d.Reasoning != "" {
					emitted = true
				}
				onDelta(d)
			}
		}
		resp, err := c.streamOnce(ctx, payload, req.SessionID, wrap, start)
		if err == nil {
			resp.Attempts = i + 1
			return resp, nil
		}
		lastErr = err
		if emitted || !retryable(err) || i == attempts {
			// Failed attempts still carry their count so the caller can see
			// how many tries the provider burned before this error.
			return core.Response{Attempts: i + 1}, visionHint(req, err)
		}
		select {
		case <-ctx.Done():
			return core.Response{Attempts: i + 1}, ctx.Err()
		case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
		}
	}
	return core.Response{Attempts: attempts + 1}, visionHint(req, lastErr)
}

// hasImages reports whether a request carries multimodal parts.
func hasImages(req core.Request) bool {
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == "image" && p.Image != nil {
				return true
			}
		}
	}
	return false
}

// visionHint turns a bare provider 400 into an actionable message when the
// request carried images: non-vision models reject the content shape, and the
// user needs to know the fix (FR-10).
func visionHint(req core.Request, err error) error {
	if err == nil || !hasImages(req) {
		return err
	}
	var se *statusError
	if errors.As(err, &se) && (se.code == http.StatusBadRequest || se.code == http.StatusUnprocessableEntity) {
		return fmt.Errorf("%w (image input rejected: the model may not support vision; "+
			"set [llm] vision = \"off\" and use OCR/transcription, or pick a vision model)", err)
	}
	return err
}

// streamOnce performs one streaming attempt.
func (c *OpenAIClient) streamOnce(ctx context.Context, payload any, sessionID string, onDelta func(core.Delta), started time.Time) (core.Response, error) {
	resp, err := c.doRawStream(ctx, "/chat/completions", payload, sessionID)
	if err != nil {
		return core.Response{}, err
	}
	defer resp.Body.Close()
	// doRaw passes 429/5xx through for retry; a stream must surface them as
	// errors too, or an empty error body parses as a successful empty reply
	// and the router's fallback chain never engages.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return core.Response{}, &statusError{code: resp.StatusCode, msg: truncate(b, 400)}
	}
	return parseSSE(resp.Body, onDelta, started)
}

// do posts a request and decodes a JSON response, retrying transient failures.
func (c *OpenAIClient) do(ctx context.Context, path string, payload, out any, sessionID string) (int, error) {
	attempts := c.provider.MaxRetries
	if attempts <= 0 {
		attempts = 2
	}
	var lastErr error
	for i := 0; i <= attempts; i++ {
		resp, err := c.doRaw(ctx, path, payload, sessionID)
		if err != nil {
			if !retryable(err) || i == attempts {
				return i + 1, err
			}
			lastErr = err
			select {
			case <-ctx.Done():
				return i + 1, ctx.Err()
			case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
			}
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return i + 1, fmt.Errorf("llm: read response: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("llm: status %d: %s", resp.StatusCode, truncate(body, 200))
			if i == attempts {
				return i + 1, lastErr
			}
			select {
			case <-ctx.Done():
				return i + 1, ctx.Err()
			case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return i + 1, fmt.Errorf("llm: status %d: %s", resp.StatusCode, truncate(body, 400))
		}
		if err := json.Unmarshal(body, out); err != nil {
			return i + 1, fmt.Errorf("llm: decode response: %w", err)
		}
		return i + 1, nil
	}
	return attempts + 1, lastErr
}

// doRaw posts a JSON payload and returns the raw response (bounded total
// timeout; for request/response calls).
func (c *OpenAIClient) doRaw(ctx context.Context, path string, payload any, sessionID string) (*http.Response, error) {
	return c.doRawWith(ctx, c.http, path, payload, sessionID)
}

// doRawStream is doRaw for SSE streams: no whole-request deadline (only the
// header wait is bounded) so long reasoning phases are not cut mid-stream.
func (c *OpenAIClient) doRawStream(ctx context.Context, path string, payload any, sessionID string) (*http.Response, error) {
	return c.doRawWith(ctx, c.streamHTTP, path, payload, sessionID)
}

func (c *OpenAIClient) doRawWith(ctx context.Context, client *http.Client, path string, payload any, sessionID string) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(c.provider.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.provider.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.provider.APIKey)
	}
	req.Header.Set("User-Agent", userAgent)
	// Session attribution: "chyan-<session id>" per conversation, plain
	// "chyan-" prefix when a call has no session (review, early title call).
	session := "chyan"
	if sessionID != "" {
		session = "chyan-" + sessionID
	}
	req.Header.Set(sessionHeader, session)
	// Configured headers win over the defaults: gateways that require a
	// session or routing header cannot be reached otherwise, and an explicit
	// Authorization override must be honored.
	for k, v := range c.provider.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: request: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		resp.Body.Close()
		return nil, &statusError{code: resp.StatusCode, msg: truncate(b, 400)}
	}
	return resp, nil
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("llm: status %d: %s", e.code, e.msg)
}

// retryable reports whether an error is a transient network failure.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// ------------------------------------------------------------- conversions

func toOpenAIMessages(msgs []core.Message) []openaiMsg {
	out := make([]openaiMsg, 0, len(msgs))
	for _, m := range msgs {
		om := openaiMsg{Role: string(m.Role), ToolCallID: m.ToolCallID}
		var text, reasoning strings.Builder
		var images []core.Image
		for _, p := range m.Content {
			if p.Type == "image" && p.Image != nil {
				images = append(images, *p.Image)
				continue
			}
			if p.Text == "" {
				continue
			}
			// Reasoning parts round-trip as reasoning_content; merging them
			// into content both breaks thinking-mode echo-back and pollutes
			// the visible prompt.
			if p.Type == "reasoning" {
				if reasoning.Len() > 0 {
					reasoning.WriteString("\n")
				}
				reasoning.WriteString(p.Text)
				continue
			}
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(p.Text)
		}
		om.Content = text.String()
		if len(images) > 0 {
			om.Content = imageContent(text.String(), images)
		}
		om.ReasoningContent = reasoning.String()
		for _, tc := range m.ToolCalls {
			otc := openaiToolCall{ID: tc.ID, Type: "function"}
			otc.Function.Name = tc.Name
			if raw, err := json.Marshal(tc.Input); err == nil {
				otc.Function.Arguments = string(raw)
			} else {
				otc.Function.Arguments = "{}"
			}
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		out = append(out, om)
	}
	return out
}

// imageContent builds the multimodal content-part array: one text part (when
// present) plus base64 data URLs, so the provider never needs to fetch an
// external URL. A missing payload degrades to a text note instead of being
// silently dropped.
func imageContent(text string, images []core.Image) []any {
	parts := make([]any, 0, len(images)+1)
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, img := range images {
		if len(img.Data) == 0 {
			parts = append(parts, map[string]any{"type": "text",
				"text": "[image unavailable: " + img.Path + "]"})
			continue
		}
		mime := img.MIME
		if mime == "" {
			mime = core.DetectImageMIME(img.Data)
		}
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
		parts = append(parts, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": dataURL},
		})
	}
	return parts
}

func toOpenAITools(tools []core.Tool) []openaiTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openaiTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, openaiTool{
			Type: "function",
			Function: openaiFunction{
				Name:       t.Name(),
				Parameters: t.Schema(),
			},
		})
	}
	return out
}

func fromOpenAIResponse(o openaiResponse) (core.Response, error) {
	resp := core.Response{
		Model:           o.Model,
		TokensIn:        o.Usage.PromptTokens,
		TokensOut:       o.Usage.CompletionTokens,
		CacheReadTokens: o.Usage.PromptTokensDetails.CachedTokens,
		ReasoningTokens: o.Usage.CompletionTokensDetails.ReasoningTokens,
	}
	if len(o.Choices) == 0 {
		return resp, nil
	}
	resp.StopReason = o.Choices[0].FinishReason
	msg := o.Choices[0].Message
	out := core.Message{Role: core.RoleAssistant}
	if msg.Reasoning != "" {
		out.Content = append(out.Content, core.Part{Type: "reasoning", Text: msg.Reasoning})
	}
	if msg.Content != "" {
		out.Content = append(out.Content, core.Part{Type: "text", Text: msg.Content})
	}
	calls, err := toolCallsFrom(msg.ToolCalls)
	if err != nil {
		return core.Response{}, err
	}
	out.ToolCalls = calls
	resp.Message = out
	return resp, nil
}

// toolCallsFrom decodes wire tool calls. Malformed arguments are an error, not
// an empty map: silently running a tool with no arguments hides a broken model
// reply and can execute something destructive.
func toolCallsFrom(in []openaiToolCall) ([]core.ToolCall, error) {
	out := make([]core.ToolCall, 0, len(in))
	for _, tc := range in {
		input := map[string]any{}
		if args := strings.TrimSpace(tc.Function.Arguments); args != "" {
			if err := json.Unmarshal([]byte(args), &input); err != nil {
				return nil, fmt.Errorf("llm: tool call %s: invalid arguments JSON: %w", tc.Function.Name, err)
			}
		}
		out = append(out, core.ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: input})
	}
	return out, nil
}

// parseSSE consumes an SSE stream of chat completion chunks, aggregating
// text deltas and tool-call fragments into one final Response. started is the
// attempt's request start, used to record time-to-first-token; any first chunk
// counts (reasoning, text or tool-call arguments), not just visible text.
func parseSSE(r io.Reader, onDelta func(core.Delta), started time.Time) (core.Response, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var resp core.Response
	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := map[int]*core.ToolCall{}
	order := []int{}
	var model string
	sawFirstChunk := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content string `json:"content"`
					// Reasoning is the thinking stream on reasoning models
					// (OpenAI-compatible "reasoning_content" field).
					Reasoning string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage openaiUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // tolerate non-JSON keepalive/comment lines
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		for _, ch := range chunk.Choices {
			if !sawFirstChunk && (ch.Delta.Content != "" || ch.Delta.Reasoning != "" || len(ch.Delta.ToolCalls) > 0) {
				resp.TTFT = time.Since(started)
				sawFirstChunk = true
			}
			if ch.FinishReason != "" {
				resp.StopReason = ch.FinishReason
			}
			if ch.Delta.Reasoning != "" {
				reasoning.WriteString(ch.Delta.Reasoning)
				if onDelta != nil {
					onDelta(core.Delta{Reasoning: ch.Delta.Reasoning})
				}
			}
			if ch.Delta.Content != "" {
				text.WriteString(ch.Delta.Content)
				if onDelta != nil {
					onDelta(core.Delta{Text: ch.Delta.Content})
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				cur, ok := toolCalls[tc.Index]
				if !ok {
					cur = &core.ToolCall{ID: tc.ID, Input: map[string]any{}}
					toolCalls[tc.Index] = cur
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Name = tc.Function.Name
				}
				// Arguments arrive as incremental JSON string fragments.
				if tc.Function.Arguments != "" {
					if s, ok := cur.Input["__args"].(string); ok {
						cur.Input["__args"] = s + tc.Function.Arguments
					} else {
						cur.Input["__args"] = tc.Function.Arguments
					}
				}
			}
		}
		if chunk.Usage.PromptTokens > 0 {
			resp.TokensIn = chunk.Usage.PromptTokens
			resp.TokensOut = chunk.Usage.CompletionTokens
			resp.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			resp.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return core.Response{}, fmt.Errorf("llm: stream: %w", err)
	}

	final := core.Message{Role: core.RoleAssistant}
	if reasoning.Len() > 0 {
		final.Content = append(final.Content, core.Part{Type: "reasoning", Text: reasoning.String()})
	}
	if text.Len() > 0 {
		final.Content = append(final.Content, core.Part{Type: "text", Text: text.String()})
	}
	for _, idx := range order {
		tc := toolCalls[idx]
		raw, _ := tc.Input["__args"].(string)
		delete(tc.Input, "__args")
		if args := strings.TrimSpace(raw); args != "" {
			// Same rule as the non-stream path: broken arguments fail the
			// call instead of silently executing the tool with no input.
			if err := json.Unmarshal([]byte(args), &tc.Input); err != nil {
				return core.Response{}, fmt.Errorf("llm: tool call %s: invalid arguments JSON: %w", tc.Name, err)
			}
		}
		final.ToolCalls = append(final.ToolCalls, *tc)
	}
	resp.Message = final
	resp.Model = model
	return resp, nil
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubBackend scripts LLM responses in order and records requests.
type stubBackend struct {
	mu        sync.Mutex
	responses []Response
	err       error
	requests  []Request
	callCount int
}

func (s *stubBackend) Chat(_ context.Context, req Request) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	s.callCount++
	if s.err != nil {
		return Response{}, s.err
	}
	if len(s.responses) == 0 {
		return Response{}, errors.New("stub: no scripted response")
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp, nil
}

func (s *stubBackend) Stream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.callCount++
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return Response{}, err
	}
	if len(s.responses) == 0 {
		s.mu.Unlock()
		return Response{}, errors.New("stub: no scripted response")
	}
	resp := s.responses[0]
	s.responses = s.responses[1:]
	s.mu.Unlock()
	if onDelta != nil {
		for _, p := range resp.Message.Content {
			onDelta(Delta{Text: p.Text})
		}
	}
	return resp, nil
}

func textResp(text string) Response {
	return Response{Message: textMessage(RoleAssistant, text), TokensIn: 1, TokensOut: 1}
}

func toolResp(name, id, inputJSON string) Response {
	msg := Message{Role: RoleAssistant}
	msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: id, Name: name, Input: map[string]any{"raw": inputJSON}})
	return Response{Message: msg}
}

// stubTool echoes its input for loop tests.
type stubTool struct {
	name   string
	calls  int
	output string
	err    error
}

func (t *stubTool) Name() string { return t.name }
func (t *stubTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *stubTool) Execute(_ context.Context, _ ToolCall) ToolResult {
	t.calls++
	if t.err != nil {
		return ToolResult{Err: t.err}
	}
	return ToolResult{Content: t.output}
}

func newTestAgent(t *testing.T, backend *stubBackend, tools *ToolRegistry, cfg AgentConfig) (*AgentImpl, *ContextManagerImpl) {
	t.Helper()
	ctxMgr := NewContextManager(ContextConfig{ContextLimit: 1000000},
		"system prompt", nil, ContextDeps{Counter: HeuristicCounter{}, SessionID: "s1"})
	return NewAgent(backend, ctxMgr, tools, nil, "s1", cfg), ctxMgr
}

func TestRunSimpleTurn(t *testing.T) {
	backend := &stubBackend{responses: []Response{textResp("final answer")}}
	agent, ctxMgr := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	msg, err := agent.Run(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if FlattenText(msg) != "final answer" {
		t.Errorf("answer = %q", FlattenText(msg))
	}
	if len(ctxMgr.recent) != 3 { // user + assistant final, plus appended user
		t.Logf("recent = %d", len(ctxMgr.recent))
	}
	// Both user input and assistant reply persisted in context history.
	if len(ctxMgr.recent) < 2 {
		t.Errorf("recent = %d, want >= 2", len(ctxMgr.recent))
	}
}

func TestRunForwardsSessionID(t *testing.T) {
	backend := &stubBackend{responses: []Response{textResp("ok")}}
	agent, _ := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	if _, err := agent.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) == 0 {
		t.Fatal("no request recorded")
	}
	if got := backend.requests[0].SessionID; got != "s1" {
		t.Errorf("session id = %q, want s1", got)
	}
}

func TestToolLoopExecutesAndReturnsFinal(t *testing.T) {
	tool := &stubTool{name: "echo", output: "tool out"}
	tools := NewToolRegistry()
	if err := tools.Register(tool); err != nil {
		t.Fatal(err)
	}
	backend := &stubBackend{responses: []Response{
		toolResp("echo", "c1", "{}"),
		textResp("done"),
	}}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{})
	msg, err := agent.Run(context.Background(), "do it")
	if err != nil {
		t.Fatal(err)
	}
	if FlattenText(msg) != "done" {
		t.Errorf("final = %q", FlattenText(msg))
	}
	if tool.calls != 1 {
		t.Errorf("tool calls = %d, want 1", tool.calls)
	}
	// Tool result must be present in the follow-up request payload.
	if len(backend.requests) < 2 {
		t.Fatalf("requests = %d", len(backend.requests))
	}
	foundToolMsg := false
	for _, m := range backend.requests[1].Messages {
		if m.Role == RoleTool {
			foundToolMsg = true
		}
	}
	if !foundToolMsg {
		t.Error("tool result missing from second request")
	}
}

func TestToolIterationCap(t *testing.T) {
	tool := &stubTool{name: "echo", output: "x"}
	tools := NewToolRegistry()
	_ = tools.Register(tool)
	// Model keeps requesting tools forever.
	var responses []Response
	for i := 0; i < 50; i++ {
		responses = append(responses, toolResp("echo", "c1", "{}"))
	}
	backend := &stubBackend{responses: responses}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{MaxToolIterations: 3})
	_, err := agent.Run(context.Background(), "loop")
	if err == nil || !strings.Contains(err.Error(), "iteration limit") {
		t.Fatalf("err = %v, want iteration limit error", err)
	}
	if tool.calls != 3 {
		t.Errorf("tool calls = %d, want 3 (capped)", tool.calls)
	}
}

// TestRelayContinuesAfterReflexion: hitting the iteration cap with relays
// left runs one tool-less self-critique call, then the loop resumes and can
// still finish the task in the same turn.
func TestRelayContinuesAfterReflexion(t *testing.T) {
	tool := &stubTool{name: "echo", output: "x"}
	tools := NewToolRegistry()
	_ = tools.Register(tool)
	backend := &stubBackend{responses: []Response{
		toolResp("echo", "c1", "{}"),
		toolResp("echo", "c2", "{}"),                          // iteration cap (2) hit here
		textResp("reflecting: files written, next run tests"), // reflexion call
		toolResp("echo", "c3", "{}"),
		textResp("done"), // relayed loop finishes
	}}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{MaxToolIterations: 2, MaxToolRelays: 1})
	var kinds []ProgressKind
	agent.OnProgress(func(ev ProgressEvent) { kinds = append(kinds, ev.Kind) })
	msg, err := agent.Run(context.Background(), "build it")
	if err != nil {
		t.Fatal(err)
	}
	if FlattenText(msg) != "done" {
		t.Errorf("final = %q", FlattenText(msg))
	}
	if tool.calls != 3 {
		t.Errorf("tool calls = %d, want 3 (2 capped + 1 after relay)", tool.calls)
	}
	// The reflexion request (3rd call) must carry no tools and the prompt.
	if len(backend.requests) < 4 {
		t.Fatalf("requests = %d, want >= 4", len(backend.requests))
	}
	reflex := backend.requests[2]
	if len(reflex.Tools) != 0 {
		t.Errorf("reflexion request tools = %d, want 0", len(reflex.Tools))
	}
	found := false
	for _, m := range reflex.Messages {
		if strings.Contains(FlattenText(m), "self-critique") {
			found = true
		}
	}
	if !found {
		t.Error("reflexion prompt missing from reflexion request")
	}
	// The reflection must land in the relayed call's history.
	found = false
	for _, m := range backend.requests[3].Messages {
		if strings.Contains(FlattenText(m), "reflecting:") {
			found = true
		}
	}
	if !found {
		t.Error("reflection missing from relayed request history")
	}
	seen := false
	for _, k := range kinds {
		if k == ProgressReflexion {
			seen = true
		}
	}
	if !seen {
		t.Errorf("kinds = %v, want ProgressReflexion", kinds)
	}
}

// TestRelayExhaustedErrors: once relays are used up the cap still fails the
// turn (the dead-loop guard stays a hard stop).
func TestRelayExhaustedErrors(t *testing.T) {
	tool := &stubTool{name: "echo", output: "x"}
	tools := NewToolRegistry()
	_ = tools.Register(tool)
	backend := &stubBackend{responses: []Response{
		toolResp("echo", "c1", "{}"), // cap (1) hit
		textResp("reflection one"),
		toolResp("echo", "c2", "{}"), // cap again after 1 relay
	}}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{MaxToolIterations: 1, MaxToolRelays: 1})
	_, err := agent.Run(context.Background(), "loop")
	if err == nil || !strings.Contains(err.Error(), "iteration limit") {
		t.Fatalf("err = %v, want iteration limit error", err)
	}
	if !strings.Contains(err.Error(), "1 relay") {
		t.Errorf("err = %v, want relay count in message", err)
	}
	if tool.calls != 2 {
		t.Errorf("tool calls = %d, want 2", tool.calls)
	}
}

// TestRelayDisabledByZero: MaxToolRelays 0 keeps the old fail-at-cap
// behavior without spending a reflexion call.
func TestRelayDisabledByZero(t *testing.T) {
	tool := &stubTool{name: "echo", output: "x"}
	tools := NewToolRegistry()
	_ = tools.Register(tool)
	backend := &stubBackend{responses: []Response{
		toolResp("echo", "c1", "{}"),
		textResp("never reached"),
	}}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{MaxToolIterations: 1, MaxToolRelays: 0})
	_, err := agent.Run(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "iteration limit") {
		t.Fatalf("err = %v, want iteration limit error", err)
	}
	if backend.callCount != 1 {
		t.Errorf("llm calls = %d, want 1 (no reflexion)", backend.callCount)
	}
}

// TestRelayUnlimited: negative MaxToolRelays relays past any fixed count
// until the model finishes (bounded only by interrupt/timeout upstream).
func TestRelayUnlimited(t *testing.T) {
	tool := &stubTool{name: "echo", output: "x"}
	tools := NewToolRegistry()
	_ = tools.Register(tool)
	var responses []Response
	for i := 0; i < 3; i++ {
		responses = append(responses, toolResp("echo", "c1", "{}"))
		responses = append(responses, textResp("reflecting"))
	}
	responses = append(responses, textResp("finally done"))
	backend := &stubBackend{responses: responses}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{MaxToolIterations: 1, MaxToolRelays: -1})
	msg, err := agent.Run(context.Background(), "long task")
	if err != nil {
		t.Fatal(err)
	}
	if FlattenText(msg) != "finally done" {
		t.Errorf("final = %q", FlattenText(msg))
	}
	if tool.calls != 3 {
		t.Errorf("tool calls = %d, want 3 (3 relays)", tool.calls)
	}
}

func TestUnknownTool(t *testing.T) {
	backend := &stubBackend{responses: []Response{
		toolResp("ghost", "c1", "{}"),
		textResp("recovered"),
	}}
	agent, _ := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	msg, err := agent.Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	// The tool error is reported back to the model, which can recover.
	if FlattenText(msg) != "recovered" {
		t.Errorf("final = %q", FlattenText(msg))
	}
}

func TestInterruptStopsRun(t *testing.T) {
	backend := &stubBackend{responses: []Response{
		toolResp("echo", "c1", "{}"),
		textResp("never"),
	}}
	tools := NewToolRegistry()
	blocking := newBlockingTool("echo")
	_ = tools.Register(blocking)
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{})

	done := make(chan error, 1)
	go func() {
		_, err := agent.Run(context.Background(), "x")
		done <- err
	}()
	// Let the tool block, then interrupt.
	blocking.waitUntilStarted()
	agent.Interrupt()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted run must error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after Interrupt")
	}
}

// blockingTool blocks in Execute until told to stop, so Interrupt is testable.
// Channels are created by newBlockingTool; entered is closed when Execute
// actually starts, keeping waitUntilStarted's timing guarantee race-free.
type blockingTool struct {
	name    string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingTool(name string) *blockingTool {
	return &blockingTool{name: name, entered: make(chan struct{}), release: make(chan struct{})}
}

func (t *blockingTool) Name() string { return t.name }
func (t *blockingTool) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *blockingTool) Execute(ctx context.Context, _ ToolCall) ToolResult {
	t.once.Do(func() { close(t.entered) })
	select {
	case <-ctx.Done():
		return ToolResult{Err: ctx.Err()}
	case <-t.release:
		return ToolResult{Content: "released"}
	}
}

func (t *blockingTool) waitUntilStarted() {
	<-t.entered
}

func TestToolOutputTruncation(t *testing.T) {
	tools := NewToolRegistry()
	big := strings.Repeat("A", 30000)
	_ = tools.Register(&stubTool{name: "big", output: big})
	backend := &stubBackend{responses: []Response{
		toolResp("big", "c1", "{}"),
		textResp("ok"),
	}}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{})
	if _, err := agent.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	// The tool result fed to the model must be truncated at the cap.
	found := false
	for _, m := range backend.requests[1].Messages {
		if m.Role == RoleTool {
			found = true
			txt := FlattenText(m)
			if len(txt) > 20200 {
				t.Errorf("tool message length = %d, want <= ~20200", len(txt))
			}
			if !strings.Contains(txt, "[truncated]") {
				t.Error("truncated tool output must carry a marker")
			}
		}
	}
	if !found {
		t.Error("tool message not found")
	}
}

func TestSetToolOutputMaxBytes(t *testing.T) {
	SetToolOutputMaxBytes(100)
	defer SetToolOutputMaxBytes(20000)
	res := truncateResult(ToolResult{Content: strings.Repeat("x", 500)})
	if !res.Truncated || len(res.Content) > 200 {
		t.Errorf("truncated = %v len = %d", res.Truncated, len(res.Content))
	}
	// Already-truncated results pass through untouched.
	pre := ToolResult{Content: "short", Truncated: true}
	if got := truncateResult(pre); got.Content != "short" {
		t.Errorf("passthrough = %q", got.Content)
	}
}

// TestToolsStableOrder: tool order is part of the prompt prefix — an unstable
// map order breaks provider prompt caching on every request (observed as a
// permanently ~2k cache hit against 8k-24k prompts).
func TestToolsStableOrder(t *testing.T) {
	reg := NewToolRegistry()
	for _, name := range []string{"zzz", "aaa", "mmm"} {
		if err := reg.Register(&stubTool{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		tools := reg.Tools()
		if len(tools) != 3 {
			t.Fatalf("tools = %d", len(tools))
		}
		for j := 1; j < len(tools); j++ {
			if tools[j-1].Name() > tools[j].Name() {
				t.Fatalf("run %d: order %s > %s", i, tools[j-1].Name(), tools[j].Name())
			}
		}
	}
}

func TestToolRegistryRejectsDuplicates(t *testing.T) {
	r := NewToolRegistry()
	if err := r.Register(&stubTool{name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&stubTool{name: "a"}); err == nil {
		t.Fatal("duplicate tool name must be rejected")
	}
	if err := r.Register(&stubTool{name: ""}); err == nil {
		t.Fatal("empty tool name must be rejected")
	}
	if _, ok := r.Get("a"); !ok {
		t.Error("registered tool must be retrievable")
	}
	if len(r.Tools()) != 1 {
		t.Errorf("tools = %d", len(r.Tools()))
	}
}

func TestLLMErrorPropagates(t *testing.T) {
	backend := &stubBackend{err: errors.New("llm down")}
	agent, _ := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	if _, err := agent.Run(context.Background(), "x"); err == nil {
		t.Fatal("LLM error must propagate")
	}
}

func TestInterruptedContextYieldsErrInterrupted(t *testing.T) {
	backend := &stubBackend{responses: []Response{textResp("hi")}}
	agent, _ := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := agent.Run(ctx, "x")
	if err == nil {
		t.Fatal("cancelled context must error")
	}
}

func TestStreamingUsedWhenSinkWired(t *testing.T) {
	// stubBackend implements Streamer; with OnDelta set the run streams.
	backend := &stubBackend{responses: []Response{textResp("streamed")}}
	agent, _ := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	var got []string
	agent.OnDelta(func(d Delta) { got = append(got, d.Text) })
	msg, err := agent.Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if FlattenText(msg) != "streamed" {
		t.Errorf("final = %q", FlattenText(msg))
	}
	if strings.Join(got, "") != "streamed" {
		t.Errorf("deltas = %v", got)
	}
}

func TestFallbackToChatWithoutSink(t *testing.T) {
	// No OnDelta: the loop uses Chat even for a streaming-capable backend.
	backend := &stubBackend{responses: []Response{textResp("plain")}}
	agent, _ := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	if _, err := agent.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if backend.callCount != 1 {
		t.Errorf("calls = %d", backend.callCount)
	}
}

// TestProgressEventsCoverToolLifecycle: every run reports thinking plus
// tool_run/tool_done around tool execution, so a UI can show activity during
// silent phases instead of looking stuck.
func TestProgressEventsCoverToolLifecycle(t *testing.T) {
	tool := &stubTool{name: "echo", output: "out"}
	tools := NewToolRegistry()
	if err := tools.Register(tool); err != nil {
		t.Fatal(err)
	}
	backend := &stubBackend{responses: []Response{
		toolResp("echo", "c1", "{}"),
		textResp("done"),
	}}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{})
	var kinds []ProgressKind
	var done *ProgressEvent
	agent.OnProgress(func(ev ProgressEvent) {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == ProgressToolRun && ev.Tool != "echo" {
			t.Errorf("tool_run tool = %q", ev.Tool)
		}
		if ev.Kind == ProgressToolDone && ev.Err != nil {
			t.Errorf("tool_done err = %v", ev.Err)
		}
		if ev.Kind == ProgressDone {
			cp := ev
			done = &cp
		}
	})
	if _, err := agent.Run(context.Background(), "do it"); err != nil {
		t.Fatal(err)
	}
	want := []ProgressKind{ProgressThinking, ProgressToolRun, ProgressToolDone, ProgressThinking, ProgressDone}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kinds[%d] = %s, want %s", i, kinds[i], want[i])
		}
	}
	// The done event must carry the accumulated turn usage (two LLM calls:
	// toolResp has no usage, textResp reports 1/1).
	if done == nil {
		t.Fatal("missing done event")
	}
	if done.TokensIn != 1 || done.TokensOut != 1 {
		t.Errorf("done usage = %d/%d, want 1/1", done.TokensIn, done.TokensOut)
	}
	if done.Elapsed <= 0 {
		t.Errorf("done elapsed = %v, want > 0", done.Elapsed)
	}
}

func TestMarshalAndFlattenHelpers(t *testing.T) {
	msg := textMessage(RoleAssistant, "hello")
	if FlattenText(msg) != "hello" {
		t.Errorf("flatten = %q", FlattenText(msg))
	}
	raw := MarshalMessage(msg)
	if string(raw) == "[]" || string(raw) == "" {
		t.Errorf("marshal = %s", raw)
	}
}

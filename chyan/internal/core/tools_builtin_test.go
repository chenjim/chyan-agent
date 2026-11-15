package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeMemory implements MemoryStore for tool tests.
type fakeMemory struct {
	entries   []MemoryEntry
	saved     []string // names saved via Remember
	savedKind string   // kind of the last Remember
	recallErr error
	saveErr   error
}

func (f *fakeMemory) Remember(_ context.Context, kind, name string, _, _ []string, content string) (string, error) {
	if f.saveErr != nil {
		return "", f.saveErr
	}
	f.saved = append(f.saved, name)
	f.savedKind = kind
	return "m" + name, nil
}

func (f *fakeMemory) Recall(_ context.Context, _, kind string, topK int) ([]MemoryEntry, error) {
	if f.recallErr != nil {
		return nil, f.recallErr
	}
	var out []MemoryEntry
	for _, e := range f.entries {
		if kind == "" || e.Kind == kind {
			out = append(out, e)
		}
	}
	if topK > len(out) {
		topK = len(out)
	}
	return out[:topK], nil
}

// fakeSessions implements SessionReader for read_session tests.
type fakeSessions struct {
	msgs []Message
	err  error
}

func (f *fakeSessions) ReadSession(_ context.Context, _ string, _ int) ([]Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.msgs, nil
}

func TestRememberTool(t *testing.T) {
	mem := &fakeMemory{}
	tool := NewRememberTool(mem)
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{
		"name":     "cut-output",
		"content":  "always truncate tool output",
		"keywords": []any{"truncation", "bloat"},
		"triggers": []any{"tool output 落库"},
	}})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(res.Content, "mcut-output") {
		t.Errorf("content = %q", res.Content)
	}
	if len(mem.saved) != 1 || mem.saved[0] != "cut-output" {
		t.Errorf("saved = %v", mem.saved)
	}
}

func TestRememberToolValidation(t *testing.T) {
	mem := &fakeMemory{}
	tool := NewRememberTool(mem)
	// Missing name.
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"content": "x"}})
	if res.Err == nil {
		t.Error("missing name must error")
	}
	// Missing content.
	res = tool.Execute(context.Background(), ToolCall{Input: map[string]any{"name": "x"}})
	if res.Err == nil {
		t.Error("missing content must error")
	}
}

func TestRememberToolNotConfigured(t *testing.T) {
	tool := NewRememberTool(nil)
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"name": "x", "content": "y"}})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "not configured") {
		t.Errorf("err = %v", res.Err)
	}
}

func TestRememberToolSaveError(t *testing.T) {
	mem := &fakeMemory{saveErr: errors.New("disk full")}
	tool := NewRememberTool(mem)
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"name": "x", "content": "y"}})
	if res.Err == nil {
		t.Error("save error must propagate")
	}
}

func TestRecallTool(t *testing.T) {
	mem := &fakeMemory{entries: []MemoryEntry{
		{Name: "cut-output", Content: "truncate output\nmore detail"},
		{Name: "go-dep", Content: "use go-toml"},
	}}
	tool := NewRecallTool(mem)
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{
		"query": "truncation bloat",
		"top_k": float64(1),
	}})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(res.Content, "cut-output") {
		t.Errorf("content = %q", res.Content)
	}
	// Only first line of each entry is surfaced.
	if strings.Contains(res.Content, "more detail") {
		t.Error("recall must show only the first line")
	}
}

func TestRecallToolNoResults(t *testing.T) {
	mem := &fakeMemory{}
	tool := NewRecallTool(mem)
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"query": "nothing"}})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Content != "no matching memories" {
		t.Errorf("content = %q", res.Content)
	}
}

func TestRecallToolValidation(t *testing.T) {
	mem := &fakeMemory{}
	tool := NewRecallTool(mem)
	if res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{}}); res.Err == nil {
		t.Error("missing query must error")
	}
	tool = NewRecallTool(nil)
	if res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"query": "x"}}); res.Err == nil {
		t.Error("nil memory must error")
	}
}

func TestRecallToolRecallError(t *testing.T) {
	mem := &fakeMemory{recallErr: errors.New("index broken")}
	tool := NewRecallTool(mem)
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"query": "x"}})
	if res.Err == nil {
		t.Error("recall error must propagate")
	}
}

func TestReadSessionTool(t *testing.T) {
	sessions := &fakeSessions{msgs: []Message{
		textMessage(RoleUser, "hello\nworld"),
		textMessage(RoleAssistant, "hi"),
	}}
	tool := NewReadSessionTool(sessions)
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{
		"session_id": "s1",
		"limit":      float64(10),
	}})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(res.Content, "[user]") || !strings.Contains(res.Content, "hello") {
		t.Errorf("content = %q", res.Content)
	}
}

func TestReadSessionToolEmpty(t *testing.T) {
	tool := NewReadSessionTool(&fakeSessions{})
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"session_id": "x"}})
	if res.Content != "session empty or not found" {
		t.Errorf("content = %q", res.Content)
	}
}

func TestReadSessionToolErrors(t *testing.T) {
	tool := NewReadSessionTool(nil)
	if res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"session_id": "x"}}); res.Err == nil {
		t.Error("nil reader must error")
	}
	tool = NewReadSessionTool(&fakeSessions{})
	if res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{}}); res.Err == nil {
		t.Error("missing session_id must error")
	}
	tool = NewReadSessionTool(&fakeSessions{err: errors.New("db down")})
	if res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{"session_id": "x"}}); res.Err == nil {
		t.Error("reader error must propagate")
	}
}

func TestRegisterBuiltins(t *testing.T) {
	r := NewToolRegistry()
	if err := RegisterBuiltins(r, &fakeMemory{}, &fakeSessions{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"remember", "recall", "read_session"} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("built-in %q not registered", name)
		}
	}
	// Re-registration must fail (duplicate names).
	if err := RegisterBuiltins(r, &fakeMemory{}, &fakeSessions{}); err == nil {
		t.Error("duplicate registration must error")
	}
}

func TestStrSlice(t *testing.T) {
	if got := strSlice([]any{"a", "b", "", 42}); len(got) != 2 || got[0] != "a" {
		t.Errorf("strSlice = %v", got)
	}
	if got := strSlice([]string{"a"}); len(got) != 1 {
		t.Errorf("strSlice passthrough = %v", got)
	}
	if got := strSlice(nil); got != nil {
		t.Errorf("strSlice nil = %v", got)
	}
	if got := strSlice("notalist"); got != nil {
		t.Errorf("strSlice scalar = %v", got)
	}
}

func TestToolSchemas(t *testing.T) {
	tools := []Tool{
		NewRememberTool(&fakeMemory{}),
		NewRecallTool(&fakeMemory{}),
		NewReadSessionTool(&fakeSessions{}),
		NewTaskTool(&fakeDispatcher{}),
	}
	for _, tool := range tools {
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Errorf("%s schema type = %v", tool.Name(), schema["type"])
		}
		if tool.Name() == "" {
			t.Error("tool name must not be empty")
		}
	}
}

// fakeDispatcher implements TaskDispatcher for task tool tests.
type fakeDispatcher struct {
	req   TaskRequest
	res   TaskResult
	err   error
	calls int
}

func (f *fakeDispatcher) RunTask(_ context.Context, req TaskRequest) (TaskResult, error) {
	f.calls++
	f.req = req
	return f.res, f.err
}

func TestTaskToolDispatch(t *testing.T) {
	f := &fakeDispatcher{res: TaskResult{SessionID: "child-1", Success: true, Summary: "did it"}}
	tool := NewTaskTool(f)
	if tool.Name() != "task" {
		t.Errorf("name = %q", tool.Name())
	}
	res := tool.Execute(context.Background(), ToolCall{
		Input: map[string]any{"prompt": "go explore", "agent_name": "explorer"},
	})
	if res.Err != nil {
		t.Fatalf("execute: %v", res.Err)
	}
	if f.calls != 1 || f.req.Prompt != "go explore" || f.req.AgentName != "explorer" {
		t.Errorf("dispatch = %+v calls=%d", f.req, f.calls)
	}
	// Only the summary crosses back, with a read_session pointer.
	if !strings.Contains(res.Content, "did it") || !strings.Contains(res.Content, "child-1") {
		t.Errorf("content = %q", res.Content)
	}
}

func TestTaskToolValidation(t *testing.T) {
	f := &fakeDispatcher{res: TaskResult{Success: true, Summary: "ok"}}
	tool := NewTaskTool(f)
	for _, input := range []map[string]any{
		{}, {"prompt": "  "}, {"prompt": 42},
	} {
		if res := tool.Execute(context.Background(), ToolCall{Input: input}); res.Err == nil {
			t.Errorf("input %v must be rejected", input)
		}
	}
	if f.calls != 0 {
		t.Errorf("no dispatch expected, got %d", f.calls)
	}
	// Anonymous dispatch (no agent_name) is allowed.
	if res := tool.Execute(context.Background(), ToolCall{
		Input: map[string]any{"prompt": "x"},
	}); res.Err != nil {
		t.Errorf("anonymous dispatch failed: %v", res.Err)
	}
}

func TestTaskToolNotConfigured(t *testing.T) {
	res := NewTaskTool(nil).Execute(context.Background(), ToolCall{
		Input: map[string]any{"prompt": "x"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "not configured") {
		t.Errorf("err = %v", res.Err)
	}
}

func TestTaskToolDispatchError(t *testing.T) {
	f := &fakeDispatcher{err: ErrNestingTooDeep}
	res := NewTaskTool(f).Execute(context.Background(), ToolCall{
		Input: map[string]any{"prompt": "x"},
	})
	if res.Err == nil {
		t.Error("dispatch error must surface")
	}
}

func TestAgentRunTask(t *testing.T) {
	f := &stubAgentFactory{result: textResp("child answer")}
	runner := NewSubAgentRunner(SubAgentConfig{}, SubAgentDeps{NewAgent: f.newAgent})
	backend := &scriptedBackend{resp: textResp("parent")}
	ctxMgr := NewContextManager(ContextConfig{ContextLimit: 100000},
		"parent", nil, ContextDeps{Counter: HeuristicCounter{}, SessionID: "p1"})
	agent := NewAgent(backend, ctxMgr, NewToolRegistry(), nil, "p1", AgentConfig{})
	agent.AttachSubAgents(runner, AgentContext{SessionID: "p1", Depth: 0})

	res, err := agent.RunTask(context.Background(), TaskRequest{AgentName: "helper", Prompt: "do"})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.Success || res.Summary != "child answer" {
		t.Errorf("result = %+v", res)
	}
	if res.SessionID == "" || res.SessionID == "p1" {
		t.Errorf("result must carry the CHILD session id, got %q", res.SessionID)
	}
	// Unconfigured runner: clear error (Agent contract).
	plain := NewAgent(backend, ctxMgr, NewToolRegistry(), nil, "p2", AgentConfig{})
	if _, err := plain.RunTask(context.Background(), TaskRequest{Prompt: "x"}); err == nil {
		t.Error("RunTask without runner must fail")
	}
}

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

// stubApprover records approvals and returns a fixed verdict.
type stubApprover struct {
	allow  bool
	err    error
	action string
	name   string
}

func (a *stubApprover) Approve(ctx context.Context, req core.ApprovalRequest) (bool, error) {
	a.action, a.name = req.Action, req.Resource
	return a.allow, a.err
}

func stubServer(callText string, callErr bool) *Server {
	s := newServer(ServerConfig{Name: "srv", Type: "remote", URL: "http://x"}, nil)
	s.dial = func(ctx context.Context, cfg ServerConfig, sec Secrets) (*rpcClient, error) {
		return &rpcClient{call: func(ctx context.Context, req request) (json.RawMessage, *rpcError, error) {
			if req.Method == "tools/call" {
				b, _ := json.Marshal(map[string]any{
					"content": []map[string]any{{"type": "text", "text": callText}},
					"isError": callErr,
				})
				return b, nil, nil
			}
			return json.RawMessage(`{"tools":[]}`), nil, nil
		}}, nil
	}
	return s
}

func TestToolName(t *testing.T) {
	if got := ToolName("search", "query"); got != "search_query" {
		t.Errorf("ToolName = %q", got)
	}
}

func TestToolSchema(t *testing.T) {
	mt := mcpTool{Name: "search", Description: "web search", InputSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
	}}
	tl := newTool("srv", mt, stubServer("", false), ToolOptions{})
	if tl.Name() != "srv_search" {
		t.Errorf("Name = %q", tl.Name())
	}
	schema := tl.Schema()
	if schema["description"] != "web search" {
		t.Errorf("description = %v", schema["description"])
	}
	if schema["type"] != "object" {
		t.Errorf("type = %v", schema["type"])
	}
	// A server-supplied description wins over the tool description.
	mt.InputSchema["description"] = "own"
	if got := newTool("srv", mt, stubServer("", false), ToolOptions{}).Schema(); got["description"] != "own" {
		t.Errorf("description override = %v", got["description"])
	}
	// InputSchema mutation must not leak into the adapter copy.
	if _, ok := mt.InputSchema["description"]; !ok {
		t.Error("source schema was mutated")
	}
}

func TestToolExecuteRenderAndTruncate(t *testing.T) {
	// Plain text result passes through.
	tl := newTool("srv", mcpTool{Name: "echo"}, stubServer("hello", false), ToolOptions{})
	res := tl.Execute(context.Background(), core.ToolCall{Input: map[string]any{}})
	if res.Content != "hello" || res.Truncated || res.Err != nil {
		t.Errorf("result = %+v", res)
	}

	// Long output is truncated at the cap with the marker set.
	tl = newTool("srv", mcpTool{Name: "echo"}, stubServer(strings.Repeat("a", 50), false), ToolOptions{MaxOutput: 10})
	res = tl.Execute(context.Background(), core.ToolCall{})
	if !res.Truncated || !strings.HasSuffix(res.Content, "\n…[truncated]") || len(res.Content) > 10+len("\n…[truncated]") {
		t.Errorf("truncated = %+v", res)
	}

	// Server-side tool error (isError) is delivered as content, not Err.
	tl = newTool("srv", mcpTool{Name: "echo"}, stubServer("bad input", true), ToolOptions{})
	res = tl.Execute(context.Background(), core.ToolCall{})
	if res.Err != nil || res.Content != "bad input" {
		t.Errorf("isError result = %+v", res)
	}

	// Transport failure surfaces as Err.
	s := stubServer("x", false)
	s.dial = func(ctx context.Context, cfg ServerConfig, sec Secrets) (*rpcClient, error) {
		return nil, errors.New("dial failed")
	}
	tl = newTool("srv", mcpTool{Name: "echo"}, s, ToolOptions{})
	res = tl.Execute(context.Background(), core.ToolCall{})
	if res.Err == nil {
		t.Errorf("expected error, got %+v", res)
	}
}

func TestToolTruncateRuneBoundary(t *testing.T) {
	// "好" is 3 bytes; a cut at 4 bytes must back off to 3.
	content := "好好"
	res := truncate(content, 4)
	if !res.Truncated || res.Content != "好\n…[truncated]" {
		t.Errorf("truncate = %+v", res)
	}
	if res := truncate("abc", 10); res.Truncated || res.Content != "abc" {
		t.Errorf("short = %+v", res)
	}
}

func TestToolApprovalGate(t *testing.T) {
	// Nil approver passes through.
	tl := newTool("srv", mcpTool{Name: "echo"}, stubServer("ok", false), ToolOptions{})
	if res := tl.Execute(context.Background(), core.ToolCall{}); res.Content != "ok" {
		t.Errorf("nil approver = %+v", res)
	}

	// Denied call is blocked before hitting the server.
	denied := &stubApprover{allow: false}
	tl = newTool("srv", mcpTool{Name: "echo"}, stubServer("ok", false), ToolOptions{Approver: denied})
	res := tl.Execute(context.Background(), core.ToolCall{})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "denied") {
		t.Errorf("denied = %+v", res)
	}
	if denied.action != "mcp" || denied.name != "srv_echo" {
		t.Errorf("approval args = %q %q", denied.action, denied.name)
	}

	// Approver error surfaces as Err.
	broken := &stubApprover{err: errors.New("reviewer down")}
	tl = newTool("srv", mcpTool{Name: "echo"}, stubServer("ok", false), ToolOptions{Approver: broken})
	if res := tl.Execute(context.Background(), core.ToolCall{}); res.Err == nil {
		t.Error("approver error expected")
	}

	// Approved call goes through.
	approved := &stubApprover{allow: true}
	tl = newTool("srv", mcpTool{Name: "echo"}, stubServer("ok", false), ToolOptions{Approver: approved})
	if res := tl.Execute(context.Background(), core.ToolCall{}); res.Content != "ok" {
		t.Errorf("approved = %+v", res)
	}
}

func TestToolDefaultOutputCap(t *testing.T) {
	tl := newTool("srv", mcpTool{Name: "echo"}, stubServer("", false), ToolOptions{})
	if tl.maxOutput != defaultOutputMaxBytes {
		t.Errorf("maxOutput = %d", tl.maxOutput)
	}
}

func TestRenderOutputVariants(t *testing.T) {
	// Multiple text blocks join with newlines.
	var out toolOutput
	out.Content = json.RawMessage(`[{"type":"text","text":"a"},{"type":"image","data":"x"},{"type":"text","text":"b"}]`)
	if got := renderOutput(out); got != "a\nb" {
		t.Errorf("blocks = %q", got)
	}
	// Empty text blocks fall through to structured content.
	out.Content = json.RawMessage(`[{"type":"image","data":"x"}]`)
	out.Structured = json.RawMessage(`{"k":1}`)
	if got := renderOutput(out); got != `{"k":1}` {
		t.Errorf("structured = %q", got)
	}
	// No structured content: raw content is kept.
	out.Structured = nil
	if got := renderOutput(out); got != `[{"type":"image","data":"x"}]` {
		t.Errorf("raw = %q", got)
	}
}

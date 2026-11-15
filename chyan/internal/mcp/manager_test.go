package mcp

import (
	"context"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

func TestManagerEndToEnd(t *testing.T) {
	f := &fakeRemote{tools: []mcpTool{
		{Name: "query", Description: "search", InputSchema: map[string]any{"type": "object"}},
		{Name: "fetch", Description: "fetch", InputSchema: map[string]any{"type": "object"}},
	}, callText: "result"}
	srv := startRemote(t, f)

	cfgs := []ServerConfig{
		{Name: "search", Type: "remote", URL: srv.URL,
			Headers: map[string]string{"Authorization": "secret:search_token"}},
		{Name: "broken", Type: "remote", URL: "http://127.0.0.1:1/mcp"}, // dial failure
		{Name: "off", Type: "remote", URL: srv.URL, Disabled: true},
	}
	m := New(cfgs, &fakeSecrets{values: map[string]string{"search_token": "tok"}}, ToolOptions{MaxOutput: 20000})
	defer m.Close()

	reg := core.NewToolRegistry()
	err := m.Connect(context.Background(), reg)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("Connect error = %v (want broken-server degradation)", err)
	}

	// Tools registered as <server>_<tool>; the broken server degraded and
	// the disabled server is skipped.
	for _, name := range []string{"search_query", "search_fetch"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("tool %s not registered", name)
		}
	}
	if len(reg.Tools()) != 2 {
		t.Errorf("registered tools = %d", len(reg.Tools()))
	}
	if m.Available()["search"] != true || m.Available()["broken"] != false {
		t.Errorf("available = %+v", m.Available())
	}
	// Tool counts are recorded per server at registration time, so the status
	// panel never guesses from name prefixes.
	if counts := m.ToolCounts(); counts["search"] != 2 || counts["broken"] != 0 {
		t.Errorf("tool counts = %+v", counts)
	}

	// The auth header was resolved from the secret store.
	tl, _ := reg.Get("search_query")
	res := tl.Execute(context.Background(), core.ToolCall{Input: map[string]any{"q": "x"}})
	if res.Err != nil || res.Content != "result" {
		t.Errorf("execute = %+v", res)
	}
	if f.lastAuth != "tok" {
		t.Errorf("Authorization header = %q", f.lastAuth)
	}

	m.Close()
	if m.Available()["search"] {
		t.Error("closed server still available")
	}
}

func TestManagerNameConflict(t *testing.T) {
	f := &fakeRemote{tools: []mcpTool{{Name: "tool"}}}
	srv := startRemote(t, f)
	m := New([]ServerConfig{{Name: "s", Type: "remote", URL: srv.URL}}, nil, ToolOptions{})
	defer m.Close()

	reg := core.NewToolRegistry()
	if err := reg.Register(newTool("s", mcpTool{Name: "tool"}, nil, ToolOptions{})); err != nil {
		t.Fatalf("pre-register: %v", err)
	}
	err := m.Connect(context.Background(), reg)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("conflict error = %v", err)
	}
	// A failed registration must not be counted as one of the server's tools.
	if counts := m.ToolCounts(); counts["s"] != 0 {
		t.Errorf("conflict counted as a tool: %+v", counts)
	}
}

func TestManagerAllServersFail(t *testing.T) {
	m := New([]ServerConfig{{Name: "down", Type: "remote", URL: "http://127.0.0.1:1/mcp"}}, nil, ToolOptions{})
	defer m.Close()
	err := m.Connect(context.Background(), core.NewToolRegistry())
	if err == nil {
		t.Error("expected connect error")
	}
}

func TestManagerLocalStdio(t *testing.T) {
	t.Setenv("CHYAN_MCP_HELPER", "1")
	t.Setenv("CHYAN_MCP_HELPER_SLEEP", "")
	m := New([]ServerConfig{{Name: "local", Type: "local", Command: helperCommand()}}, nil, ToolOptions{})
	defer m.Close()

	reg := core.NewToolRegistry()
	if err := m.Connect(context.Background(), reg); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tl, ok := reg.Get("local_echo")
	if !ok {
		t.Fatal("local_echo not registered")
	}
	res := tl.Execute(context.Background(), core.ToolCall{})
	if res.Content != "pong" {
		t.Errorf("execute = %+v", res)
	}
}

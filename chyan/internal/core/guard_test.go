package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepeatedFailingCallIsBlocked: after the limit, an identical invocation
// must not reach the tool again; the model gets a synthetic error instead.
func TestRepeatedFailingCallIsBlocked(t *testing.T) {
	tool := &stubTool{name: "boom", err: errors.New("always fails")}
	tools := NewToolRegistry()
	if err := tools.Register(tool); err != nil {
		t.Fatal(err)
	}
	backend := &stubBackend{responses: []Response{
		toolResp("boom", "1", "x"),
		toolResp("boom", "2", "x"),
		toolResp("boom", "3", "x"),
		toolResp("boom", "4", "x"),
		textResp("done"),
	}}
	agent, ctxMgr := newTestAgent(t, backend, tools, AgentConfig{
		MaxToolIterations:       10,
		MaxRepeatedToolFailures: 2,
	})
	msg, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if FlattenText(msg) != "done" {
		t.Errorf("answer = %q", FlattenText(msg))
	}
	if tool.calls != 2 {
		t.Errorf("tool executed %d times, want 2 (third blocked)", tool.calls)
	}
	found := false
	for _, m := range ctxMgr.recent {
		if strings.Contains(FlattenText(m), "blocked before execution") {
			found = true
		}
	}
	if !found {
		t.Error("blocked call must be reported back to the model")
	}
}

// TestToolGuardDistinguishesArguments: a changed argument is a new attempt,
// not a repetition of the falsified call.
func TestToolGuardDistinguishesArguments(t *testing.T) {
	g := newToolGuard(1)
	a := ToolCall{Name: "bash", Input: map[string]any{"command": "make test"}}
	b := ToolCall{Name: "bash", Input: map[string]any{"command": "make build"}}
	g.fail(a)
	if allowed, _ := g.allow(a); allowed {
		t.Error("same signature must be blocked after one failure")
	}
	if allowed, _ := g.allow(b); !allowed {
		t.Error("different arguments must be allowed")
	}
	g.ok(a)
	if allowed, _ := g.allow(a); !allowed {
		t.Error("a success must clear the failure counter")
	}
}

// TestOversizedOutputIsPreservedAsArtifact: truncation keeps a pointer and a
// hash, and the full original survives on disk.
func TestOversizedOutputIsPreservedAsArtifact(t *testing.T) {
	full := strings.Repeat("evidence-", 3000) // ~27KB > 20KB cap
	tool := &stubTool{name: "dump", output: full}
	tools := NewToolRegistry()
	if err := tools.Register(tool); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	backend := &stubBackend{responses: []Response{toolResp("dump", "1", ""), textResp("ok")}}
	agent, ctxMgr := newTestAgent(t, backend, tools, AgentConfig{ArtifactDir: dir})
	if _, err := agent.Run(context.Background(), "dump it"); err != nil {
		t.Fatal(err)
	}

	var artifact string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".txt") {
			artifact = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact == "" {
		t.Fatal("artifact file not written")
	}
	saved, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != full {
		t.Errorf("artifact content len = %d, want %d", len(saved), len(full))
	}
	toolMsg := ""
	for _, m := range ctxMgr.recent {
		if m.Role == RoleTool {
			toolMsg = FlattenText(m)
		}
	}
	if !strings.Contains(toolMsg, "[artifact: ") || !strings.Contains(toolMsg, "[sha256: ") {
		t.Errorf("tool message missing artifact pointer: %q", toolMsg)
	}
	if len(toolMsg) > ToolOutputMaxBytes()+1024 {
		t.Errorf("tool message length %d exceeds cap", len(toolMsg))
	}
}

// TestExtractEvidence: compaction records raw artifact pointers and marked
// tool errors, paired with the originating tool name.
func TestExtractEvidence(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "1", Content: []Part{{Type: "text", Text: "head\n[artifact: /tmp/a.txt]\n[sha256: abc]"}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "2", Name: "edit"}}},
		{Role: RoleTool, ToolCallID: "2", Content: []Part{{Type: "text", Text: "[tool error] patch context mismatch"}}},
	}
	got := extractEvidence(msgs)
	if len(got) != 2 {
		t.Fatalf("evidence = %v", got)
	}
	if !strings.Contains(got[0], "bash → /tmp/a.txt") {
		t.Errorf("artifact evidence = %q", got[0])
	}
	if !strings.Contains(got[1], "edit failed: patch context mismatch") {
		t.Errorf("error evidence = %q", got[1])
	}
}

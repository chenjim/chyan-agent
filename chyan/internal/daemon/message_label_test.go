package daemon

import (
	"testing"

	"chyan.tech/chyan/internal/core"
)

// TestAPIMessagesLabelsToolCalls: the label is derived from the assistant row
// that issued the call, not from a duplicated copy in the tool message.
func TestAPIMessagesLabelsToolCalls(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
			ID: "c1", Name: "bash", Input: map[string]any{"command": "sqlite3 chyan.db"},
		}}},
		{Role: core.RoleTool, ToolCallID: "c1",
			Content: []core.Part{{Type: "text", Text: "ok"}}},
	}
	out := apiMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("messages = %d", len(out))
	}
	if out[0].Tool != "" {
		t.Fatalf("assistant label = %q, want empty", out[0].Tool)
	}
	if out[1].Tool != "bash · sqlite3 chyan.db" {
		t.Fatalf("tool label = %q", out[1].Tool)
	}

	// Old rows without a matching call fall back to the default summary.
	orphan := apiMessages([]core.Message{{
		Role: core.RoleTool, ToolCallID: "gone",
		Content: []core.Part{{Type: "text", Text: "out"}},
	}})
	if orphan[0].Tool != "" {
		t.Fatalf("orphan label = %q, want empty", orphan[0].Tool)
	}

	// A call without a summarisable argument still shows the tool name.
	named := apiMessages([]core.Message{
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "c2", Name: "read"}}},
		{Role: core.RoleTool, ToolCallID: "c2"},
	})
	if named[1].Tool != "read" {
		t.Fatalf("name-only label = %q", named[1].Tool)
	}
}

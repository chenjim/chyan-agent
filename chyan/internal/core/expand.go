// Chained recall: tool outputs over the prompt-cache-safe cap keep only a
// pointer in context. The expand tool lets the model pull the full evidence
// back on demand, window by window, without re-running the tool.
package core

import (
	"context"
	"fmt"
	"strings"
)

// RefContent is one window of an expanded reference.
type RefContent struct {
	// Text is the resolved window (rune-safe, never larger than limit).
	Text string
	// Next lists follow-up references found inside the window (nested
	// artifacts), so the model can keep walking the chain.
	Next []string
	// Total is the full size in bytes of the resolved reference.
	Total int
}

// RefReader resolves an expandable reference. offset/limit are bytes; limit
// <= 0 means the caller's default. Implementations own path/session safety.
type RefReader interface {
	ExpandRef(ctx context.Context, ref string, offset, limit int) (RefContent, error)
}

// ExpandTool fetches the full body behind a truncated-output pointer
// ("[artifact: <path>]" or a handoff note).
type ExpandTool struct {
	reader RefReader
}

// NewExpandTool wires the reference reader (nil disables with a clear error).
func NewExpandTool(reader RefReader) *ExpandTool {
	return &ExpandTool{reader: reader}
}

// Name implements Tool.
func (t *ExpandTool) Name() string { return "expand" }

// Schema implements Tool.
func (t *ExpandTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ref": map[string]any{"type": "string",
				"description": "reference to expand: artifact:<path> or handoff:<session_id>"},
			"offset": map[string]any{"type": "integer",
				"description": "byte offset to start from (default 0; use the continuation hint for the next window)"},
			"limit": map[string]any{"type": "integer",
				"description": "max bytes to return (default: the tool output cap)"},
		},
		"required": []string{"ref"},
	}
}

// Execute implements Tool.
func (t *ExpandTool) Execute(ctx context.Context, call ToolCall) ToolResult {
	if t.reader == nil {
		return ToolResult{Err: fmt.Errorf("expand: reference reader not configured")}
	}
	ref, _ := call.Input["ref"].(string)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ToolResult{Err: fmt.Errorf("expand: ref is required")}
	}
	offset := intArg(call.Input["offset"])
	limit := intArg(call.Input["limit"])
	res, err := t.reader.ExpandRef(ctx, ref, offset, limit)
	if err != nil {
		return ToolResult{Err: fmt.Errorf("expand: %w", err)}
	}
	var b strings.Builder
	b.WriteString(res.Text)
	if end := offset + len(res.Text); res.Total > end && end > offset {
		fmt.Fprintf(&b, "\n[more available: expand ref=%q offset=%d]", ref, end)
	}
	for i, next := range res.Next {
		if i >= 5 {
			break
		}
		fmt.Fprintf(&b, "\n[next ref: %s]", next)
	}
	return ToolResult{Content: b.String()}
}

// intArg reads a non-negative integer from tool input, tolerating JSON
// numbers decoded as float64 (and int64 from direct callers).
func intArg(v any) int {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	}
	return 0
}

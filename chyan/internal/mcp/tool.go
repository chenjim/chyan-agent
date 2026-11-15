package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"chyan.tech/chyan/internal/core"
)

// defaultOutputMaxBytes is the tool_output_max_bytes default (appendix A).
const defaultOutputMaxBytes = 20000

// Tool adapts one MCP server tool to core.Tool under the registered name
// "<server>_<tool>". It enforces the approval gate (action "mcp") and the
// unified output truncation (ToolResult.Truncated).
type Tool struct {
	name      string // <server>_<tool>
	server    string
	remote    string // raw tool name sent to the server
	desc      string
	schema    map[string]any
	srv       *Server
	approver  core.Approver // nil = pass through (P3 permission gate seam)
	maxOutput int
}

// ToolOptions carries wiring shared by all adapted tools.
type ToolOptions struct {
	Approver  core.Approver // nil passes the approval gate
	MaxOutput int           // tool_output_max_bytes; 0 = 20000
}

// newTool builds the adapter for one catalog entry.
func newTool(server string, mt mcpTool, srv *Server, opts ToolOptions) *Tool {
	maxOut := opts.MaxOutput
	if maxOut <= 0 {
		maxOut = defaultOutputMaxBytes
	}
	return &Tool{
		name:      ToolName(server, mt.Name),
		server:    server,
		remote:    mt.Name,
		desc:      mt.Description,
		schema:    mt.InputSchema,
		srv:       srv,
		approver:  opts.Approver,
		maxOutput: maxOut,
	}
}

// ToolName joins a server name and a remote tool name into the registered
// "<server>_<tool>" name (architecture 4.8).
func ToolName(server, tool string) string { return server + "_" + tool }

// Name implements core.Tool.
func (t *Tool) Name() string { return t.name }

// Schema implements core.Tool. MCP inputSchema is already a JSON Schema
// object; a description prefix keeps the remote tool doc visible to the
// model in the function-calling payload.
func (t *Tool) Schema() map[string]any {
	schema := make(map[string]any, len(t.schema)+1)
	for k, v := range t.schema {
		schema[k] = v
	}
	if t.desc != "" {
		if _, ok := schema["description"]; !ok {
			schema["description"] = t.desc
		}
	}
	return schema
}

// Execute implements core.Tool: approval gate, tools/call, render + truncate.
func (t *Tool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	if t.approver != nil {
		ok, err := t.approver.Approve(ctx, core.ApprovalRequest{
			Action: "mcp", Resource: t.name,
			Detail: "call remote MCP tool " + t.name,
		})
		if err != nil {
			return core.ToolResult{Err: fmt.Errorf("mcp %s: approval: %w", t.name, err)}
		}
		if !ok {
			return core.ToolResult{Err: errors.New("mcp: call denied")}
		}
	}
	out, err := t.srv.callTool(ctx, t.remote, call.Input)
	if err != nil {
		return core.ToolResult{Err: err}
	}
	// A server-side tool error (isError) is data, not a transport error:
	// the payload is returned as-is so the model can recover from it.
	return truncate(renderOutput(out), t.maxOutput)
}

// renderOutput flattens MCP content blocks into plain text; the raw
// payload is kept when the shape is unexpected.
func renderOutput(out toolOutput) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(out.Content) > 0 && json.Unmarshal(out.Content, &blocks) == nil && len(blocks) > 0 {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	// Non-text or empty content: fall back to structured content or raw.
	if len(out.Structured) > 0 {
		return string(out.Structured)
	}
	return string(out.Content)
}

// truncate caps content at max bytes on a rune boundary and marks the cut.
func truncate(content string, max int) core.ToolResult {
	if len(content) <= max {
		return core.ToolResult{Content: content}
	}
	cut := max
	// Do not split a multi-byte rune.
	for cut > 0 && !isUTF8Start(content[cut]) {
		cut--
	}
	return core.ToolResult{
		Content:   content[:cut] + "\n…[truncated]",
		Truncated: true,
	}
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

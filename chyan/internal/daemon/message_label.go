package daemon

import (
	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/core"
)

// toolLabel renders the call behind a role=tool message as a one-line label
// ("bash · sqlite3 chyan.db …") so the collapsed output block says what ran.
// Calls are read from the assistant rows already present in the transcript;
// tool messages persist only ToolCallID, not a second copy of the arguments.
//
// Older rows persisted before this change have no matching call and get an
// empty label; the frontend then falls back to its default title.
func toolLabel(m core.Message, calls map[string]core.ToolCall) string {
	if m.Role != core.RoleTool || m.ToolCallID == "" {
		return ""
	}
	call, ok := calls[m.ToolCallID]
	if !ok || call.Name == "" {
		return ""
	}
	if s := core.SummarizeArgs(call.Input); s != "" {
		return call.Name + " · " + s
	}
	return call.Name
}

// apiMessage converts one stored message for the transcript API, attaching the
// tool label when the row came from a tool call.
func apiMessage(m core.Message, calls map[string]core.ToolCall) api.Message {
	return api.Message{
		Role:      string(m.Role),
		Text:      visibleText(m),
		Tool:      toolLabel(m, calls),
		Images:    imagePaths(m),
		CreatedAt: m.Timestamp,
	}
}

// apiMessages converts a transcript in one pass, building the call-id lookup
// from assistant rows so tool outputs can be labelled without extra storage.
func apiMessages(msgs []core.Message) []api.Message {
	calls := make(map[string]core.ToolCall)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = tc
		}
	}
	out := make([]api.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, apiMessage(m, calls))
	}
	return out
}

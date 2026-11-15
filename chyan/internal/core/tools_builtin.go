package core

import (
	"context"
	"fmt"
	"strings"
)

// MemoryEntry is one long-term memory record as seen by built-in tools. The
// memory package adapts its FileStore to this shape (core stays decoupled).
type MemoryEntry struct {
	ID       string
	Name     string
	Kind     string // fact | kb | model | directive
	Keywords []string
	Triggers []string
	Content  string
	Status   string // "active" | "superseded"
}

// Memory kinds (FR-7): facts, knowledge base, mental models, directives.
// The store persists any slug kind; these are the four vocabulary entries.
const (
	KindFact      = "fact"
	KindKB        = "kb"
	KindModel     = "model"
	KindDirective = "directive"
)

// ValidKind reports whether kind is one of the four long-term memory kinds
// (FR-7). Unknown kinds fall back to fact at the tool layer.
func ValidKind(kind string) bool {
	switch kind {
	case KindFact, KindKB, KindModel, KindDirective:
		return true
	}
	return false
}

// MemoryStore is the long-term memory seam for built-in tools (remember/
// recall). internal/memory adapts its FileStore to this interface. An empty
// kind means "fact"; recall with empty kind searches all kinds.
type MemoryStore interface {
	Remember(ctx context.Context, kind, name string, keywords, triggers []string, content string) (string, error)
	Recall(ctx context.Context, query, kind string, topK int) ([]MemoryEntry, error)
}

// SessionReader reads a session's transcript for read_session (store.Store
// adapts to this; core only needs the narrow shape).
type SessionReader interface {
	ReadSession(ctx context.Context, sessionID string, limit int) ([]Message, error)
}

// RememberTool writes a long-term memory ("记住这个坑"). Lesson entries add
// triggers so the pitfall resurfaces before the same mistake is repeated.
type RememberTool struct {
	mem MemoryStore
}

// NewRememberTool wires the memory store (nil disables with a clear error).
func NewRememberTool(mem MemoryStore) *RememberTool {
	return &RememberTool{mem: mem}
}

// Name implements Tool.
func (t *RememberTool) Name() string { return "remember" }

// Schema implements Tool.
func (t *RememberTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "short memory name"},
			"kind": map[string]any{"type": "string", "enum": []any{KindFact, KindKB, KindModel, KindDirective},
				"description": "memory kind (default fact)"},
			"content":  map[string]any{"type": "string", "description": "the fact or lesson body"},
			"keywords": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"triggers": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "for lessons: scenarios where this must be recalled"},
		},
		"required": []string{"name", "content"},
	}
}

// Execute implements Tool.
func (t *RememberTool) Execute(ctx context.Context, call ToolCall) ToolResult {
	if t.mem == nil {
		return ToolResult{Err: fmt.Errorf("remember: memory store not configured")}
	}
	name, _ := call.Input["name"].(string)
	content, _ := call.Input["content"].(string)
	if name == "" || content == "" {
		return ToolResult{Err: fmt.Errorf("remember: name and content are required")}
	}
	kind, _ := call.Input["kind"].(string)
	if !ValidKind(kind) {
		kind = KindFact
	}
	id, err := t.mem.Remember(ctx, kind, name, strSlice(call.Input["keywords"]), strSlice(call.Input["triggers"]), content)
	if err != nil {
		return ToolResult{Err: fmt.Errorf("remember: %w", err)}
	}
	return ToolResult{Content: fmt.Sprintf("saved %s", id)}
}

// RecallTool queries long-term memory by keywords/context.
type RecallTool struct {
	mem MemoryStore
}

// NewRecallTool wires the memory store.
func NewRecallTool(mem MemoryStore) *RecallTool {
	return &RecallTool{mem: mem}
}

// Name implements Tool.
func (t *RecallTool) Name() string { return "recall" }

// Schema implements Tool.
func (t *RecallTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "keywords or conversation context"},
			"kind": map[string]any{"type": "string", "enum": []any{KindFact, KindKB, KindModel, KindDirective},
				"description": "restrict to one memory kind (default: all kinds)"},
			"top_k": map[string]any{"type": "integer", "description": "max results (default 5)"},
		},
		"required": []string{"query"},
	}
}

// Execute implements Tool.
func (t *RecallTool) Execute(ctx context.Context, call ToolCall) ToolResult {
	if t.mem == nil {
		return ToolResult{Err: fmt.Errorf("recall: memory store not configured")}
	}
	query, _ := call.Input["query"].(string)
	if query == "" {
		return ToolResult{Err: fmt.Errorf("recall: query is required")}
	}
	kind, _ := call.Input["kind"].(string)
	if !ValidKind(kind) {
		kind = "" // all kinds
	}
	topK := 5
	if n, ok := call.Input["top_k"].(float64); ok && n > 0 {
		topK = int(n)
	}
	entries, err := t.mem.Recall(ctx, query, kind, topK)
	if err != nil {
		return ToolResult{Err: fmt.Errorf("recall: %w", err)}
	}
	if len(entries) == 0 {
		return ToolResult{Content: "no matching memories"}
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", e.Kind, e.Name, firstLineOf(e.Content))
	}
	return ToolResult{Content: b.String()}
}

// ReadSessionTool fetches another session's transcript (used by parents to
// inspect a sub-agent's full process, architecture 4.1.1).
type ReadSessionTool struct {
	reader SessionReader
}

// NewReadSessionTool wires the session reader.
func NewReadSessionTool(reader SessionReader) *ReadSessionTool {
	return &ReadSessionTool{reader: reader}
}

// Name implements Tool.
func (t *ReadSessionTool) Name() string { return "read_session" }

// Schema implements Tool.
func (t *ReadSessionTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{"type": "string"},
			"limit":      map[string]any{"type": "integer", "description": "max messages (default 50)"},
		},
		"required": []string{"session_id"},
	}
}

// Execute implements Tool.
func (t *ReadSessionTool) Execute(ctx context.Context, call ToolCall) ToolResult {
	if t.reader == nil {
		return ToolResult{Err: fmt.Errorf("read_session: session reader not configured")}
	}
	id, _ := call.Input["session_id"].(string)
	if id == "" {
		return ToolResult{Err: fmt.Errorf("read_session: session_id is required")}
	}
	limit := 50
	if n, ok := call.Input["limit"].(float64); ok && n > 0 {
		limit = int(n)
	}
	msgs, err := t.reader.ReadSession(ctx, id, limit)
	if err != nil {
		return ToolResult{Err: fmt.Errorf("read_session: %w", err)}
	}
	if len(msgs) == 0 {
		return ToolResult{Content: "session empty or not found"}
	}
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, firstLineOf(FlattenText(m)))
	}
	return ToolResult{Content: b.String()}
}

// TaskDispatcher spawns one child agent for the task tool (the agent loop is
// the natural dispatcher: architecture 4.1.1 task tool exposure).
type TaskDispatcher interface {
	RunTask(ctx context.Context, req TaskRequest) (TaskResult, error)
}

// TaskTool lets the model delegate work to a sub-agent; only the summary
// comes back (context isolation). Permissions are never widened by the
// child (Intersect, perm.go).
type TaskTool struct {
	dispatcher TaskDispatcher
}

// NewTaskTool wires the dispatcher (nil disables with a clear error).
func NewTaskTool(d TaskDispatcher) *TaskTool {
	return &TaskTool{dispatcher: d}
}

// Name implements Tool.
func (t *TaskTool) Name() string { return "task" }

// Schema implements Tool.
func (t *TaskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string", "description": "the task for the sub-agent"},
			"agent_name": map[string]any{"type": "string",
				"description": "optional .agents/agents/<name>.md definition; empty = anonymous"},
		},
		"required": []string{"prompt"},
	}
}

// Execute implements Tool. The tool is read-only-ish (it never mutates the
// parent session) but the child runs the normal approval-gated tool chain.
func (t *TaskTool) Execute(ctx context.Context, call ToolCall) ToolResult {
	if t.dispatcher == nil {
		return ToolResult{Err: fmt.Errorf("task: sub-agent dispatch not configured")}
	}
	prompt, _ := call.Input["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return ToolResult{Err: fmt.Errorf("task: prompt is required")}
	}
	agentName, _ := call.Input["agent_name"].(string)
	res, err := t.dispatcher.RunTask(ctx, TaskRequest{AgentName: agentName, Prompt: prompt})
	if err != nil {
		return ToolResult{Err: fmt.Errorf("task: %w", err)}
	}
	return ToolResult{Content: SummarizeChild(res)}
}

// RegisterBuiltins wires the built-in tools (read_session/remember/recall)
// plus any extras into a registry. Used by the CLI/daemon wiring layer.
func RegisterBuiltins(r *ToolRegistry, mem MemoryStore, reader SessionReader) error {
	for _, t := range []Tool{
		NewRememberTool(mem),
		NewRecallTool(mem),
		NewReadSessionTool(reader),
	} {
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// strSlice normalizes a JSON array of strings from tool input.
func strSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if s, ok := v.([]string); ok {
			return s
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// firstLineOf returns the first line of a multi-line string.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Package api defines the daemon HTTP API contract shared by the daemon
// (server), the TUI/PWA (clients) and tests. Types only — no implementation.
// REST base path: /api/v1. Auth: Authorization: Bearer <token> on the remote
// listener; the local unix-socket listener trusts the OS user.
package api

import (
	"time"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/trace"
)

// Task is an asynchronous background dispatch (kind=dispatch).
type Task struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Status    string    `json:"status"` // pending|running|done|failed|interrupted
	Reply     string    `json:"reply,omitempty"`
	Err       string    `json:"err,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateTaskRequest starts one background agent run.
type CreateTaskRequest struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id,omitempty"` // empty = new session
	Agent     string `json:"agent,omitempty"`      // .agents/agents/<name>.md
	// ForkFrom copies this session into a fresh one before the run.
	ForkFrom string `json:"fork_from,omitempty"`
	// Phase is planner|executor (empty = executor); planner only researches
	// and submits a plan.
	Phase string `json:"phase,omitempty"`
	// Model overrides the run model for this task ("" = config/agent default).
	Model string `json:"model,omitempty"`
	// WorkDir selects the workspace root for this task. Empty keeps the
	// daemon default; continuing a session reuses its recorded workspace.
	WorkDir string `json:"work_dir,omitempty"`
	// Images are data URLs (data:image/png;base64,...), at most 4 per task,
	// each decoded payload <= 5MB (FR-10).
	Images []string `json:"images,omitempty"`
	// Retry replays the session's last user turn instead of appending a new
	// one; the failed attempt's message is already in history.
	Retry bool `json:"retry,omitempty"`
}

// WorkspaceInfo describes the daemon's default workspace and the paths the
// user recently selected (FR-12 W1).
type WorkspaceInfo struct {
	Path   string   `json:"path"`
	Recent []string `json:"recent"`
	// Artifacts is the absolute root for spilled tool output, so the Web can
	// deep-link an [artifact: path] marker into the read-only viewer.
	Artifacts string `json:"artifacts,omitempty"`
}

// SessionSummary is one conversation thread in listings.
type SessionSummary struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"` // sub-agent child link (PWA task tree)
	Agent    string `json:"agent,omitempty"`     // agent definition name ("" = default)
	Title    string `json:"title,omitempty"`
	Model    string `json:"model,omitempty"`
	Status   string `json:"status,omitempty"` // active | interrupted
	// Cumulative usage (sub-agent cost rolls up into the parent session).
	CostUSD         float64   `json:"cost_usd,omitempty"`
	TokensIn        int       `json:"tokens_in,omitempty"`
	TokensOut       int       `json:"tokens_out,omitempty"`
	CacheReadTokens int       `json:"cache_read_tokens,omitempty"`
	ReasoningTokens int       `json:"reasoning_tokens,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ContextUsage is one session's live window usage: how full the conversation
// is *right now*. The cumulative token counters on SessionSummary only grow and
// say nothing about whether the next turn still fits, so the Web header shows
// this instead.
type ContextUsage struct {
	ContextLimit  int `json:"context_limit"`  // resolved model window
	FixedCost     int `json:"fixed_cost"`     // system prompt + tool definitions
	HistoryTokens int `json:"history_tokens"` // conversation history estimate
	HistoryBytes  int `json:"history_bytes"`
	Used          int `json:"used"`       // FixedCost + HistoryTokens
	SoftLimit     int `json:"soft_limit"` // compression trigger line
	HardLimit     int `json:"hard_limit"`
}

// UpdateSessionRequest renames one session.
type UpdateSessionRequest struct {
	Title string `json:"title"`
}

// CreateSessionRequest starts an empty conversation (CLI shell notes and
// /status need an id before the first agent turn).
type CreateSessionRequest struct {
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
}

// HandoffNote is the compacted conversation state (shared with core).
type HandoffNote = core.HandoffNote

// CompactResult is the POST /sessions/{id}/compact confirmation.
type CompactResult struct {
	Note HandoffNote `json:"note"`
}

// AppendNoteRequest records a user-run shell command and its output as a user
// message, without an LLM call (CLI "!" escape).
type AppendNoteRequest struct {
	Command string `json:"command"`
	Output  string `json:"output"`
}

// AppendNoteResult reports which session received the note.
type AppendNoteResult struct {
	SessionID string `json:"session_id"`
}

// CommandInfo is one project slash command for CLI help and completion.
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CronJobRequest is the editable definition of one scheduled job. It carries
// no runtime state (last run/status/session): those are server-owned.
type CronJobRequest struct {
	Name        string   `json:"name"`
	Schedule    string   `json:"schedule"`
	Prompt      string   `json:"prompt"`
	Channel     string   `json:"channel,omitempty"`
	Enabled     bool     `json:"enabled"`
	WorkDir     string   `json:"workdir,omitempty"`
	Script      string   `json:"script,omitempty"`
	NoAgent     bool     `json:"no_agent,omitempty"`
	After       []string `json:"after,omitempty"`
	ContextFrom []string `json:"context_from,omitempty"`
}

// MemoryEntry is one long-term memory row for the Web memory browser.
type MemoryEntry struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Domain      string    `json:"domain"`
	ProjectPath string    `json:"project_path,omitempty"`
	Name        string    `json:"name"`
	Keywords    []string  `json:"keywords,omitempty"`
	Triggers    []string  `json:"triggers,omitempty"`
	Content     string    `json:"content"`
	Status      string    `json:"status"`
	Path        string    `json:"path"`
	CreatedAt   time.Time `json:"created_at"`
}

// Message is one persisted conversation row (part-based content).
type Message struct {
	ID        string   `json:"id"`
	Role      string   `json:"role"`
	Text      string   `json:"text"`
	ToolCalls []string `json:"tool_calls,omitempty"`
	// Tool labels a role=tool message with the call that produced it
	// ("bash · sqlite3 chyan.db …") so collapsed output says what ran.
	Tool string `json:"tool,omitempty"`
	// Images are artifact paths for image parts (absolute, inside the artifact
	// root); the Web fetches them with auth and shows thumbnails in the bubble.
	Images    []string  `json:"images,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Session is a session with its transcript.
type Session struct {
	SessionSummary
	Messages []Message `json:"messages"`
	// Context is the live window usage for this transcript. It rides the
	// session payload so the client does not need a second history read.
	Context *ContextUsage `json:"context,omitempty"`
}

// ApprovalTicket is one ask-effect request awaiting a decision. The diff
// preview shows the pending change so a phone reviewer can decide wisely
// (architecture 4.2).
type ApprovalTicket struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id,omitempty"`
	Action    string    `json:"action"`   // bash|edit|write|mcp|...
	Resource  string    `json:"resource"` // path, command, tool call
	Diff      string    `json:"diff,omitempty"`
	Verdict   string    `json:"verdict,omitempty"` // ai reviewer hint: low|high|uncertain
	CreatedAt time.Time `json:"created_at"`
}

// ApprovalDecision resolves one ticket.
type ApprovalDecision struct {
	Decision string `json:"decision"` // approve|deny
	Reason   string `json:"reason,omitempty"`
}

// SearchHit is one message-level match from the conversation search dialog.
// Results carry the owning session plus a snippet around the matched term so
// the UI can show context without loading every transcript first.
type SearchHit struct {
	SessionID string    `json:"session_id"`
	Title     string    `json:"title"`
	Model     string    `json:"model,omitempty"`
	Role      string    `json:"role"`
	Snippet   string    `json:"snippet"`
	CreatedAt time.Time `json:"created_at"` // message time
	UpdatedAt time.Time `json:"updated_at"` // session time (sort/date badge)
}

// FileNode is one entry in the read-only project file tree.
type FileNode struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

// FileContent is a file preview for the read-only viewer.
type FileContent struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Trunc   bool   `json:"truncated,omitempty"`
}

// FileDiff is a diff between two versions of one file (approval preview).
type FileDiff struct {
	Path string `json:"path"`
	Diff string `json:"diff"` // unified diff
}

// MCPStatus is one configured MCP server's live state, so the TUI/Web can say
// which extension is down instead of silently missing its tools.
type MCPStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Tools     int    `json:"tools"`
}

// Status is daemon-wide observability. Context budget fields reflect the
// most recent session (a per-session breakdown can be added per request).
type Status struct {
	Version          string `json:"version"`
	Sessions         int    `json:"sessions"`
	PendingApprovals int    `json:"pending_approvals"`
	// RunningTasks counts pending + running background dispatches so the Web
	// header can surface global activity from any tab.
	RunningTasks  int    `json:"running_tasks"`
	ReviewMode    string `json:"review_mode"`
	ContextLimit  int    `json:"context_limit"`
	FixedCost     int    `json:"fixed_cost"`
	HardLimit     int    `json:"hard_limit"`
	SoftLimit     int    `json:"soft_limit"`
	HistoryTokens int    `json:"history_tokens"`
	HistoryBytes  int    `json:"history_bytes"`
	// MCPServers is empty when no [mcp.servers.*] table is configured.
	MCPServers []MCPStatus `json:"mcp_servers,omitempty"`
}

// ReloadResult is the confirmation payload of POST /api/v1/config/reload:
// what the daemon re-read after swapping its configuration.
type ReloadResult struct {
	ConfigPath string `json:"config_path"`
	Model      string `json:"model"`
	ReviewMode string `json:"review_mode"`
	PermRules  int    `json:"perm_rules"`
	Skills     int    `json:"skills"`
	Commands   int    `json:"commands"`
	MCPServers int    `json:"mcp_servers"`
	Tools      int    `json:"tools"`
}

// Error is the JSON error envelope.
type Error struct {
	Error string `json:"error"`
}

// along with the daemon's aggregator; the transport shape is the same JSON.
type TraceSpan = trace.Span

// TraceSummary is the per-turn attribution rollup (see trace.Summary).
type TraceSummary = trace.Summary

// TraceTurn is one recorded turn's attribution: the rollup (model/tool/wait/
// memory/subagent/overhead) plus its span rows (FR-13). The Web trace viewer
// reads this shape; `chyan trace` prints the same data as text.
type TraceTurn struct {
	Summary TraceSummary `json:"summary"`
	Spans   []TraceSpan  `json:"spans"`
}

// TraceResponse groups the turns of one session (or one trace/task) newest
// first, so the viewer never has to sort spans client-side.
type TraceResponse struct {
	Turns []TraceTurn `json:"turns"`
}

// SSE event names (event: <name>, data: <json>).
const (
	EventTaskStarted  = "task.started"
	EventTaskOutput   = "task.output"
	EventTaskProgress = "task.progress"
	EventTaskDone     = "task.done"
	// EventCronRun reports one scheduled job finishing (status + session id).
	EventCronRun      = "cron.run"
	EventApprovalNew  = "approval.requested"
	EventApprovalDone = "approval.resolved"
	EventMessage      = "message.received"
	// EventNotice is a daemon-level alert (watchdog self-healing actions).
	EventNotice = "daemon.notice"
	// EventMetric carries one scheduling measurement (queue wait, turn
	// duration, batch size); the PWA may graph it without affecting the turn.
	EventMetric = "daemon.metric"
)

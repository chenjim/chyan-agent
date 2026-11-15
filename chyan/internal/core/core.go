// Package core defines the agent kernel contracts: message model, LLM
// interface, tool execution, context budget and agent orchestration.
package core

import (
	"context"
	"time"
)

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a session. Part-based content aligns with the
// opencode session schema (text / reasoning / tool) for export interop.
type Message struct {
	Role       Role
	Content    []Part
	ToolCalls  []ToolCall
	ToolCallID string // set when Role == RoleTool
	Timestamp  time.Time
}

// Part is a content fragment inside a message.
type Part struct {
	Type  string // "text" | "reasoning" | "tool" | "image"
	Text  string
	Tool  *ToolCall
	Image *Image
}

// Image is one multimodal payload. Path/SHA256/Size persist as the audit
// pointer; Data is the raw bytes used for the current request and is never
// written to the session database (persist the artifact, not the blob).
type Image struct {
	MIME   string `json:"mime,omitempty"`
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int    `json:"size,omitempty"`
	Data   []byte `json:"-"`
}

// ToolImage is an image returned by a tool (e.g. `read` on a png). The agent
// turns it into an image Part and forwards it to the model.
type ToolImage struct {
	MIME string
	Data []byte
}

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// ToolResult is the outcome of one tool execution.
type ToolResult struct {
	Content   string
	Truncated bool
	Err       error
	// Images are multimodal payloads to show the model alongside Content.
	Images []ToolImage
}

// Tool is an executable capability exposed to the model.
type Tool interface {
	Name() string
	// Schema returns the JSON Schema for the tool arguments.
	Schema() map[string]any
	Execute(ctx context.Context, call ToolCall) ToolResult
}

// Request is one LLM call payload.
type Request struct {
	Messages []Message
	Tools    []Tool // nil when the model is called without tools
	Model    string
	// SessionID identifies the chyan session this call belongs to. Adapters
	// forward it as request metadata so gateways can group and attribute
	// traffic per session (e.g. the x-opencode-session header).
	SessionID string
	// ReasoningEffort caps thinking on reasoning models ("none"/"low"/…);
	// empty keeps the provider default. Cheap classifiers (permission
	// review) set "none": one-word verdicts do not pay for thinking.
	ReasoningEffort string
}

// Response is one LLM call outcome.
type Response struct {
	Message         Message
	Model           string
	TokensIn        int
	TokensOut       int
	CacheReadTokens int // prompt tokens served from the provider's cache
	ReasoningTokens int // completion tokens spent thinking (subset of TokensOut)
	CostUSD         float64
	// Attempts counts transport attempts across the provider fallback chain;
	// a slow call that succeeded on try 4 must not look like a slow try 1.
	Attempts int
	// TTFT is time to the first streamed token; zero when non-streaming or
	// unknown (callers must treat zero as "unavailable", not "instant").
	TTFT time.Duration
	// StopReason is the provider finish reason (stop/length/tool_calls/...).
	StopReason string
}

// LLM abstracts a chat completion backend (OpenAI-compatible or native).
type LLM interface {
	Chat(ctx context.Context, req Request) (Response, error)
}

// Delta is one streaming chunk emitted by a backend that supports token
// streaming. Tool-call deltas are aggregated by the provider adapter and
// reported once complete in the final Response.
type Delta struct {
	Text      string // text content chunk
	Reasoning string // reasoning content chunk, when the model exposes it
}

// ProgressKind classifies a run progress notification.
type ProgressKind string

const (
	ProgressThinking  ProgressKind = "thinking"  // about to call the LLM
	ProgressReasoning ProgressKind = "reasoning" // live reasoning token chunk
	ProgressToolRun   ProgressKind = "tool_run"  // tool execution started
	ProgressToolDone  ProgressKind = "tool_done" // tool execution finished
	ProgressReflexion ProgressKind = "reflexion" // iteration cap hit; self-critique before relay
	ProgressDone      ProgressKind = "done"      // run finished; carries turn usage
)

// ProgressEvent reports run activity to a UI sink so long silent phases
// (model thinking, tool execution) stay visible instead of looking stuck.
type ProgressEvent struct {
	Kind            ProgressKind
	Tool            string        // tool name for tool_* events
	Detail          string        // short argument summary for tool_run
	Elapsed         time.Duration // execution time for tool_done / whole run for done
	Err             error         // tool failure, nil on success
	Model           string        // run model (done)
	TokensIn        int           // prompt tokens summed across the turn (done; billing)
	TokensOut       int           // completion tokens across the turn (done)
	CacheReadTokens int           // prompt tokens served from cache (done)
	ReasoningTokens int           // thinking tokens, subset of TokensOut (done)
	CostUSD         float64       // estimated cost across the turn (done)
	ContextTokens   int           // final call's prompt size = current context length (done)
}

// Streamer is optionally implemented by LLM backends that support token
// streaming (a provider-model adapter capability). The agent loop prefers
// Stream when available for live TUI/PWA output and falls back to Chat
// otherwise.
type Streamer interface {
	// Stream calls the LLM, emitting incremental deltas via onDelta and
	// returning the assembled final response.
	Stream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error)
}

// Budget summarizes current context window usage.
type Budget struct {
	ContextLimit  int // resolved window size in tokens
	FixedCost     int // system prompt + tool definitions
	HardLimit     int // history hard cap
	SoftLimit     int // compression trigger line
	HistoryTokens int
	HistoryBytes  int
}

// HandoffNote is the structured handoff generated at compression time
// (manual /compact or near hard limit). It merges and replaces the legacy
// two-tier summary mechanism; Decisions and Corrections are also mirrored
// into project memory (double insurance, see architecture 4.1).
type HandoffNote struct {
	Intent      string   `json:"intent,omitempty"`      // user goal
	Corrections []string `json:"corrections,omitempty"` // user corrections, to avoid repeating mistakes
	Decisions   []string `json:"decisions,omitempty"`   // settled decisions, not to be reopened silently
	Done        []string `json:"done,omitempty"`
	Active      []string `json:"active,omitempty"` // in-flight work; doubles as the internal task list
	Next        []string `json:"next,omitempty"`
	References  []string `json:"references,omitempty"` // file paths / commands / URLs
	// Evidence are original artifacts (paths + hashes) and tool failures from
	// the folded window, extracted deterministically (no LLM) so a conclusion
	// can be traced back to the raw output instead of a summary.
	Evidence    []string  `json:"evidence,omitempty"`
	Lessons     []string  `json:"lessons,omitempty"` // pitfall lessons surfaced this window (triggers format)
	Summary     string    `json:"summary,omitempty"` // compressed conversation summary
	GeneratedAt time.Time `json:"generated_at,omitempty"`
}

// HandoffWriter persists a handoff note as a session attachment (SQLite KV)
// and mirrors Decisions/Corrections into project memory.
type HandoffWriter interface {
	WriteHandoff(ctx context.Context, sessionID string, note HandoffNote) error
}

// MemoryMirror double-writes settled decisions and user corrections into
// long-term memory at compression time (double insurance, architecture 4.1).
// kind is "decision" or "correction". Pitfall lessons are NOT mirrored here:
// lesson persistence is explicit (remember tool) or dream-extracted only, so
// a compress never silently writes to memory. nil disables mirroring.
type MemoryMirror interface {
	Mirror(ctx context.Context, kind, text string) error
}

// ContextManager owns recent history and the compression/handoff policy.
// Compression is window-aware (thresholds derive from the model window
// table) and data never compressed here: SQLite keeps the full truth.
type ContextManager interface {
	// Append adds messages (user/assistant/tool) to recent history without
	// forced trimming. Callers append completed turns and tool-loop
	// iterations at their boundaries.
	Append(msgs ...Message) error
	// BuildMessages assembles the LLM payload. Handoff compression may only
	// trigger here — turn boundary or tool-loop gap — and only when all
	// tool-call/result pairs are complete (never mid-research/mid-tool).
	BuildMessages(userInput string) ([]Message, error)
	// Compact generates a handoff note now (manual /compact or near hard
	// limit), clears recent history and returns the note. Context is
	// append-only before the note; the note replaces the prefix after.
	Compact(ctx context.Context) (HandoffNote, error)
	// Status reports current window usage for observability.
	Status() Budget
	// Reset clears handoff state and recent history.
	Reset()
}

// Compressor produces the Summary section of a handoff note. Implementations
// may call an LLM or fall back to deterministic truncation.
type Compressor interface {
	Compress(ctx context.Context, msgs []Message, targetRatio float64) (string, error)
}

// HandoffExtractor derives structured handoff sections (Intent, Decisions,
// Done, Active, Next, ...) from messages being folded away. Implementations
// may call an LLM; HeuristicExtractor is the deterministic default. The
// Summary section is owned by Compressor, not this interface.
type HandoffExtractor interface {
	Extract(ctx context.Context, msgs []Message) (HandoffNote, error)
}

// Permission effects (three-state model, aligned with opencode).
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectAsk   Effect = "ask"
	EffectDeny  Effect = "deny"
)

// PermissionChecker decides the effect for one action on one resource.
type PermissionChecker interface {
	// Check returns the effect for action (bash/edit/read/web/...) on
	// resource (path, command pattern, ...).
	Check(action, resource string) Effect
	// Intersect returns permissions narrowed by a sub-agent declaration:
	// effective = parent ∩ declared, never widened.
	Intersect(declared map[string]Effect) PermissionChecker
}

// ReviewMode selects who decides ask-effect requests.
type ReviewMode string

const (
	// ReviewHuman routes ask to a human via CLI/TUI/PWA/IM.
	ReviewHuman ReviewMode = "human"
	// ReviewAI (default) lets an AI reviewer approve low-risk asks and
	// escalate uncertain or high-risk ones to a human.
	ReviewAI ReviewMode = "ai"
	// ReviewFull auto-approves everything except explicit deny rules
	// (deny is always a hard block, even in full mode).
	ReviewFull ReviewMode = "full"
)

// ApprovalRequest is one ask-effect request presented to a human approver.
// Diff carries the pending change preview (unified diff for file mutations,
// command text otherwise) so a phone/remote reviewer can decide wisely
// (architecture 4.2, option B).
type ApprovalRequest struct {
	Action   string
	Resource string
	Detail   string // short summary of the tool call input
	Diff     string // change preview: unified diff or command text
	// SessionID ties the ask to its conversation so a client can show the
	// pending card inline instead of only in the global approval list.
	SessionID string
}

// Approver handles ask-effect requests (CLI/TUI/PWA/IM implement this).
type Approver interface {
	// Approve blocks until the user approves or denies the request.
	Approve(ctx context.Context, req ApprovalRequest) (bool, error)
}

// RiskVerdict is an AI reviewer's decision for one ask.
type RiskVerdict string

const (
	RiskLow       RiskVerdict = "low"       // auto-approve
	RiskHigh      RiskVerdict = "high"      // deny
	RiskUncertain RiskVerdict = "uncertain" // escalate to human
)

// RiskReviewer judges whether an ask is safe to auto-approve (ReviewAI
// mode). Implementations may call an LLM or use rule heuristics.
type RiskReviewer interface {
	Review(ctx context.Context, action, resource, detail string) (RiskVerdict, error)
}

// AgentConfig configures one Agent run.
type AgentConfig struct {
	Model             string
	Review            ReviewMode // how ask-effect requests are decided
	SystemPrompt      string
	MaxToolIterations int
	// MaxToolRelays bounds reflexion relays after the tool iteration cap:
	// 0 disables relaying (fail at the cap), N allows up to N relays,
	// negative is unlimited (bounded only by interrupt / sub-agent timeout).
	MaxToolRelays   int
	MaxSubAgents    int
	MaxNestDepth    int // sub-agent nesting cap (<= 2)
	SubAgentTimeout time.Duration
	// MaxRepeatedToolFailures blocks an identical tool call after this many
	// failures inside one user turn: 0 uses the default (3), negative
	// disables the guard.
	MaxRepeatedToolFailures int
	// ArtifactDir stores full tool outputs that exceed the truncation cap
	// (<dir>/<session>/<tool>/<ts>-<sha>.txt). Empty disables artifact
	// persistence and falls back to plain truncation.
	ArtifactDir string
}

// TaskRequest is a sub-agent dispatch payload (the task tool input).
type TaskRequest struct {
	// AgentName is optional. Empty means an anonymous sub-agent using the
	// default model and inherited (not widened) parent permissions.
	// Non-empty loads ~/.agents/agents/<name>.md for system prompt, model
	// and permission narrowing.
	AgentName string
	Prompt    string
}

// TaskResult is the summary returned to the parent agent. Intermediate
// steps stay in the child session and can be fetched via read_session.
type TaskResult struct {
	SessionID string
	Summary   string
	Success   bool
	Err       string
}

// Agent is the main orchestration loop: LLM calls, tool loop, sub-agent
// dispatch, permission gating.
type Agent interface {
	// Run executes one user turn to completion.
	Run(ctx context.Context, userInput string) (Message, error)
	// RunTask spawns a sub-agent session (child of the current session)
	// and returns its result summary.
	RunTask(ctx context.Context, req TaskRequest) (TaskResult, error)
	// Interrupt cancels the in-flight run (including sub-agents).
	Interrupt()
}

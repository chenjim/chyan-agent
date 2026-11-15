package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chyan.tech/chyan/internal/trace"
)

// ErrInterrupted is returned when a run is cancelled via Interrupt.
var ErrInterrupted = errors.New("core: run interrupted")

// ToolRegistry resolves tools by name (built-ins plus MCP adapters).
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewToolRegistry returns an empty registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: map[string]Tool{}}
}

// Register adds a tool; duplicate names are rejected to keep function-calling
// unambiguous (MCP tools use <server>_<tool> to avoid collisions).
func (r *ToolRegistry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Name()
	if name == "" {
		return errors.New("core: tool name is empty")
	}
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("core: tool %q already registered", name)
	}
	r.tools[name] = t
	return nil
}

// Get returns a tool by name.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Tools returns all registered tools in name order for the LLM payload.
// Sorted: tool definition order is part of the prompt prefix, and a random
// map order would break provider prompt caching on every request.
func (r *ToolRegistry) Tools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// LLMBackend is a chat backend with optional streaming (capability
// negotiation lives in llm.Router; core only calls through this).
type LLMBackend interface {
	Chat(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error)
}

// AgentImpl is the main orchestration loop (the Agent interface's engine).
type AgentImpl struct {
	mu sync.Mutex

	backend LLMBackend
	ctxMgr  *ContextManagerImpl
	tools   *ToolRegistry
	store   MessageSink
	session string
	cfg     AgentConfig

	// runner dispatches sub-agents (optional; nil disables RunTask).
	runner *SubAgentRunner
	// parent is this run's effective context inherited by every child.
	parent AgentContext

	// streamDeltas, when non-nil, receives live token deltas (TUI/PWA).
	streamDeltas func(Delta)
	// onProgress, when non-nil, receives run activity events (thinking/tool)
	// so long silent phases stay visible.
	onProgress func(ProgressEvent)
	// tracer records local span metadata (FR-13); nil disables persistence but
	// keeps IDs so callers can still correlate.
	tracer *trace.Recorder

	cancel context.CancelFunc
}

// Compile-time proof that the engine satisfies the Agent contract (RunTask
// was a declared-but-unimplemented gap caught in P4 wiring).
var _ Agent = (*AgentImpl)(nil)

// MessageSink persists conversation turns (store.Store satisfies this shape
// at the wiring layer; core keeps the dependency narrow).
type MessageSink interface {
	AppendMessage(ctx context.Context, msg Message) error
}

// NewAgent builds an agent run engine for one session.
func NewAgent(backend LLMBackend, ctxMgr *ContextManagerImpl, tools *ToolRegistry,
	store MessageSink, session string, cfg AgentConfig) *AgentImpl {
	if cfg.MaxToolIterations <= 0 {
		cfg.MaxToolIterations = 25
	}
	if cfg.MaxSubAgents <= 0 {
		cfg.MaxSubAgents = 4
	}
	if cfg.MaxNestDepth <= 0 {
		cfg.MaxNestDepth = 2
	}
	if cfg.MaxRepeatedToolFailures == 0 {
		cfg.MaxRepeatedToolFailures = 3
	}
	return &AgentImpl{
		backend: backend, ctxMgr: ctxMgr, tools: tools,
		store: store, session: session, cfg: cfg,
	}
}

// OnDelta wires a live-output callback (streaming render).
func (a *AgentImpl) OnDelta(fn func(Delta)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.streamDeltas = fn
}

// OnProgress wires a run-activity callback (thinking/tool visibility).
func (a *AgentImpl) OnProgress(fn func(ProgressEvent)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onProgress = fn
}

// SetTracer wires the span recorder. Nil is valid and disables persistence.
func (a *AgentImpl) SetTracer(rec *trace.Recorder) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tracer = rec
}

// report emits a progress event to the wired sink, if any.
func (a *AgentImpl) report(ev ProgressEvent) {
	a.mu.Lock()
	fn := a.onProgress
	a.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// AttachSubAgents wires the dispatcher and the exact run context children
// must inherit (including per-run settings that differ from process globals).
func (a *AgentImpl) AttachSubAgents(r *SubAgentRunner, parent AgentContext) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runner = r
	a.parent = parent
}

// RunTask spawns one child session (parent = this agent's session) and
// returns only its summary (Agent contract, architecture 4.1.1).
func (a *AgentImpl) RunTask(ctx context.Context, req TaskRequest) (TaskResult, error) {
	a.mu.Lock()
	runner, parent := a.runner, a.parent
	a.mu.Unlock()
	if runner == nil {
		return TaskResult{}, errors.New("core: sub-agent dispatch not configured")
	}
	return runner.Dispatch(ctx, req, parent)
}

// Interrupt cancels the in-flight run (tool loop and LLM calls).
func (a *AgentImpl) Interrupt() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
}

// Run executes one text-only user turn.
func (a *AgentImpl) Run(ctx context.Context, userInput string) (Message, error) {
	return a.RunMessage(ctx, textMessage(RoleUser, userInput))
}

// RunMessage executes one user turn whose input may carry multimodal parts
// (images). The caller has already persisted the input; RunMessage only feeds
// the live context so the current turn sees the same content as replay does.
func (a *AgentImpl) RunMessage(ctx context.Context, input Message) (Message, error) {
	ctx, turn := trace.BeginTurn(ctx, a.tracer, trace.TurnOptions{
		SessionID: a.session, AgentID: a.session,
	})
	msg, err := a.runTurn(ctx, input, turn)
	// Interrupt is a user action, not a failure: classify it as cancelled so
	// trace error rates and error_class stay meaningful.
	traceErr := err
	if errors.Is(err, ErrInterrupted) {
		traceErr = trace.WithClass(err, trace.ClassCancelled)
	}
	if turn != nil {
		turn.SetAttr("model", a.cfg.Model)
		turn.End(traceErr)
	}
	return msg, err
}

// runTurn is RunMessage inside an already-opened turn span. The span stays
// open until the turn returns so a failure is recorded with its class.
func (a *AgentImpl) runTurn(ctx context.Context, input Message, turn *trace.Turn) (Message, error) {
	runCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	defer cancel()

	start := time.Now()
	var usage struct {
		in, out, cacheRead, reasoning, lastIn int
		cost                                  float64
	}
	// done reports whole-turn usage once, so UIs can show model/elapsed/tokens.
	// lastIn (final call's prompt size) is the live context length; in sums all
	// calls and reflects billing.
	defer func() {
		if turn != nil {
			turn.SetAttr("tokens_in", usage.in)
			turn.SetAttr("tokens_out", usage.out)
			turn.SetAttr("cache_read_tokens", usage.cacheRead)
			turn.SetAttr("reasoning_tokens", usage.reasoning)
			turn.SetAttr("cost_usd", usage.cost)
			turn.SetAttr("context_tokens", usage.lastIn)
		}
		a.report(ProgressEvent{Kind: ProgressDone, Elapsed: time.Since(start),
			Model: a.cfg.Model, TokensIn: usage.in, TokensOut: usage.out,
			CacheReadTokens: usage.cacheRead, ReasoningTokens: usage.reasoning,
			CostUSD: usage.cost, ContextTokens: usage.lastIn})
	}()

	if err := a.ctxMgr.Append(input); err != nil {
		return Message{}, err
	}

	// The guard is scoped to one user turn: a later turn may legitimately
	// retry after the environment changed, but within a turn an identical
	// failing call is a loop, not progress.
	guard := newToolGuard(a.cfg.MaxRepeatedToolFailures)

	relays := 0
	for {
		for i := 0; i < a.cfg.MaxToolIterations; i++ {
			select {
			case <-runCtx.Done():
				return Message{}, ErrInterrupted
			default:
			}

			payload, err := a.ctxMgr.BuildMessages("")
			if err != nil {
				return Message{}, err
			}
			a.report(ProgressEvent{Kind: ProgressThinking})
			resp, err := a.callLLM(runCtx, payload)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return Message{}, ErrInterrupted
				}
				return Message{}, err
			}
			usage.in += resp.TokensIn
			usage.out += resp.TokensOut
			usage.cacheRead += resp.CacheReadTokens
			usage.reasoning += resp.ReasoningTokens
			usage.cost += resp.CostUSD
			usage.lastIn = resp.TokensIn // latest call's prompt = live context length

			// The model's turn is part of history before tools run so the
			// tool-call/result pairs stay adjacent in the window.
			if err := a.ctxMgr.Append(resp.Message); err != nil {
				return Message{}, err
			}
			a.persist(runCtx, resp.Message)

			if len(resp.Message.ToolCalls) == 0 {
				return resp.Message, nil
			}

			var imageParts []Part
			for _, call := range resp.Message.ToolCalls {
				a.report(ProgressEvent{Kind: ProgressToolRun, Tool: call.Name, Detail: SummarizeArgs(call.Input)})
				toolStart := time.Now()
				result := a.executeToolGuarded(runCtx, call, guard)
				a.report(ProgressEvent{Kind: ProgressToolDone, Tool: call.Name, Elapsed: time.Since(toolStart), Err: result.Err})
				// ToolCallID pairs the output with its call. The transcript
				// label is derived from that assistant row on read, so the
				// arguments are not copied into this message a second time.
				msg := Message{Role: RoleTool, ToolCallID: call.ID,
					Content: []Part{{Type: "text", Text: result.Content}}, Timestamp: nowUTC()}
				if result.Err != nil {
					// The marker lets compaction extract tool failures into
					// the handoff Evidence section without an LLM.
					msg.Content = append(msg.Content, Part{Type: "text", Text: "[tool error] " + result.Err.Error()})
				}
				if err := a.ctxMgr.Append(msg); err != nil {
					return Message{}, err
				}
				a.persist(runCtx, msg)
				// An unanswered approval is fatal: deny the operation and stop
				// the whole turn instead of letting the model improvise.
				if errors.Is(result.Err, ErrApprovalTimeout) {
					return Message{}, ErrApprovalTimeout
				}
				for _, im := range result.Images {
					part, err := SaveImage(a.cfg.ArtifactDir, a.session, im.MIME, im.Data)
					if err != nil {
						imageParts = append(imageParts, Part{Type: "text",
							Text: "[image dropped: " + err.Error() + "]"})
						continue
					}
					imageParts = append(imageParts, part)
				}
			}
			// Tool images ride in a user message after all tool results: the
			// chat-completions contract only allows text in role=tool.
			if len(imageParts) > 0 {
				imgMsg := Message{Role: RoleUser, Timestamp: nowUTC(),
					Content: append([]Part{{Type: "text", Text: "[image output from tools]"}}, imageParts...)}
				if err := a.ctxMgr.Append(imgMsg); err != nil {
					return Message{}, err
				}
				a.persist(runCtx, imgMsg)
			}
		}

		// Iteration cap hit with the model still mid-task: self-critique and
		// relay instead of failing the turn (max_tool_relays bounds the total).
		if a.cfg.MaxToolRelays == 0 || (a.cfg.MaxToolRelays > 0 && relays >= a.cfg.MaxToolRelays) {
			return Message{}, fmt.Errorf("core: tool iteration limit (%d) reached after %d relay(s)",
				a.cfg.MaxToolIterations, relays)
		}
		relays++
		resp, err := a.reflexionTurn(runCtx, relays)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return Message{}, ErrInterrupted
			}
			return Message{}, err
		}
		usage.in += resp.TokensIn
		usage.out += resp.TokensOut
		usage.cacheRead += resp.CacheReadTokens
		usage.reasoning += resp.ReasoningTokens
		usage.cost += resp.CostUSD
		usage.lastIn = resp.TokensIn
	}
}

// reflexionPrompt is the self-critique nudge sent when the tool iteration cap
// is hit: review progress, spot futile repetition, then commit to one next
// step before the relayed loop continues (Reflexion-style).
const reflexionPrompt = "You have reached the tool iteration limit for this turn without finishing. " +
	"Pause and self-critique before continuing:\n" +
	"1. What has been accomplished so far?\n" +
	"2. What remains to reach the goal?\n" +
	"3. Are any recent tool calls repeating without progress? If so, change the approach instead of repeating them.\n" +
	"Reply with a brief reflection and the single most important next step. Do not call tools."

// reflexionTurn runs one tool-less self-critique call and folds both the
// nudge and the reflection into history so the relayed loop resumes from an
// explicit progress review. The reflection reaches the UI as a progress
// detail, never as part of the final answer stream (CLI scripts must see
// only the answer).
func (a *AgentImpl) reflexionTurn(ctx context.Context, relay int) (Response, error) {
	prompt := textMessage(RoleUser, reflexionPrompt)
	if err := a.ctxMgr.Append(prompt); err != nil {
		return Response{}, err
	}
	a.persist(ctx, prompt)
	payload, err := a.ctxMgr.BuildMessages("")
	if err != nil {
		return Response{}, err
	}
	req := Request{Messages: payload, Model: a.cfg.Model, SessionID: a.session} // no tools: pure reflection
	ctx, span := trace.Start(ctx, a.tracer, trace.KindLLM, "reflexion", map[string]any{"model": req.Model})
	resp, err := a.backend.Chat(ctx, req)
	if span != nil {
		span.SetAttr("attempts", resp.Attempts)
		span.SetAttr("tokens_in", resp.TokensIn)
		span.SetAttr("tokens_out", resp.TokensOut)
		span.SetAttr("stop_reason", resp.StopReason)
		span.End(err)
	}
	if err != nil {
		return Response{}, err
	}
	if err := a.ctxMgr.Append(resp.Message); err != nil {
		return Response{}, err
	}
	a.persist(ctx, resp.Message)
	a.report(ProgressEvent{Kind: ProgressReflexion,
		Detail: fmt.Sprintf("relay %d: %s", relay, clampDetail(FlattenText(resp.Message)))})
	return resp, nil
}

// clampDetail bounds a progress detail line (UI note, not an answer).
func clampDetail(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// callLLM prefers streaming when the backend implements Streamer and a delta
// sink is wired (live output); otherwise it falls back to Chat.
func (a *AgentImpl) callLLM(ctx context.Context, payload []Message) (resp Response, err error) {
	req := Request{Messages: payload, Tools: a.tools.Tools(), Model: a.cfg.Model, SessionID: a.session}
	ctx, span := trace.Start(ctx, a.tracer, trace.KindLLM, "chat", map[string]any{"model": req.Model})
	defer func() {
		if span == nil {
			return
		}
		span.SetAttr("model", firstNonEmpty(resp.Model, req.Model))
		span.SetAttr("attempts", resp.Attempts)
		span.SetAttr("tokens_in", resp.TokensIn)
		span.SetAttr("tokens_out", resp.TokensOut)
		span.SetAttr("cache_read_tokens", resp.CacheReadTokens)
		span.SetAttr("reasoning_tokens", resp.ReasoningTokens)
		span.SetAttr("cost_usd", resp.CostUSD)
		span.SetAttr("stop_reason", resp.StopReason)
		if resp.TTFT > 0 {
			span.SetAttr("ttft_ms", resp.TTFT.Milliseconds())
		} else {
			span.SetAttr("ttft_ms", "unavailable")
		}
		span.SetAttr("tools", len(req.Tools))
		span.End(err)
	}()
	a.mu.Lock()
	sink := a.streamDeltas
	a.mu.Unlock()
	if sink != nil {
		if streamer, ok := a.backend.(Streamer); ok {
			// Reasoning chunks surface as progress so long silent thinking
			// phases stay visible even when only text reaches the UI sink.
			wrap := func(d Delta) {
				if d.Reasoning != "" {
					a.report(ProgressEvent{Kind: ProgressReasoning, Detail: d.Reasoning})
				}
				sink(d)
			}
			return streamer.Stream(ctx, req, wrap)
		}
	}
	return a.backend.Chat(ctx, req)
}

// executeToolGuarded runs one tool call unless the same invocation already
// failed MaxRepeatedToolFailures times in this turn. The block is a synthetic
// tool error: the model sees why it was stopped and must change approach.
func (a *AgentImpl) executeToolGuarded(ctx context.Context, call ToolCall, guard *toolGuard) ToolResult {
	if allowed, n := guard.allow(call); !allowed {
		label := call.Name
		if detail := SummarizeArgs(call.Input); detail != "" {
			label += " " + detail
		}
		err := fmt.Errorf("repeated failing call blocked (%s failed %d times this turn); "+
			"do not retry it, change the approach or the arguments", label, n)
		// The block happens before execution: record it so a run that burns
		// iterations on a repeated call is explainable from the trace too.
		_, span := trace.Start(ctx, a.tracer, trace.KindToolExecution, call.Name,
			map[string]any{"tool": call.Name, "call_id": call.ID, "blocked": true})
		if span != nil {
			span.SetAttr("repeat_failures", n)
			span.End(trace.WithClass(err, trace.ClassInvalidArgument))
		}
		return ToolResult{
			Content: "blocked before execution: identical invocation already failed",
			Err:     trace.WithClass(err, trace.ClassInvalidArgument),
		}
	}
	res := a.executeTool(ctx, call)
	if res.Err != nil {
		guard.fail(call)
	} else {
		guard.ok(call)
	}
	return res
}

// executeTool runs one tool call with output truncation (the single place
// where tool output is capped: architecture 4.1 anti-bloat) and preserves
// the full original as an artifact when the cap would discard evidence.
func (a *AgentImpl) executeTool(ctx context.Context, call ToolCall) (res ToolResult) {
	ctx, span := trace.Start(ctx, a.tracer, trace.KindToolExecution, call.Name, map[string]any{
		"tool": call.Name, "call_id": call.ID, "args": SummarizeArgs(call.Input),
	})
	defer func() {
		if span == nil {
			return
		}
		span.SetAttr("result_bytes", len(res.Content))
		span.SetAttr("truncated", res.Truncated)
		span.End(res.Err)
	}()
	tool, ok := a.tools.Get(call.Name)
	if !ok {
		return ToolResult{Err: trace.WithClass(fmt.Errorf("unknown tool %q", call.Name), trace.ClassInvalidArgument)}
	}
	res = tool.Execute(ctx, call)
	return a.preserveToolOutput(call, res)
}

// preserveToolOutput writes oversized output to ArtifactDir before truncating
// the model-facing copy, leaving a path + sha256 pointer in the message.
func (a *AgentImpl) preserveToolOutput(call ToolCall, res ToolResult) ToolResult {
	if res.Truncated {
		return res
	}
	max := ToolOutputMaxBytes()
	if len(res.Content) <= max {
		return res
	}
	if a.cfg.ArtifactDir != "" {
		path, digest, err := saveArtifact(a.cfg.ArtifactDir, a.session, call.Name, res.Content)
		if err == nil {
			omitted := len(res.Content) - max
			res.Content = res.Content[:max] + fmt.Sprintf(
				"\n…[truncated %d bytes]…\n[artifact: %s]\n[sha256: %s]\n"+
					"[full output persisted; read the artifact file when exact details are needed]",
				omitted, path, digest)
			res.Truncated = true
			return res
		}
		log.Printf("core: artifact save failed, falling back to truncation: %v", err)
	}
	return truncateResult(res)
}

// truncateResult caps tool output at tool_output_max_bytes, marking the cut.
func truncateResult(res ToolResult) ToolResult {
	if res.Truncated {
		return res
	}
	max := int(toolOutputMaxBytes.Load())
	if len(res.Content) > max {
		res.Content = res.Content[:max] + "\n…[truncated]"
		res.Truncated = true
	}
	return res
}

// SummarizeArgs renders a short one-line summary of tool arguments for
// progress display and transcript labels (e.g. the command being run).
func SummarizeArgs(input map[string]any) string {
	if input == nil {
		return ""
	}
	// The most common argument name carries the visible intent.
	for _, key := range []string{"command", "path", "url", "prompt", "query"} {
		if s, ok := input[key].(string); ok && s != "" {
			s = strings.ReplaceAll(s, "\n", " ")
			if len(s) > 80 {
				s = s[:80] + "…"
			}
			return s
		}
	}
	return ""
}

// toolOutputMaxBytes is the tool output cap (appendix A). Overridable by
// config at the wiring layer; core keeps the default. Atomic because the
// config setter races with in-flight tool executions (daemon background
// tasks run while another App is constructed).
var toolOutputMaxBytes atomic.Int64

func init() { toolOutputMaxBytes.Store(20000) }

// SetToolOutputMaxBytes overrides the truncation cap (config wiring).
func SetToolOutputMaxBytes(n int) {
	if n > 0 {
		toolOutputMaxBytes.Store(int64(n))
	}
}

// ToolOutputMaxBytes returns the active truncation cap.
func ToolOutputMaxBytes() int {
	return int(toolOutputMaxBytes.Load())
}

// persist stores a message best-effort: persistence failures must not break
// the turn (the store is a sink, not part of the conversation contract).
func (a *AgentImpl) persist(ctx context.Context, msg Message) {
	if a.store == nil {
		return
	}
	// Persist independently of the turn's cancellation: an assistant message
	// with tool_calls stored without its results replays as an invalid pair
	// on the next turn, so a killed run must still drain what it produced.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := a.store.AppendMessage(pctx, msg); err != nil {
		log.Printf("core: persist message failed: %v", err)
	}
}

// FlattenText renders a message's text parts for prompt/summary use.
func FlattenText(msg Message) string {
	var b strings.Builder
	for _, p := range msg.Content {
		if p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// MarshalMessage serializes a message for JSON storage (store.Message.Content).
func MarshalMessage(msg Message) []byte {
	raw, err := json.Marshal(msg.Content)
	if err != nil {
		return []byte("[]")
	}
	return raw
}

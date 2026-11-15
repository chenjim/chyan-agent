// Package trace defines chyan's local, versioned span model for one agent
// turn. It is deliberately small and self-owned: third-party GenAI tracing
// attributes are still evolving, and chyan's persistence contract must not
// follow them (ADR D47). Spans carry correlation IDs and redacted metadata
// only; prompts, tool payloads and memory bodies never enter a span.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"chyan.tech/chyan/internal/logging"
)

// SchemaVersion is stored with every span so a future format change can be
// detected instead of silently misread.
const SchemaVersion = 1

// Kind classifies one span. Low cardinality on purpose: dashboards group by
// kind, never by free-form names.
type Kind string

const (
	KindTurn             Kind = "turn"
	KindLLM              Kind = "llm_request"
	KindHook             Kind = "hook"
	KindToolExecution    Kind = "tool_execution"
	KindToolWaitApproval Kind = "tool_wait_approval"
	KindMemoryRecall     Kind = "memory_recall"
	KindMemoryWrite      Kind = "memory_write"
	KindSubagent         Kind = "subagent"
)

// Status is the span outcome.
type Status string

const (
	StatusOK        Status = "ok"
	StatusError     Status = "error"
	StatusCancelled Status = "cancelled"
)

// ErrorClass values returned to the model and stored on the span. A class is
// a routing signal ("retry"/"fix arguments"/"stop"), not an exception message.
const (
	ClassInvalidArgument = "invalid_argument"
	ClassPermission      = "permission_denied"
	ClassNotFound        = "not_found"
	ClassTimeout         = "timeout"
	ClassUnavailable     = "unavailable"
	ClassRateLimited     = "rate_limited"
	ClassCancelled       = "cancelled"
	ClassInternal        = "internal"
)

// Span is one timed unit of work. IDs are opaque; ParentID links a child to
// the enclosing span so a sub-agent's model and tool costs roll up to the
// root turn.
type Span struct {
	ID            string         `json:"id"`
	TraceID       string         `json:"trace_id"`
	ParentID      string         `json:"parent_id,omitempty"`
	Kind          Kind           `json:"kind"`
	Name          string         `json:"name,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	TaskID        string         `json:"task_id,omitempty"`
	AgentID       string         `json:"agent_id,omitempty"`
	ParentAgentID string         `json:"parent_agent_id,omitempty"`
	Start         time.Time      `json:"start"`
	End           time.Time      `json:"end"`
	Status        Status         `json:"status"`
	ErrorClass    string         `json:"error_class,omitempty"`
	Attrs         map[string]any `json:"attrs,omitempty"`
	Version       int            `json:"version"`
}

// Duration is End minus Start.
func (s Span) Duration() time.Duration { return s.End.Sub(s.Start) }

// Sink persists finished spans. Implementations must not make a turn fail
// when trace storage fails.
type Sink interface {
	AppendSpan(ctx context.Context, s Span) error
}

// Recorder writes spans to a sink. A nil sink recording is a no-op: tracing
// is best effort, never a reason to break a run.
type Recorder struct {
	sink Sink
	now  func() time.Time
}

// NewRecorder builds a recorder; nil disables persistence but keeps the
// context plumbing (IDs are still generated for correlation).
func NewRecorder(sink Sink) *Recorder {
	return &Recorder{sink: sink, now: time.Now}
}

// SetSink swaps the destination (wiring happens after App construction).
func (r *Recorder) SetSink(sink Sink) {
	if r == nil {
		return
	}
	r.sink = sink
}

// Record persists one span, redacting string values first.
func (r *Recorder) Record(ctx context.Context, s Span) {
	if r == nil || r.sink == nil {
		return
	}
	Sanitize(&s)
	_ = r.sink.AppendSpan(ctx, s) // tracing must never break a turn
}

func (r *Recorder) clock() time.Time {
	if r == nil || r.now == nil {
		return time.Now()
	}
	return r.now()
}

// carrier travels in context. Values are immutable: a child context gets its
// own carrier so a sibling span never overwrites the parent's current span.
type carrier struct {
	traceID       string
	sessionID     string
	taskID        string
	agentID       string
	parentAgentID string
	spanID        string
	version       int
}

type carrierKey struct{}

// WithTrace installs a fresh trace carrier without opening a span, so work that
// happens before the turn (memory injection, session setup) still lands in the
// same trace as the later turn. An existing trace is preserved.
func WithTrace(ctx context.Context, sessionID, taskID, agentID string) context.Context {
	if c, ok := ctx.Value(carrierKey{}).(carrier); ok && c.traceID != "" {
		return ctx
	}
	return context.WithValue(ctx, carrierKey{}, carrier{
		traceID: newID(), sessionID: sessionID, taskID: taskID, agentID: agentID,
		version: SchemaVersion,
	})
}

// TurnOptions configures a root turn span. When a trace already exists in ctx
// (a sub-agent reusing its parent's trace), the new turn hangs under the
// current span and keeps the parent's trace ID.
type TurnOptions struct {
	SessionID     string
	TaskID        string
	AgentID       string
	ParentAgentID string
}

// Turn is the root span of one user turn. End must be called exactly once.
type Turn struct {
	rec  *Recorder
	ctx  context.Context
	span Span
	mu   sync.Mutex
}

// BeginTurn creates the root span and returns a context carrying it.
func BeginTurn(ctx context.Context, rec *Recorder, opts TurnOptions) (context.Context, *Turn) {
	now := rec.clock()
	parent, hasParent := ctx.Value(carrierKey{}).(carrier)
	traceID := newID()
	parentSpan := ""
	if hasParent && parent.traceID != "" {
		traceID = parent.traceID
		parentSpan = parent.spanID
	}
	agentID := opts.AgentID
	if agentID == "" && hasParent {
		agentID = parent.agentID
	}
	parentAgentID := opts.ParentAgentID
	if parentAgentID == "" && hasParent && agentID != parent.agentID {
		parentAgentID = parent.agentID
	}
	sessionID := opts.SessionID
	if sessionID == "" && hasParent {
		sessionID = parent.sessionID
	}
	taskID := opts.TaskID
	if taskID == "" && hasParent {
		taskID = parent.taskID
	}
	span := Span{
		ID: newID(), TraceID: traceID, ParentID: parentSpan,
		Kind: KindTurn, SessionID: sessionID, TaskID: taskID,
		AgentID: agentID, ParentAgentID: parentAgentID,
		Start: now, Status: StatusOK, Attrs: map[string]any{}, Version: SchemaVersion,
	}
	t := &Turn{rec: rec, span: span}
	t.ctx = context.WithValue(ctx, carrierKey{}, carrier{
		traceID: traceID, sessionID: sessionID, taskID: taskID,
		agentID: agentID, parentAgentID: parentAgentID, spanID: span.ID,
		version: SchemaVersion,
	})
	return t.ctx, t
}

// Context returns the context carrying this turn's root span.
func (t *Turn) Context() context.Context { return t.ctx }

// ID returns the root span id.
func (t *Turn) ID() string { return t.span.ID }

// TraceID returns the trace id shared by the whole turn.
func (t *Turn) TraceID() string { return t.span.TraceID }

// SetAttr adds one metadata attribute at any point before End.
func (t *Turn) SetAttr(key string, value any) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.span.Attrs == nil {
		t.span.Attrs = map[string]any{}
	}
	t.span.Attrs[key] = value
}

// End closes the root span and records it.
func (t *Turn) End(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.span.End = t.rec.clock()
	SetOutcome(&t.span, err)
	span := t.span
	t.mu.Unlock()
	t.rec.Record(context.Background(), span)
}

// SpanHandle is a live child span. End closes it and records it.
type SpanHandle struct {
	rec  *Recorder
	ctx  context.Context
	span Span
}

// Start opens a child span under the span currently carried by ctx. The
// returned context carries the new span, so nested work links correctly.
func Start(ctx context.Context, rec *Recorder, kind Kind, name string, attrs map[string]any) (context.Context, *SpanHandle) {
	parent, ok := ctx.Value(carrierKey{}).(carrier)
	if !ok {
		parent = carrier{version: SchemaVersion}
	}
	span := Span{
		ID: newID(), TraceID: parent.traceID, ParentID: parent.spanID,
		Kind: kind, Name: name, SessionID: parent.sessionID, TaskID: parent.taskID,
		AgentID: parent.agentID, ParentAgentID: parent.parentAgentID,
		Start: rec.clock(), Status: StatusOK, Attrs: cloneAttrs(attrs), Version: SchemaVersion,
	}
	next := parent
	next.spanID = span.ID
	next.version = SchemaVersion
	h := &SpanHandle{rec: rec, span: span, ctx: context.WithValue(ctx, carrierKey{}, next)}
	return h.ctx, h
}

// Context returns the context carrying this span.
func (h *SpanHandle) Context() context.Context { return h.ctx }

// ID returns the span id.
func (h *SpanHandle) ID() string { return h.span.ID }

// SetAttr records one attribute.
func (h *SpanHandle) SetAttr(key string, value any) {
	if h == nil {
		return
	}
	if h.span.Attrs == nil {
		h.span.Attrs = map[string]any{}
	}
	h.span.Attrs[key] = value
}

// End closes and records the span. Safe to call on a nil handle.
func (h *SpanHandle) End(err error) {
	if h == nil {
		return
	}
	h.span.End = h.rec.clock()
	SetOutcome(&h.span, err)
	h.rec.Record(context.Background(), h.span)
}

// SetParentAgentHint marks the calling agent as the parent for any child
// agent that starts a turn from this context.
func SetParentAgentHint(ctx context.Context, agentID string) context.Context {
	c, ok := ctx.Value(carrierKey{}).(carrier)
	if !ok {
		return ctx
	}
	c.parentAgentID = agentID
	return context.WithValue(ctx, carrierKey{}, c)
}

// SetOutcome fills status and error class from err.
func SetOutcome(s *Span, err error) {
	switch {
	case err == nil:
		s.Status = StatusOK
		s.ErrorClass = ""
	case errors.Is(err, context.Canceled):
		s.Status = StatusCancelled
		s.ErrorClass = ClassCancelled
	default:
		if s.ErrorClass == "" {
			s.ErrorClass = Classify(err)
		}
		// An explicit cancelled class (e.g. the agent's ErrInterrupted) is a
		// user action, not a failure: keep it out of the error distribution.
		if s.ErrorClass == ClassCancelled {
			s.Status = StatusCancelled
		} else {
			s.Status = StatusError
		}
	}
}

// Classify maps an error to a low-cardinality class. Explicit typed errors
// win; otherwise the message is inspected for the common transport shapes.
func Classify(err error) string {
	if err == nil {
		return ""
	}
	var ce *ClassError
	if errors.As(err, &ce) {
		return ce.Class
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, context.Canceled):
		return ClassCancelled
	}
	msg := err.Error()
	switch {
	case containsAny(msg, "invalid argument", "invalid ", "missing ", "required", "unknown tool", "must be"):
		return ClassInvalidArgument
	case containsAny(msg, "denied", "permission"):
		return ClassPermission
	case containsAny(msg, "not found", "no such file"):
		return ClassNotFound
	case containsAny(msg, "timeout", "timed out", "deadline"):
		return ClassTimeout
	case containsAny(msg, "rate limit", "429", "too many requests"):
		return ClassRateLimited
	case containsAny(msg, "connection refused", "unavailable", "network", "eof", "502", "503"):
		return ClassUnavailable
	default:
		return ClassInternal
	}
}

// ClassError lets a component attach an explicit class while preserving the
// underlying error for wrapping and errors.Is.
type ClassError struct {
	Class string
	Err   error
}

func (e *ClassError) Error() string { return e.Err.Error() }
func (e *ClassError) Unwrap() error { return e.Err }

// WithClass wraps err with an explicit class (nil stays nil).
func WithClass(err error, class string) error {
	if err == nil {
		return nil
	}
	return &ClassError{Class: class, Err: err}
}

// Sanitize redacts credential-looking strings and bounds oversized values so
// trace rows stay metadata, not a copy of the transcript.
func Sanitize(s *Span) {
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	for k, v := range s.Attrs {
		switch val := v.(type) {
		case string:
			s.Attrs[k] = clip(logging.Redact(val), 200)
		case []string:
			out := make([]string, 0, len(val))
			for _, item := range val {
				out = append(out, clip(logging.Redact(item), 120))
			}
			s.Attrs[k] = out
		}
	}
}

// clip bounds a string by runes so multi-byte text (Chinese, emoji) is never
// cut into invalid UTF-8 at the truncation boundary.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && containsFold(s, sub) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFoldASCII(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func cloneAttrs(in map[string]any) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

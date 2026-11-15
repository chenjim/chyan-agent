package trace

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type memSink struct {
	mu    sync.Mutex
	spans []Span
}

func (m *memSink) AppendSpan(_ context.Context, s Span) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, s)
	return nil
}

func (m *memSink) all() []Span {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Span, len(m.spans))
	copy(out, m.spans)
	return out
}

// find returns the first recorded span of kind; spans are recorded when they
// end, so parent/child order in the slice is not creation order.
func (m *memSink) find(kind Kind) (Span, bool) {
	for _, sp := range m.all() {
		if sp.Kind == kind {
			return sp, true
		}
	}
	return Span{}, false
}

func TestBeginTurnRootAndChildSpans(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink)
	rootCtx, turn := BeginTurn(context.Background(), rec, TurnOptions{SessionID: "ses1", AgentID: "ses1"})
	turn.SetAttr("model", "m1")

	_, llm := Start(rootCtx, rec, KindLLM, "chat", map[string]any{"model": "m1"})
	llm.SetAttr("attempts", 2)
	llm.End(nil)

	_, tool := Start(rootCtx, rec, KindToolExecution, "bash", map[string]any{"tool": "bash"})
	tool.End(errors.New("permission denied: blocked"))

	_, wait := Start(rootCtx, rec, KindToolWaitApproval, "bash", map[string]any{"approved": true})
	wait.End(nil)

	turn.End(nil)
	spans := sink.all()
	if len(spans) != 4 {
		t.Fatalf("spans = %d, want 4", len(spans))
	}
	root := turnSpan(t, spans)
	if root.ParentID != "" || root.TraceID == "" {
		t.Errorf("root span = %+v", root)
	}
	if root.Status != StatusOK {
		t.Errorf("root status = %s", root.Status)
	}
	if root.Attrs["model"] != "m1" {
		t.Errorf("root attrs = %v", root.Attrs)
	}
	for _, sp := range spans {
		if sp.Kind == KindTurn {
			continue
		}
		if sp.TraceID != root.TraceID {
			t.Errorf("%s trace = %q, want %q", sp.Kind, sp.TraceID, root.TraceID)
		}
		if sp.ParentID != root.ID {
			t.Errorf("%s parent = %q, want root %q", sp.Kind, sp.ParentID, root.ID)
		}
		if sp.SessionID != "ses1" || sp.AgentID != "ses1" {
			t.Errorf("%s session/agent = %q/%q", sp.Kind, sp.SessionID, sp.AgentID)
		}
	}
	toolSpan, ok := sink.find(KindToolExecution)
	if !ok || toolSpan.Status != StatusError || toolSpan.ErrorClass != ClassPermission {
		t.Errorf("tool span outcome = %+v", toolSpan)
	}
}

func turnSpan(t *testing.T, spans []Span) Span {
	t.Helper()
	for _, sp := range spans {
		if sp.Kind == KindTurn {
			return sp
		}
	}
	t.Fatal("no turn span recorded")
	return Span{}
}

func TestBeginTurnReusesParentTraceForSubAgent(t *testing.T) {
	sink := &memSink{}
	rec := NewRecorder(sink)
	parentCtx, parent := BeginTurn(context.Background(), rec, TurnOptions{SessionID: "parent", AgentID: "parent"})
	subCtx, sub := Start(parentCtx, rec, KindSubagent, "worker", map[string]any{"agent": "worker"})

	childCtx, child := BeginTurn(subCtx, rec, TurnOptions{SessionID: "child", AgentID: "child"})
	child.SetAttr("tokens_in", 10)
	_, llm := Start(childCtx, rec, KindLLM, "chat", nil)
	llm.End(nil)
	child.End(nil)
	sub.End(nil)
	parent.End(nil)

	spans := sink.all()
	if len(spans) != 4 {
		t.Fatalf("spans = %d", len(spans))
	}
	subSpan, ok := sink.find(KindSubagent)
	if !ok || subSpan.AgentID != "parent" {
		t.Errorf("subagent span = %+v", subSpan)
	}
	childTurn := turnSpan(t, spans)
	if childTurn.TraceID != parent.TraceID() {
		t.Errorf("child trace = %q, parent = %q", childTurn.TraceID, parent.TraceID())
	}
	if childTurn.ParentID != subSpan.ID {
		t.Errorf("child parent = %q, want subagent span %q", childTurn.ParentID, subSpan.ID)
	}
	if childTurn.ParentAgentID != "parent" || childTurn.AgentID != "child" {
		t.Errorf("child agent linkage = %q -> %q", childTurn.AgentID, childTurn.ParentAgentID)
	}
	childLLM, ok := sink.find(KindLLM)
	if !ok || childLLM.ParentID != childTurn.ID {
		t.Errorf("child llm parent = %q, want %q", childLLM.ParentID, childTurn.ID)
	}
	if childLLM.AgentID != "child" {
		t.Errorf("child llm agent = %q", childLLM.AgentID)
	}
}

func TestCancelAndClassify(t *testing.T) {
	if got := Classify(context.Canceled); got != ClassCancelled {
		t.Errorf("cancel class = %s", got)
	}
	if got := Classify(WithClass(errors.New("boom"), ClassRateLimited)); got != ClassRateLimited {
		t.Errorf("explicit class = %s", got)
	}
	if got := Classify(errors.New("HTTP 503 service unavailable")); got != ClassUnavailable {
		t.Errorf("503 class = %s", got)
	}
	if got := Classify(errors.New("old_string not found in file")); got != ClassNotFound {
		t.Errorf("not found class = %s", got)
	}
	if got := Classify(errors.New("something odd")); got != ClassInternal {
		t.Errorf("default class = %s", got)
	}
}

func TestSanitizeRedactsAndClips(t *testing.T) {
	sp := Span{Attrs: map[string]any{
		"token":   "token=super-secret-value",
		"command": string(make([]byte, 300)),
		"ids":     []string{"a", "token=leak"},
	}}
	Sanitize(&sp)
	if sp.Attrs["token"] != "token=***" {
		t.Errorf("token attr = %v", sp.Attrs["token"])
	}
	if s, _ := sp.Attrs["command"].(string); len([]rune(s)) > 201 {
		t.Errorf("command not clipped: %d runes", len([]rune(s)))
	}
	if sp.Version != SchemaVersion {
		t.Errorf("version = %d", sp.Version)
	}
}

func TestSummarizeSplitsApprovalWaitFromToolExecution(t *testing.T) {
	base := time.Now()
	turn := Span{ID: "t", TraceID: "tr", Kind: KindTurn, Start: base, End: base.Add(10 * time.Second), Status: StatusOK}
	llm := Span{ID: "l", TraceID: "tr", ParentID: "t", Kind: KindLLM, Start: base, End: base.Add(2 * time.Second),
		Attrs: map[string]any{"tokens_in": float64(100), "tokens_out": float64(20), "cache_read_tokens": float64(30), "cost_usd": 0.5, "attempts": float64(2)}}
	tool := Span{ID: "x", TraceID: "tr", ParentID: "t", Kind: KindToolExecution, Start: base.Add(2 * time.Second), End: base.Add(7 * time.Second)}
	wait := Span{ID: "w", TraceID: "tr", ParentID: "x", Kind: KindToolWaitApproval, Start: base.Add(2 * time.Second), End: base.Add(6 * time.Second)}
	mem := Span{ID: "m", TraceID: "tr", ParentID: "t", Kind: KindMemoryRecall, Start: base.Add(7 * time.Second), End: base.Add(8 * time.Second), ErrorClass: ClassInternal, Status: StatusError}

	s := Summarize([]Span{turn, llm, tool, wait, mem})
	if s.DurationMS != 10000 {
		t.Errorf("duration = %d", s.DurationMS)
	}
	if s.ModelMS != 2000 {
		t.Errorf("model = %d", s.ModelMS)
	}
	if s.ToolMS != 1000 {
		t.Errorf("tool = %d, want 1000 (5s total minus 4s wait)", s.ToolMS)
	}
	if s.WaitMS != 4000 {
		t.Errorf("wait = %d", s.WaitMS)
	}
	if s.MemoryMS != 1000 {
		t.Errorf("memory = %d", s.MemoryMS)
	}
	// 10s - 2s model - 1s tool - 4s wait - 1s memory = 2s orchestration.
	if s.OverheadMS != 2000 {
		t.Errorf("overhead = %d", s.OverheadMS)
	}
	if s.TokensIn != 100 || s.TokensOut != 20 || s.CacheRead != 30 || s.Attempts != 2 {
		t.Errorf("usage = %+v", s)
	}
	if s.CostUSD != 0.5 {
		t.Errorf("cost = %f", s.CostUSD)
	}
	if s.Errors[ClassInternal] != 1 {
		t.Errorf("errors = %v", s.Errors)
	}
}

func TestSummarizeMergesParallelOverlap(t *testing.T) {
	base := time.Now()
	turn := Span{ID: "t", TraceID: "tr", Kind: KindTurn, Start: base, End: base.Add(3 * time.Second), Status: StatusOK}
	// Two children overlap: [0,2s] and [1s,3s] cover 3s, not 4s.
	a := Span{ID: "a", TraceID: "tr", ParentID: "t", Kind: KindLLM, Start: base, End: base.Add(2 * time.Second)}
	b := Span{ID: "b", TraceID: "tr", ParentID: "t", Kind: KindLLM, Start: base.Add(time.Second), End: base.Add(3 * time.Second)}
	// Two tools overlap too: [0,1s] and [0.5s,1.5s] cover 1.5s, not 2s.
	x := Span{ID: "x", TraceID: "tr", ParentID: "t", Kind: KindToolExecution, Start: base, End: base.Add(time.Second)}
	y := Span{ID: "y", TraceID: "tr", ParentID: "t", Kind: KindToolExecution, Start: base.Add(500 * time.Millisecond), End: base.Add(1500 * time.Millisecond)}

	s := Summarize([]Span{turn, a, b, x, y})
	if s.ModelMS != 3000 {
		t.Errorf("model = %d, want 3000 (merged)", s.ModelMS)
	}
	if s.ToolMS != 1500 {
		t.Errorf("tool = %d, want 1500 (merged)", s.ToolMS)
	}
	if s.DurationMS != 3000 {
		t.Errorf("duration = %d", s.DurationMS)
	}
}

func TestSanitizeKeepsValidUTF8(t *testing.T) {
	long := strings.Repeat("中", 300)
	sp := Span{Attrs: map[string]any{"args": long}}
	Sanitize(&sp)
	got, _ := sp.Attrs["args"].(string)
	if !utf8.ValidString(got) {
		t.Fatalf("clipped value is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clipped value missing ellipsis: %q", got[:10])
	}
	if len([]rune(got)) != 201 {
		t.Errorf("runes = %d, want 201", len([]rune(got)))
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(nil)
	if s.SpanCount != 0 || s.Errors == nil {
		t.Errorf("empty summary = %+v", s)
	}
}

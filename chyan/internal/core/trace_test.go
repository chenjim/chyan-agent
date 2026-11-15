package core

import (
	"context"
	"errors"
	"testing"

	"chyan.tech/chyan/internal/trace"
)

type spanSink struct{ spans []trace.Span }

func (s *spanSink) AppendSpan(_ context.Context, sp trace.Span) error {
	s.spans = append(s.spans, sp)
	return nil
}

func (s *spanSink) of(kind trace.Kind) []trace.Span {
	var out []trace.Span
	for _, sp := range s.spans {
		if sp.Kind == kind {
			out = append(out, sp)
		}
	}
	return out
}

func (s *spanSink) root() trace.Span {
	for _, sp := range s.spans {
		if sp.Kind == trace.KindTurn {
			return sp
		}
	}
	return trace.Span{}
}

func TestAgentRecordsTurnLLMAndToolSpans(t *testing.T) {
	backend := &stubBackend{responses: []Response{
		toolResp("echo", "c1", `{}`),
		textResp("done"),
	}}
	tools := NewToolRegistry()
	tool := &stubTool{name: "echo", output: "hello"}
	if err := tools.Register(tool); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{})
	sink := &spanSink{}
	agent.SetTracer(trace.NewRecorder(sink))

	if _, err := agent.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	root := sink.root()
	if root.ID == "" || root.Status != trace.StatusOK {
		t.Fatalf("root span = %+v", root)
	}
	if root.ParentID != "" {
		t.Errorf("root should have no parent: %+v", root)
	}
	if got := root.Attrs["tokens_in"]; got != 1 && got != float64(1) {
		// Only the final text response carries usage (1 in / 1 out).
		t.Errorf("turn tokens_in = %v", got)
	}
	llms := sink.of(trace.KindLLM)
	if len(llms) != 2 {
		t.Fatalf("llm spans = %d, want 2", len(llms))
	}
	for _, sp := range llms {
		if sp.ParentID != root.ID || sp.TraceID != root.TraceID {
			t.Errorf("llm span not linked to root: %+v", sp)
		}
	}
	toolSpans := sink.of(trace.KindToolExecution)
	if len(toolSpans) != 1 {
		t.Fatalf("tool spans = %d", len(toolSpans))
	}
	if toolSpans[0].Name != "echo" || toolSpans[0].ParentID != root.ID {
		t.Errorf("tool span = %+v", toolSpans[0])
	}
	if toolSpans[0].Attrs["result_bytes"] != 5 {
		t.Errorf("result_bytes = %v", toolSpans[0].Attrs["result_bytes"])
	}
	if tool.calls != 1 {
		t.Errorf("tool executed %d times", tool.calls)
	}
}

func TestAgentTurnSpanErrorClass(t *testing.T) {
	backend := &stubBackend{err: errors.New("connection refused")}
	agent, _ := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	sink := &spanSink{}
	agent.SetTracer(trace.NewRecorder(sink))

	if _, err := agent.Run(context.Background(), "go"); err == nil {
		t.Fatal("expected an error")
	}
	root := sink.root()
	if root.Status != trace.StatusError || root.ErrorClass != trace.ClassUnavailable {
		t.Errorf("root outcome = %s/%s", root.Status, root.ErrorClass)
	}
	llms := sink.of(trace.KindLLM)
	if len(llms) != 1 || llms[0].Status != trace.StatusError {
		t.Errorf("llm spans = %+v", llms)
	}
}

func TestInterruptedTurnIsCancelledNotError(t *testing.T) {
	backend := &stubBackend{err: context.Canceled}
	agent, _ := newTestAgent(t, backend, NewToolRegistry(), AgentConfig{})
	sink := &spanSink{}
	agent.SetTracer(trace.NewRecorder(sink))

	if _, err := agent.Run(context.Background(), "go"); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	root := sink.root()
	if root.Status != trace.StatusCancelled || root.ErrorClass != trace.ClassCancelled {
		t.Errorf("root outcome = %s/%s, want cancelled", root.Status, root.ErrorClass)
	}
}

func TestRepeatedFailingToolIsRecordedAsBlockedSpan(t *testing.T) {
	backend := &stubBackend{responses: []Response{
		toolResp("boom", "c1", `{}`),
		toolResp("boom", "c2", `{}`),
		textResp("gave up"),
	}}
	tools := NewToolRegistry()
	if err := tools.Register(&stubTool{name: "boom", err: errors.New("invalid argument: path is required")}); err != nil {
		t.Fatal(err)
	}
	agent, _ := newTestAgent(t, backend, tools, AgentConfig{MaxRepeatedToolFailures: 1})
	sink := &spanSink{}
	agent.SetTracer(trace.NewRecorder(sink))

	if _, err := agent.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var blocked, executed int
	for _, sp := range sink.of(trace.KindToolExecution) {
		if sp.Attrs["blocked"] == true {
			blocked++
			if sp.ErrorClass != trace.ClassInvalidArgument {
				t.Errorf("blocked class = %s", sp.ErrorClass)
			}
			continue
		}
		executed++
	}
	if executed != 1 || blocked != 1 {
		t.Errorf("executed=%d blocked=%d", executed, blocked)
	}
}

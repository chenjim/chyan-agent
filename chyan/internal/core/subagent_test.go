package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubAgentFactory produces scripted child agents and tracks dispatches.
type stubAgentFactory struct {
	mu     sync.Mutex
	depths []int
	names  []string
	result Response
	err    error
	block  chan struct{} // when non-nil, Run blocks until closed
}

func (f *stubAgentFactory) newAgent(_ context.Context, req TaskRequest, parent AgentContext) (*AgentImpl, error) {
	f.mu.Lock()
	f.depths = append(f.depths, parent.Depth)
	f.names = append(f.names, req.AgentName)
	block := f.block
	result := f.result
	err := f.err
	f.mu.Unlock()

	backend := &scriptedBackend{resp: result, err: err, block: block}
	ctxMgr := NewContextManager(ContextConfig{ContextLimit: 100000},
		"child", nil, ContextDeps{Counter: HeuristicCounter{}, SessionID: "child-" + req.AgentName})
	return NewAgent(backend, ctxMgr, NewToolRegistry(), nil, "child-"+req.AgentName,
		AgentConfig{MaxToolIterations: 3}), nil
}

// scriptedBackend returns one scripted response; blocks in Run when told.
type scriptedBackend struct {
	resp  Response
	err   error
	block chan struct{}
}

func (s *scriptedBackend) Chat(ctx context.Context, _ Request) (Response, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
	if s.err != nil {
		return Response{}, s.err
	}
	return s.resp, nil
}

func (s *scriptedBackend) Stream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	return s.Chat(ctx, req)
}

func TestDispatchReturnsSummaryOnly(t *testing.T) {
	f := &stubAgentFactory{result: textResp("child answer")}
	runner := NewSubAgentRunner(SubAgentConfig{}, SubAgentDeps{NewAgent: f.newAgent})
	res, err := runner.Dispatch(context.Background(),
		TaskRequest{AgentName: "explorer", Prompt: "look around"}, AgentContext{SessionID: "p1", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "child answer" {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.SessionID == "" {
		t.Error("result must carry the child session id for read_session")
	}
	if !res.Success {
		t.Error("success = false")
	}
}

func TestDispatchRejectsNestingTooDeep(t *testing.T) {
	f := &stubAgentFactory{result: textResp("x")}
	runner := NewSubAgentRunner(SubAgentConfig{MaxDepth: 2}, SubAgentDeps{NewAgent: f.newAgent})
	// Depth 2 already at the cap (MaxDepth=2): grandchild dispatch rejected.
	_, err := runner.Dispatch(context.Background(), TaskRequest{Prompt: "x"},
		AgentContext{SessionID: "p", Depth: 2})
	if !errors.Is(err, ErrNestingTooDeep) {
		t.Fatalf("err = %v, want ErrNestingTooDeep", err)
	}
	// Anonymous dispatch at depth 0/1 works.
	if _, err := runner.Dispatch(context.Background(), TaskRequest{Prompt: "x"},
		AgentContext{SessionID: "p", Depth: 0}); err != nil {
		t.Fatalf("depth 0 dispatch failed: %v", err)
	}
}

func TestDispatchConcurrencyCap(t *testing.T) {
	block := make(chan struct{})
	f := &stubAgentFactory{result: textResp("x"), block: block}
	runner := NewSubAgentRunner(SubAgentConfig{MaxConcurrent: 2},
		SubAgentDeps{NewAgent: f.newAgent})

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = runner.Dispatch(context.Background(), TaskRequest{Prompt: "x"},
				AgentContext{SessionID: "p", Depth: 0})
		}(i)
	}
	// Give the first two time to occupy both slots.
	time.Sleep(50 * time.Millisecond)
	close(block)
	wg.Wait()

	rejected := 0
	for _, err := range errs {
		if errors.Is(err, ErrTooManySubAgents) {
			rejected++
		}
	}
	if rejected == 0 {
		t.Error("concurrency cap should reject at least one dispatch")
	}
}

func TestDispatchChildError(t *testing.T) {
	f := &stubAgentFactory{err: errors.New("child exploded")}
	runner := NewSubAgentRunner(SubAgentConfig{}, SubAgentDeps{NewAgent: f.newAgent})
	res, err := runner.Dispatch(context.Background(), TaskRequest{Prompt: "x"},
		AgentContext{SessionID: "p", Depth: 0})
	if err == nil {
		t.Fatal("child error must propagate")
	}
	if res.Success || res.Err == "" {
		t.Errorf("result = %+v, want failure with error text", res)
	}
}

func TestDispatchTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	f := &stubAgentFactory{result: textResp("x"), block: block}
	runner := NewSubAgentRunner(SubAgentConfig{Timeout: 50 * time.Millisecond},
		SubAgentDeps{NewAgent: f.newAgent})
	_, err := runner.Dispatch(context.Background(), TaskRequest{Prompt: "x"},
		AgentContext{SessionID: "p", Depth: 0})
	if err == nil {
		t.Fatal("timeout must error")
	}
}

func TestDispatchParallelAggregates(t *testing.T) {
	f := &stubAgentFactory{result: textResp("result")}
	runner := NewSubAgentRunner(SubAgentConfig{MaxConcurrent: 4},
		SubAgentDeps{NewAgent: f.newAgent})
	reqs := []TaskRequest{{AgentName: "a", Prompt: "1"}, {AgentName: "b", Prompt: "2"}}
	results := runner.DispatchParallel(context.Background(), reqs, AgentContext{SessionID: "p", Depth: 0})
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	for i, r := range results {
		if !r.Success || r.Summary != "result" {
			t.Errorf("result[%d] = %+v", i, r)
		}
	}
}

func TestSubAgentDepthTracking(t *testing.T) {
	f := &stubAgentFactory{result: textResp("x")}
	runner := NewSubAgentRunner(SubAgentConfig{MaxDepth: 3}, SubAgentDeps{NewAgent: f.newAgent})
	for _, depth := range []int{0, 1, 2} {
		_, err := runner.Dispatch(context.Background(), TaskRequest{Prompt: "x"},
			AgentContext{SessionID: "p", Depth: depth})
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Children receive parent depth + 1.
	for i, d := range f.depths {
		want := i + 1
		if d != want {
			t.Errorf("child %d got depth %d, want %d", i, d, want)
		}
	}
}

func TestSummarizeChild(t *testing.T) {
	out := SummarizeChild(TaskResult{SessionID: "s1", Summary: "done work", Success: true})
	if !strings.Contains(out, "done work") || !strings.Contains(out, "read_session(s1)") {
		t.Errorf("summary = %q", out)
	}
	fail := SummarizeChild(TaskResult{Err: "boom"})
	if !strings.Contains(fail, "boom") {
		t.Errorf("failure summary = %q", fail)
	}
}

func TestInterruptCancelsRun(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	f := &stubAgentFactory{result: textResp("x"), block: block}
	runner := NewSubAgentRunner(SubAgentConfig{}, SubAgentDeps{NewAgent: f.newAgent})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runner.Dispatch(ctx, TaskRequest{Prompt: "x"}, AgentContext{SessionID: "p", Depth: 0})
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel() // parent cancellation propagates to the child run
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled dispatch must error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not return after cancellation")
	}
}

package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// reqRecorder captures every LLM request in order. The agent's call is
// requests[0]; the follow-up title call (if any) comes later — so assertions
// target the first request, not the last.
type reqRecorder struct {
	mu       sync.Mutex
	requests []core.Request
}

func (r *reqRecorder) Chat(_ context.Context, req core.Request) (core.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: "ok"}}}}, nil
}

func (r *reqRecorder) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	resp, err := r.Chat(ctx, req)
	if err == nil && onDelta != nil {
		for _, p := range resp.Message.Content {
			onDelta(core.Delta{Text: p.Text})
		}
	}
	return resp, err
}

func (r *reqRecorder) first() core.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return core.Request{}
	}
	return r.requests[0]
}

// newSession creates a fixed session for a run.
func newSession(t *testing.T, a *App, id string) {
	t.Helper()
	if err := a.store.CreateSession(context.Background(), store.Session{
		ID: id, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestSaveLoadPlan(t *testing.T) {
	a := newTestApp(t, &reqRecorder{})
	ctx := context.Background()

	// Missing plan: empty, no error (executor falls back to ReAct).
	got, err := a.LoadPlan(ctx, "ses_absent")
	if err != nil {
		t.Fatalf("LoadPlan missing: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("missing plan = %+v, want empty", got)
	}

	plan := core.Plan{Goal: "add README", Steps: []core.PlanStep{
		{Title: "create", Detail: "write README.md", Status: core.StepPending},
	}}
	if err := a.SavePlan(ctx, "ses_p", plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	got, err = a.LoadPlan(ctx, "ses_p")
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if got.Goal != "add README" || len(got.Steps) != 1 || got.Steps[0].Title != "create" {
		t.Fatalf("roundtrip plan = %+v", got)
	}
}

func TestPlannerPhaseIsReadOnly(t *testing.T) {
	rec := &reqRecorder{}
	a := newTestApp(t, rec)
	if _, _, err := a.RunOpts(context.Background(), "", "plan the work",
		RunOptions{Phase: core.PhasePlanner}); err != nil {
		t.Fatalf("RunOpts planner: %v", err)
	}
	req := rec.first()
	names := map[string]bool{}
	for _, tool := range req.Tools {
		names[tool.Name()] = true
	}
	for _, drop := range []string{"write", "edit", "delete", "remember", "task", "tts", "transcribe"} {
		if names[drop] {
			t.Errorf("planner phase kept write tool %q", drop)
		}
	}
	for _, want := range []string{"read", "grep", "submit_plan"} {
		if !names[want] {
			t.Errorf("planner phase missing %q", want)
		}
	}
	if len(req.Messages) == 0 {
		t.Fatal("no messages sent")
	}
	if !strings.Contains(core.FlattenText(req.Messages[0]), "Planner") {
		t.Errorf("planner prompt not used:\n%s", core.FlattenText(req.Messages[0]))
	}
}

func TestExecutorPhaseInjectsPlan(t *testing.T) {
	rec := &reqRecorder{}
	a := newTestApp(t, rec)
	ctx := context.Background()
	sid := "ses_exec"
	newSession(t, a, sid)
	if err := a.SavePlan(ctx, sid, core.Plan{
		Goal:  "add README",
		Steps: []core.PlanStep{{Title: "create file", Detail: "write README.md"}},
	}); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if _, _, err := a.RunOpts(ctx, sid, "do it",
		RunOptions{Phase: core.PhaseExecutor}); err != nil {
		t.Fatalf("RunOpts executor: %v", err)
	}
	req := rec.first()
	if len(req.Messages) == 0 {
		t.Fatal("no messages sent")
	}
	sys := core.FlattenText(req.Messages[0])
	if !strings.Contains(sys, "Executor") {
		t.Errorf("executor prompt not used:\n%s", sys)
	}
	if !strings.Contains(sys, "add README") {
		t.Errorf("plan goal not injected:\n%s", sys)
	}
}

// TestPlanToExecuteClosedLoop drives the full data flow: the planner calls
// submit_plan (persisted via the session KV), then the executor injects that
// same plan into its prompt.
func TestPlanToExecuteClosedLoop(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{
		{Message: core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
			ID: "c1", Name: "submit_plan", Input: map[string]any{
				"goal":  "write greeting",
				"steps": []any{map[string]any{"title": "create file", "detail": "hi.txt"}},
			}}}}},
		textResp("plan ready"),
		textResp("executing the plan"),
	}}
	a := newTestApp(t, backend)
	ctx := context.Background()
	sid := "ses_loop"
	// A fixed title keeps titleSession from consuming a scripted response.
	if err := a.store.CreateSession(ctx, store.Session{ID: sid, Title: "fixed",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, _, err := a.RunOpts(ctx, sid, "plan a greeting",
		RunOptions{Phase: core.PhasePlanner}); err != nil {
		t.Fatalf("planner run: %v", err)
	}
	plan, err := a.LoadPlan(ctx, sid)
	if err != nil || plan.Goal != "write greeting" {
		t.Fatalf("submit_plan did not persist: plan=%+v err=%v", plan, err)
	}

	if _, _, err := a.RunOpts(ctx, sid, "execute",
		RunOptions{Phase: core.PhaseExecutor}); err != nil {
		t.Fatalf("executor run: %v", err)
	}
	if !strings.Contains(core.FlattenText(backend.lastRequest.Messages[0]), "write greeting") {
		t.Fatalf("executor prompt missing plan:\n%s", core.FlattenText(backend.lastRequest.Messages[0]))
	}
}

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeTool records whether Execute ran (for guard tests).
type fakeTool struct {
	name   string
	called bool
	input  map[string]any
}

func (f *fakeTool) Name() string           { return f.name }
func (f *fakeTool) Schema() map[string]any { return map[string]any{} }
func (f *fakeTool) Execute(_ context.Context, c ToolCall) ToolResult {
	f.called = true
	f.input = c.Input
	return ToolResult{Content: "ok"}
}

// fakePlanStore captures saved plans.
type fakePlanStore struct {
	saved map[string]Plan
	err   error
}

func (f *fakePlanStore) SavePlan(_ context.Context, sid string, p Plan) error {
	if f.err != nil {
		return f.err
	}
	if f.saved == nil {
		f.saved = map[string]Plan{}
	}
	f.saved[sid] = p
	return nil
}

func (f *fakePlanStore) LoadPlan(_ context.Context, sid string) (Plan, error) {
	if f.err != nil {
		return Plan{}, f.err
	}
	return f.saved[sid], nil
}

func TestSubmitPlanToolSavesPlan(t *testing.T) {
	store := &fakePlanStore{}
	tool := NewSubmitPlanTool(store, "ses1")
	if tool.Name() != "submit_plan" {
		t.Fatalf("name = %q", tool.Name())
	}
	res := tool.Execute(context.Background(), ToolCall{Input: map[string]any{
		"goal": "add a README",
		"steps": []any{
			map[string]any{"title": "create file", "detail": "write README.md"},
			map[string]any{"title": "fill content", "detail": "sections"},
		},
	}})
	if res.Err != nil {
		t.Fatalf("unexpected err: %v", res.Err)
	}
	got := store.saved["ses1"]
	if got.Goal != "add a README" || len(got.Steps) != 2 {
		t.Fatalf("saved plan = %+v", got)
	}
	if got.Steps[0].Status != StepPending {
		t.Fatalf("step status = %q, want pending", got.Steps[0].Status)
	}
	if !strings.Contains(res.Content, "2 step") {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestSubmitPlanToolValidation(t *testing.T) {
	tool := NewSubmitPlanTool(&fakePlanStore{}, "s")
	for name, input := range map[string]map[string]any{
		"missing goal":  {"steps": []any{map[string]any{"title": "a"}}},
		"missing steps": {"goal": "g"},
		"empty steps":   {"goal": "g", "steps": []any{}},
		"no valid step": {"goal": "g", "steps": []any{map[string]any{"title": ""}}},
	} {
		if res := tool.Execute(context.Background(), ToolCall{Input: input}); res.Err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// Nil store disables the tool.
	nilTool := NewSubmitPlanTool(nil, "s")
	if res := nilTool.Execute(context.Background(), ToolCall{Input: map[string]any{
		"goal": "g", "steps": []any{map[string]any{"title": "a"}}}}); res.Err == nil {
		t.Error("nil store: expected error")
	}
	// Store failure propagates.
	bad := NewSubmitPlanTool(&fakePlanStore{err: errors.New("boom")}, "s")
	if res := bad.Execute(context.Background(), ToolCall{Input: map[string]any{
		"goal": "g", "steps": []any{map[string]any{"title": "a"}}}}); res.Err == nil {
		t.Error("store error: expected error")
	}
}

func TestWriteReasonReadOnly(t *testing.T) {
	for _, cmd := range []string{
		"ls -la", "cat foo.txt", "grep foo bar", "find . -name '*.go'",
		"git log --oneline", "git diff", "git status", "git show HEAD",
		"git branch", "git branch -a", "git tag -l", "git remote -v",
		"git config --get user.name", "git reflog",
		"git branch -r", "git branch -vv", "git branch --show-current",
		"git tag -n", "git tag --sort=-creatordate", "git tag --format=x",
		"git tag --points-at=HEAD", "git tag --contains=HEAD",
		"git remote", "git remote show origin", "git remote get-url origin",
		"git config --get-all user.name", "git config --get-regexp user",
		"git config -l", "git config --show-origin --get user.name",
		"git reflog show",
		"sed s/a/b/ f", "awk '{print}' f", "ls | head", "cat a && cat b",
		"wc -l f",
	} {
		if r := writeReason(cmd); r != "" {
			t.Errorf("writeReason(%q) = %q, want allowed", cmd, r)
		}
	}
}

func TestWriteReasonRejectsWrites(t *testing.T) {
	for _, cmd := range []string{
		"rm foo", "mv a b", "cp a b", "mkdir d", "touch f", "chmod +x f",
		"echo hi > out.txt", "cat a >> b", "echo x | tee f",
		"sed -i s/a/b/ f", "perl -pi -e s/a/b/ f",
		"git commit -m x", "git push", "git checkout main", "git add .",
		"git branch -D old", "git branch new", "git tag -d v1", "git tag v1",
		"git remote add o url", "git remote set-url o url",
		"git config user.name x", "git config --list && git config a b",
		"git config --unset user.name", "git remote prune origin",
		// Bare "git config" lists, but it is rejected conservatively: no
		// subcommand means the intent cannot be verified.
		"git config",
		"git tag --delete v1", "git reflog drop HEAD@{1}",
		"git reflog expire --all", "git reflog delete HEAD@{1}",
		"python3 -c \"open('f','w')\"", "node -e \"require('fs').writeFileSync('f','x')\"",
		"sh -c 'rm -rf x'", "perl -e 'open F'", "sudo ls", "find . -delete",
		"find . -exec rm {} ;",
		"ls && rm foo", "cat f; mv a b", "npm install x",
	} {
		if r := writeReason(cmd); r == "" {
			t.Errorf("writeReason(%q) = allowed, want rejected", cmd)
		}
	}
}

func TestReadOnlyCommandGuard(t *testing.T) {
	inner := &fakeTool{name: "bash"}
	g := NewReadOnlyCommandGuard(inner)
	if g.Name() != "bash" {
		t.Fatalf("name = %q", g.Name())
	}
	// Read-only command passes through to the inner tool.
	if res := g.Execute(context.Background(), ToolCall{Input: map[string]any{"command": "ls"}}); res.Err != nil {
		t.Fatalf("read command rejected: %v", res.Err)
	}
	if !inner.called {
		t.Fatal("inner tool not called for read command")
	}
	// Write command is blocked before reaching the inner tool.
	inner.called = false
	res := g.Execute(context.Background(), ToolCall{Input: map[string]any{"command": "rm -rf /"}})
	if res.Err == nil {
		t.Fatal("write command not rejected")
	}
	if inner.called {
		t.Fatal("inner tool called for write command")
	}
}

func TestPlannerRegistryFilters(t *testing.T) {
	src := NewToolRegistry()
	names := []string{"read", "write", "bash", "task", "remember", "some_mcp", "grep"}
	for _, n := range names {
		if err := src.Register(&fakeTool{name: n}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
	submit := NewSubmitPlanTool(&fakePlanStore{}, "s")
	reg := PlannerRegistry(src, submit)

	got := map[string]bool{}
	for _, tool := range reg.Tools() {
		got[tool.Name()] = true
	}
	// Allowed reads + submit_plan + bash (guarded) stay; writes/task/mcp drop.
	for _, want := range []string{"read", "grep", "bash", "submit_plan"} {
		if !got[want] {
			t.Errorf("expected %q in planner registry", want)
		}
	}
	for _, drop := range []string{"write", "task", "remember", "some_mcp"} {
		if got[drop] {
			t.Errorf("expected %q dropped from planner registry", drop)
		}
	}
	// bash is wrapped in the read-only guard.
	bash, ok := reg.Get("bash")
	if !ok {
		t.Fatal("bash missing")
	}
	if _, isGuard := bash.(*ReadOnlyCommandGuard); !isGuard {
		t.Errorf("bash not wrapped in ReadOnlyCommandGuard: %T", bash)
	}
}

func TestRenderPlan(t *testing.T) {
	if RenderPlan(Plan{}) != "" {
		t.Error("empty plan should render empty")
	}
	out := RenderPlan(Plan{Goal: "g", Steps: []PlanStep{
		{Title: "t1", Detail: "d1"},
		{Title: "t2", Status: StepDone},
	}})
	for _, want := range []string{"Goal: g", "1. [pending] t1 — d1", "2. [done] t2"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderPlan missing %q in %q", want, out)
		}
	}
}

func TestPlanEmpty(t *testing.T) {
	if !(Plan{}).Empty() {
		t.Error("zero plan should be empty")
	}
	if (Plan{Steps: []PlanStep{{Title: "x"}}}).Empty() {
		t.Error("plan with steps should not be empty")
	}
}

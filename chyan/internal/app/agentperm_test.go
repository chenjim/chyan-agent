package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// writeAgentDef drops an agent definition into <workDir>/.agents/agents.
func writeAgentDef(t *testing.T, workDir, name, body string) {
	t.Helper()
	dir := filepath.Join(workDir, ".agents", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAgentDefParsesFrontmatter: model + permission subset come out of the
// frontmatter; unknown effects never widen (dropped).
func TestAgentDefParsesFrontmatter(t *testing.T) {
	workDir := t.TempDir()
	writeAgentDef(t, workDir, "explorer", `---
model: fast-model
description: read-only explorer
permission:
  edit: deny
  bash: deny
  web: ask
  bogus: launch
---
You are a read-only explorer.`)

	a, err := New(Options{
		SkipGate: true, SkipExtensions: true,
		Home: t.TempDir(), MemoryRoot: t.TempDir(), WorkDir: workDir,
		Backend: &stubBackend{responses: []core.Response{textResp("x")}},
		Store:   store.NewMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.SetAgent("explorer"); err != nil {
		t.Fatal(err)
	}
	if a.agentModel != "fast-model" {
		t.Errorf("agentModel = %q", a.agentModel)
	}
	if a.prompt != "You are a read-only explorer." {
		t.Errorf("prompt = %q", a.prompt)
	}
	if len(a.agentPerms) != 3 {
		t.Fatalf("agentPerms = %v (bogus effect must be dropped)", a.agentPerms)
	}
	if a.agentPerms["edit"] != core.EffectDeny || a.agentPerms["bash"] != core.EffectDeny {
		t.Errorf("deny rules = %v", a.agentPerms)
	}
	if a.agentPerms["web"] != core.EffectAsk {
		t.Errorf("ask rule = %v", a.agentPerms)
	}
	if _, ok := a.agentPerms["bogus"]; ok {
		t.Error("unknown effect must be dropped, not stored")
	}
}

// TestAgentPermDenyBlocksReadOnly: a denied action hard-blocks even tools
// that would otherwise bypass the gate (FR-5, architecture 4.1.1 explorer).
func TestAgentPermDenyBlocksReadOnly(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	tool, _ := a.Registry().Get("read")
	// Wrap like one run does, with a deny subset.
	gated := &gatedTool{inner: tool, app: a, session: "ses-x",
		perms: map[string]core.Effect{"read": core.EffectDeny}}
	res := gated.Execute(context.Background(), core.ToolCall{Input: map[string]any{"path": "a.txt"}})
	if res.Err == nil {
		t.Fatal("denied read must fail")
	}
	audits, err := a.store.ListAudit(context.Background(), "ses-x", 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range audits {
		if e.Action == "read" && !e.Approved {
			found = true
		}
	}
	if !found {
		t.Errorf("denial must be audited: %+v", audits)
	}
}

// TestAgentPermAskForcesGate: declaring ask on a read-only tool pulls it
// through the gate (only narrows: allow on a write tool cannot bypass it).
func TestAgentPermAskForcesGate(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	gate := &stubGate{approved: true, verdict: "low"}
	a.SetGate(gate)

	read, _ := a.Registry().Get("read")
	gated := &gatedTool{inner: read, app: a, session: "ses-y",
		perms: map[string]core.Effect{"read": core.EffectAsk}}
	_ = gated.Execute(context.Background(), core.ToolCall{Input: map[string]any{"path": "a.txt"}})
	if len(gate.asks) != 1 {
		t.Errorf("declared ask must force the gate: %v", gate.asks)
	}

	// allow on a gated tool must NOT bypass the gate: it still asks.
	// Wrap the raw (unwrapped) tool — the registry stores gated wrappers.
	bash := a.rawTools["bash"]
	gated2 := &gatedTool{inner: bash, app: a, session: "ses-y",
		perms: map[string]core.Effect{"bash": core.EffectAllow}, needsApproval: true}
	before := len(gate.asks)
	_ = gated2.Execute(context.Background(), core.ToolCall{Input: map[string]any{"command": "ls"}})
	if len(gate.asks) != before+1 {
		t.Errorf("allow must not bypass the gate: %v", gate.asks)
	}
}

// TestMergePermsOnlyNarrows: deny > ask > allow at every key.
func TestMergePermsOnlyNarrows(t *testing.T) {
	cases := []struct {
		parent, child, want map[string]core.Effect
	}{
		{nil, map[string]core.Effect{"edit": core.EffectDeny}, map[string]core.Effect{"edit": core.EffectDeny}},
		{map[string]core.Effect{"edit": core.EffectDeny}, nil, map[string]core.Effect{"edit": core.EffectDeny}},
		{map[string]core.Effect{"edit": core.EffectAllow}, map[string]core.Effect{"edit": core.EffectAsk}, map[string]core.Effect{"edit": core.EffectAsk}},
		{map[string]core.Effect{"edit": core.EffectAsk}, map[string]core.Effect{"edit": core.EffectAllow}, map[string]core.Effect{"edit": core.EffectAsk}},
		{map[string]core.Effect{"edit": core.EffectAsk}, map[string]core.Effect{"edit": core.EffectDeny}, map[string]core.Effect{"edit": core.EffectDeny}},
		{map[string]core.Effect{"bash": core.EffectDeny}, map[string]core.Effect{"web": core.EffectAsk},
			map[string]core.Effect{"bash": core.EffectDeny, "web": core.EffectAsk}},
	}
	for i, c := range cases {
		got := mergePerms(c.parent, c.child)
		if len(got) != len(c.want) {
			t.Fatalf("case %d: merged = %v, want %v", i, got, c.want)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("case %d: %s = %s, want %s", i, k, got[k], v)
			}
		}
	}
}

// TestSubAgentPermsNarrow: a child's declaration narrows the parent's subset.
func TestSubAgentPermsNarrow(t *testing.T) {
	workDir := t.TempDir()
	writeAgentDef(t, workDir, "explorer", `---
permission:
  edit: deny
  bash: deny
---
explore only`)
	a, err := New(Options{
		SkipGate: true, SkipExtensions: true,
		Home: t.TempDir(), MemoryRoot: t.TempDir(), WorkDir: workDir,
		Backend: &stubBackend{responses: []core.Response{textResp("x")}},
		Store:   store.NewMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.SetAgent("explorer"); err != nil {
		t.Fatal(err)
	}
	// Effective perms = parent ∩ child; merging with an extra deny wins.
	merged := mergePerms(a.agentPerms, map[string]core.Effect{"web": core.EffectDeny})
	if merged["edit"] != core.EffectDeny || merged["bash"] != core.EffectDeny {
		t.Errorf("child denials lost: %v", merged)
	}
	if merged["web"] != core.EffectDeny {
		t.Errorf("merged web = %v", merged["web"])
	}
}

// TestRunOptsAgentSessionMetadataBeforeLLM verifies creation records the
// pinned run even when no usage event can repair the model afterwards.
func TestRunOptsAgentSessionMetadataBeforeLLM(t *testing.T) {
	workDir := t.TempDir()
	writeAgentDef(t, workDir, "pinned", `---
model: pinned-model
---
pinned prompt`)
	a, err := New(Options{
		SkipGate: true, SkipExtensions: true,
		Home: t.TempDir(), MemoryRoot: t.TempDir(), WorkDir: workDir,
		Backend: errBackend{}, Store: store.NewMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, sid, err := a.RunOpts(context.Background(), "", "run", RunOptions{Agent: "pinned"})
	if err == nil {
		t.Fatal("expected the injected LLM failure")
	}
	s, err := a.store.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.Agent != "pinned" || s.Model != "pinned-model" {
		t.Errorf("new session metadata = agent %q, model %q", s.Agent, s.Model)
	}
}

func TestRunOptsSubAgentInheritsPinnedAgent(t *testing.T) {
	workDir := t.TempDir()
	writeAgentDef(t, workDir, "pinned", `---
model: pinned-model
permission:
  read: deny
---
pinned prompt`)
	writeAgentDef(t, workDir, "global", `---
model: global-model
permission:
  read: allow
---
global prompt`)
	backend := &stubBackend{responses: []core.Response{
		{Message: core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
			ID: "task-1", Name: "task", Input: map[string]any{"prompt": "inspect"},
		}}}},
		{Message: core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
			ID: "read-1", Name: "read", Input: map[string]any{"path": "a.txt"},
		}}}},
		textResp("child done"),
		textResp("parent done"),
	}}
	a, err := New(Options{
		SkipGate: true, SkipExtensions: true,
		Home: t.TempDir(), MemoryRoot: t.TempDir(), WorkDir: workDir,
		Backend: backend, Store: store.NewMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.SetAgent("global"); err != nil {
		t.Fatal(err)
	}
	_, sid, err := a.RunOpts(context.Background(), "", "delegate", RunOptions{Agent: "pinned"})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := a.store.ListSessions(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var child *store.Session
	for i := range sessions {
		if sessions[i].ParentID == sid {
			child = &sessions[i]
		}
	}
	if child == nil {
		t.Fatal("sub-agent session was not created")
	}
	if child.Model != "pinned-model" {
		t.Errorf("child model = %q, want pinned-model", child.Model)
	}
	audits, err := a.store.ListAudit(context.Background(), child.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	denied := false
	for _, entry := range audits {
		if entry.Action == "read" && !entry.Approved && entry.Verdict == "deny" {
			denied = true
		}
	}
	if !denied {
		t.Errorf("child did not inherit pinned read denial: %+v", audits)
	}
}

// TestRegistryForBuildsRunView: each run gets its own wrappers bound to its
// session and permissions.
func TestRegistryForBuildsRunView(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	reg := a.registryFor("ses-a", map[string]core.Effect{"bash": core.EffectDeny}, a.WorkDir())
	tool, ok := reg.Get("bash")
	if !ok {
		t.Fatal("bash must be in the run view")
	}
	gated, ok := tool.(*gatedTool)
	if !ok {
		t.Fatalf("run view tool = %T, want *gatedTool", tool)
	}
	if gated.session != "ses-a" {
		t.Errorf("session = %q", gated.session)
	}
	// The deny subset blocks bash in this run only.
	res := gated.Execute(context.Background(), core.ToolCall{Input: map[string]any{"command": "ls"}})
	if res.Err == nil {
		t.Error("denied bash must fail")
	}
	// Another run without the subset is unaffected.
	reg2 := a.registryFor("ses-b", nil, a.WorkDir())
	tool2, _ := reg2.Get("bash")
	if tool2.(*gatedTool).perms != nil {
		t.Error("unconstrained run must have nil perms")
	}
}

// sharedOnlyTool is a tool that lives only in the shared registry, the shape
// MCP adapters have after connectMCP.
type sharedOnlyTool struct {
	name     string
	executed int
}

func (t *sharedOnlyTool) Name() string           { return t.name }
func (t *sharedOnlyTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *sharedOnlyTool) Execute(context.Context, core.ToolCall) core.ToolResult {
	t.executed++
	return core.ToolResult{Content: "remote ok"}
}

// TestRegistryForWrapsSharedTools: MCP adapters pass through the shared
// registry, but the run view must still gate them (approval, agent subset,
// audit) instead of letting them reach the server ungated.
func TestRegistryForWrapsSharedTools(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	shared := &sharedOnlyTool{name: "srv_echo"}
	if err := a.tools.Register(shared); err != nil {
		t.Fatal(err)
	}
	gate := &stubGate{approved: false, verdict: "high"}
	a.SetGate(gate)

	reg := a.registryFor("ses-mcp", nil, a.WorkDir())
	tool, ok := reg.Get("srv_echo")
	if !ok {
		t.Fatal("shared tool must appear in the run view")
	}
	gated, ok := tool.(*gatedTool)
	if !ok {
		t.Fatalf("run view tool = %T, want *gatedTool", tool)
	}
	if !gated.needsApproval {
		t.Error("MCP tools must require approval")
	}
	res := tool.Execute(context.Background(), core.ToolCall{Input: map[string]any{"q": "x"}})
	if res.Err == nil {
		t.Fatal("denied MCP call must error")
	}
	if shared.executed != 0 {
		t.Error("denied MCP call must never reach the server")
	}
	if len(gate.asks) != 1 || gate.asks[0].Action != "srv_echo" {
		t.Errorf("gate asks = %+v", gate.asks)
	}

	// The agent permission subset narrows MCP actions too.
	reg2 := a.registryFor("ses-mcp2", map[string]core.Effect{"srv_echo": core.EffectDeny}, a.WorkDir())
	tool2, _ := reg2.Get("srv_echo")
	res2 := tool2.Execute(context.Background(), core.ToolCall{Input: map[string]any{"q": "y"}})
	if res2.Err == nil {
		t.Error("agent deny subset must block the MCP tool")
	}
	if shared.executed != 0 {
		t.Error("denied MCP call must never reach the server")
	}
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chyan.tech/chyan/internal/config"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/llm"
	"chyan.tech/chyan/internal/secret"
	"chyan.tech/chyan/internal/store"
)

// TestMain forces the offline token counter before any test runs an agent
// loop, so tests never trigger the tiktoken BPE download.
func TestMain(m *testing.M) {
	llm.ForceHeuristicForTest()
	os.Exit(m.Run())
}

// stubBackend scripts LLM responses for app-level tests. Mutex-guarded: the
// concurrent-runs test drives several goroutines through Chat at once.
type stubBackend struct {
	mu          sync.Mutex
	responses   []core.Response
	calls       int
	lastRequest core.Request
	requests    []core.Request
}

func (s *stubBackend) Chat(_ context.Context, req core.Request) (core.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastRequest = req
	s.requests = append(s.requests, req)
	if len(s.responses) == 0 {
		return core.Response{Message: core.Message{Role: core.RoleAssistant,
			Content: []core.Part{{Type: "text", Text: "ok"}}}}, nil
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}

func (s *stubBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := s.Chat(ctx, req)
	if err == nil && onDelta != nil {
		for _, p := range r.Message.Content {
			onDelta(core.Delta{Text: p.Text})
		}
	}
	return r, err
}

func newTestApp(t *testing.T, backend core.LLMBackend) *App {
	t.Helper()
	a, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true,
		Home:           t.TempDir(),
		MemoryRoot:     t.TempDir(),
		WorkDir:        t.TempDir(),
		Backend:        backend,
		Store:          store.NewMemory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func textResp(text string) core.Response {
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: text}}}}
}

func TestNewWiresStack(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("hi")}})
	if a.WorkDir() == "" {
		t.Error("work dir must be set")
	}
	if a.Registry() == nil {
		t.Error("tool registry must exist")
	}
	// Built-in tools registered.
	for _, name := range []string{"read", "write", "edit", "bash", "remember", "recall", "read_session"} {
		if _, ok := a.Registry().Get(name); !ok {
			t.Errorf("built-in tool %q not registered", name)
		}
	}
}

func TestRunNewSession(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{textResp("answer")}}
	a := newTestApp(t, backend)
	msg, sid, err := a.Run(context.Background(), "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if sid == "" {
		t.Fatal("session id must be assigned")
	}
	var got string
	for _, p := range msg.Content {
		got += p.Text
	}
	if got != "answer" {
		t.Errorf("reply = %q", got)
	}
	// Both user and assistant messages persisted.
	recs, err := a.ReadSession(context.Background(), sid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Errorf("stored messages = %d, want 2 (user + assistant)", len(recs))
	}
}

func TestRunContinuesSession(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{textResp("one"), textResp("two")}}
	a := newTestApp(t, backend)
	_, sid, err := a.Run(context.Background(), "", "first")
	if err != nil {
		t.Fatal(err)
	}
	_, sid2, err := a.Run(context.Background(), sid, "second")
	if err != nil {
		t.Fatal(err)
	}
	if sid2 != sid {
		t.Errorf("session id changed: %q -> %q", sid, sid2)
	}
	recs, _ := a.ReadSession(context.Background(), sid, 10)
	if len(recs) != 4 {
		t.Errorf("stored messages = %d, want 4 (two turns)", len(recs))
	}
}

func TestRunContinueAlias(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{textResp("a"), textResp("b")}}
	a := newTestApp(t, backend)
	_, sid, err := a.Run(context.Background(), "", "first")
	if err != nil {
		t.Fatal(err)
	}
	// "continue" resolves to the most recent session.
	_, sid2, err := a.Run(context.Background(), "continue", "again")
	if err != nil {
		t.Fatal(err)
	}
	if sid2 != sid {
		t.Errorf("continue resolved to %q, want %q", sid2, sid)
	}
}

func TestRunUnknownSession(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	if _, _, err := a.Run(context.Background(), "nope", "x"); err == nil {
		t.Error("unknown session must error")
	}
}

func TestMessageRoundTripKeepsToolCalls(t *testing.T) {
	// Regression: tool_calls/tool_call_id must round-trip, else replayed
	// history has orphaned tool results and compression can never fire.
	msg := core.Message{
		Role:       core.RoleAssistant,
		Content:    []core.Part{{Type: "text", Text: "calling"}},
		ToolCalls:  []core.ToolCall{{ID: "c1", Name: "bash", Input: map[string]any{"cmd": "ls"}}},
		ToolCallID: "",
		Timestamp:  time.Now().UTC(),
	}
	raw := marshalMessage(msg)
	got := unmarshalMessage(string(core.RoleAssistant), raw, msg.Timestamp)
	if len(got.ToolCalls) != 1 {
		t.Fatalf("tool calls round trip = %d, want 1", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Name != "bash" || got.ToolCalls[0].Input["cmd"] != "ls" {
		t.Errorf("tool call = %+v", got.ToolCalls[0])
	}

	result := core.Message{Role: core.RoleTool, ToolCallID: "c1",
		Content: []core.Part{{Type: "text", Text: "out"}}, Timestamp: time.Now().UTC()}
	raw = marshalMessage(result)
	got = unmarshalMessage(string(core.RoleTool), raw, result.Timestamp)
	if got.ToolCallID != "c1" {
		t.Errorf("tool call id = %q", got.ToolCallID)
	}
}

func TestCorruptBlobTolerated(t *testing.T) {
	got := unmarshalMessage("user", []byte("{not json"), time.Now().UTC())
	if got.Role != core.RoleUser {
		t.Errorf("role = %v", got.Role)
	}
}

func TestRememberRecallRoundTrip(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	id, err := a.Remember(context.Background(), core.KindFact, "truncate-tool-output",
		[]string{"truncation"}, []string{"tool output 落库"}, "always truncate tool output")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("remember must return an id")
	}
	hits, err := a.Recall(context.Background(), "truncation tool output", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("recall must find the stored memory")
	}
	if hits[0].Name != "truncate-tool-output" {
		t.Errorf("hit = %+v", hits[0])
	}
	// Triggered entries are procedural lessons.
	if hits[0].Triggers[0] != "tool output 落库" {
		t.Errorf("triggers = %v", hits[0].Triggers)
	}
}

func TestMemoryKindRouting(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	// Four kinds round-trip and filter (FR-7).
	if _, err := a.Remember(context.Background(), core.KindDirective, "be-concise",
		nil, nil, "always be concise"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Remember(context.Background(), core.KindFact, "some-fact",
		[]string{"concise"}, nil, "a fact about concise"); err != nil {
		t.Fatal(err)
	}
	// Unknown kind falls back to fact.
	if _, err := a.Remember(context.Background(), "bogus", "fallback",
		[]string{"concise"}, nil, "bogus kind becomes fact"); err != nil {
		t.Fatal(err)
	}
	all, err := a.Recall(context.Background(), "concise", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("all kinds recall = %d, want >= 3", len(all))
	}
	onlyDirective, err := a.Recall(context.Background(), "concise", core.KindDirective, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range onlyDirective {
		if h.Kind != core.KindDirective {
			t.Errorf("kind filter leaked %q: %+v", h.Kind, h)
		}
	}
	if len(onlyDirective) == 0 {
		t.Error("directive kind must be recallable")
	}
}

func TestRecallEmpty(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	hits, err := a.Recall(context.Background(), "nothing here", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0", len(hits))
	}
}

func TestStatusReflectsHistory(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{textResp("a"), textResp("b")}}
	a := newTestApp(t, backend)
	_, sid, err := a.Run(context.Background(), "", "hello world")
	if err != nil {
		t.Fatal(err)
	}
	b, err := a.Status(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if b.ContextLimit <= 0 {
		t.Errorf("context limit = %d", b.ContextLimit)
	}
	if b.HistoryTokens <= 0 {
		t.Errorf("history tokens = %d", b.HistoryTokens)
	}
}

func TestCompactProducesNote(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{textResp("done")}}
	a := newTestApp(t, backend)
	_, sid, err := a.Run(context.Background(), "", "please refactor auth")
	if err != nil {
		t.Fatal(err)
	}
	note, err := a.Compact(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if note.Intent == "" {
		t.Error("compact must capture an intent")
	}
}

func TestWriteHandoffPersistsKV(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	note := core.HandoffNote{Intent: "goal", Decisions: []string{"use Go"}}
	if err := a.WriteHandoff(context.Background(), "ses1", note); err != nil {
		t.Fatal(err)
	}
	raw, err := a.store.GetKV(context.Background(), "handoff:ses1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "use Go") {
		t.Errorf("handoff kv = %s", raw)
	}
}

func TestLoadAgentPrompt(t *testing.T) {
	workDir := t.TempDir()
	a, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true,
		Home:           t.TempDir(), MemoryRoot: t.TempDir(), WorkDir: workDir,
		Backend: &stubBackend{}, Store: store.NewMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.loadAgentPrompt("ghost"); err == nil {
		t.Error("missing agent must error")
	}
	// A project agent definition supplies the system prompt body.
	agents := filepath.Join(workDir, ".agents", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	def := "---\ndescription: explorer\n---\nYou are a read-only explorer."
	if err := os.WriteFile(filepath.Join(agents, "explorer.md"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := a.loadAgentPrompt("explorer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "read-only explorer") {
		t.Errorf("body = %q", body)
	}
	// SetAgent applies the definition as the system prompt and resets cleanly.
	if err := a.SetAgent("explorer"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.systemPrompt(), "read-only explorer") {
		t.Errorf("system prompt = %q", a.systemPrompt())
	}
	if err := a.SetAgent("ghost"); err == nil {
		t.Error("SetAgent on missing definition must error")
	}
	if err := a.SetAgent(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.systemPrompt(), "chyan") {
		t.Errorf("default prompt not restored: %q", a.systemPrompt())
	}
}

func TestNewIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id, err := newID("msg")
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "msg") {
			t.Errorf("id prefix = %q", id)
		}
	}
}

func TestLessonType(t *testing.T) {
	if got := lessonType([]string{"x"}); got != "procedural" {
		t.Errorf("with triggers = %q", got)
	}
	if got := lessonType(nil); got != "semantic" {
		t.Errorf("no triggers = %q", got)
	}
}

func TestMirrorWritesMemory(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	if err := a.Mirror(context.Background(), "decision", "use Go not Rust"); err != nil {
		t.Fatal(err)
	}
	hits, err := a.Recall(context.Background(), "Go Rust decision", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("mirrored decision must be retrievable")
	}
}

func TestBuildBackendFromConfig(t *testing.T) {
	// Unwired New() exercises the config path: secret resolution + router.
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.toml")
	cfg := "[llm]\nbase_url = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\napi_key = \"secret:llm_key\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// Seed the secret so ResolveSecrets succeeds.
	sec := secret.New(filepath.Join(home, "secret"))
	if err := sec.Put("llm_key", "sk-test"); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true, Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(), ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Registry() == nil {
		t.Error("stack must be wired from config")
	}
}

func TestNewErrors(t *testing.T) {
	home := t.TempDir()
	// Missing base_url: buildBackend must fail.
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[llm]\nmodel = \"m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true, Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(), ConfigPath: cfgPath}); err == nil {
		t.Error("missing base_url must fail")
	}
	// Bad config file: Load must fail.
	if err := os.WriteFile(cfgPath, []byte("{not toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true, Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(), ConfigPath: cfgPath}); err == nil {
		t.Error("malformed config must fail")
	}
}

// TestChannelAssemblyEndToEnd wires a [channels.feishu] entry against an
// httptest feishu API and delivers a cron job output through App.Deliver:
// token fetch + im/v1/messages send + secret reference resolution.
func TestChannelAssemblyEndToEnd(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"tok-1","expire":7200}`))
		case "/open-apis/im/v1/messages":
			gotAuth = r.Header.Get("Authorization")
			gotBody = string(body)
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.toml")
	cfg := "[llm]\nbase_url = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\n" +
		"[channels.feishu]\ntype = \"feishu\"\napp_id = \"cli_x\"\n" +
		"app_secret = \"secret:fs\"\nreceive_id_type = \"chat_id\"\n" +
		"base_url = \"" + srv.URL + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	sec := secret.New(filepath.Join(home, "secret"))
	if err := sec.Put("fs", "shh"); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(), ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if a.Channels() == nil {
		t.Fatal("channels registry must be wired")
	}
	if err := a.Deliver(context.Background(), "feishu:oc_abc", "job done"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if gotAuth != "Bearer tok-1" {
		t.Errorf("auth = %q", gotAuth)
	}
	for _, want := range []string{"oc_abc", "job done"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("send body missing %q: %s", want, gotBody)
		}
	}
	// Without any channel configured, delivery fails cleanly.
	bare := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	if err := bare.Deliver(context.Background(), "feishu:x", "t"); err == nil {
		t.Error("Deliver without channels must error")
	}
}

// TestBuildChannelUnknownType rejects unsupported adapters; disabled
// channels are never built.
func TestBuildChannelUnknownType(t *testing.T) {
	if _, err := buildChannel("x", config.Channel{Type: "slack"}, nil); err == nil {
		t.Error("unknown type must error")
	}
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.toml")
	cfg := "[llm]\nbase_url = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\n" +
		"[channels.off]\ntype = \"feishu\"\ndisabled = true\n" +
		"[channels.bad]\ntype = \"pigeon\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(), ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Both entries skipped: disabled one and the unknown-type one. The
	// registry still exists but delivery has no usable channel.
	if err := a.Deliver(context.Background(), "feishu:oc_x", "t"); err == nil {
		t.Error("no usable channel must make Deliver fail")
	}
}

// failingStore errors on every operation to exercise app error branches.
type failingStore struct{ store.Store }

func (failingStore) CreateSession(context.Context, store.Session) error {
	return errBoom
}
func (failingStore) GetSession(context.Context, string) (store.Session, error) {
	return store.Session{}, errBoom
}
func (failingStore) ListSessions(context.Context, int, int) ([]store.Session, error) {
	return nil, errBoom
}
func (failingStore) ListMessages(context.Context, string, int, int) ([]store.Message, error) {
	return nil, errBoom
}
func (failingStore) AppendMessage(context.Context, store.Message) error { return errBoom }

var errBoom = errors.New("store down")

func TestStoreErrorBranches(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	a.store = failingStore{store.NewMemory()}
	ctx := context.Background()
	if _, _, err := a.Run(ctx, "", "x"); err == nil {
		t.Error("Run must fail when the store is down")
	}
	if _, _, err := a.Run(ctx, "continue", "x"); err == nil {
		t.Error("Run(continue) must fail when the store is down")
	}
	if _, err := a.Status(ctx, "s"); err == nil {
		t.Error("Status must fail when the store is down")
	}
	if _, err := a.Compact(ctx, "s"); err == nil {
		t.Error("Compact must fail when the store is down")
	}
	if err := a.AppendMessage(ctx, core.Message{}); err == nil {
		t.Error("AppendMessage must fail when the store is down")
	}
	if _, err := a.ReadSession(ctx, "s", 0); err == nil {
		t.Error("ReadSession must fail when the store is down")
	}
	if _, err := a.Recall(ctx, "x", "", 0); err == nil {
		// Recall hits memory, not the store: must not fail here.
	} else {
		t.Errorf("Recall must not touch the store: %v", err)
	}
}

func TestResolveSessionContinueEmpty(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	// continue on an empty store starts a fresh session.
	sid, err := a.resolveSession(context.Background(), "continue")
	if err != nil {
		t.Fatal(err)
	}
	if sid == "" {
		t.Error("continue on empty store must create a session")
	}
}

// stubGate implements ToolGate for approval tests.
type stubGate struct {
	approved bool
	verdict  string
	err      error
	asks     []core.ApprovalRequest
}

func (g *stubGate) Ask(_ context.Context, req core.ApprovalRequest) (bool, string, error) {
	g.asks = append(g.asks, req)
	return g.approved, g.verdict, g.err
}

func TestGateDeniesDangerousTool(t *testing.T) {
	gate := &stubGate{approved: false, verdict: "high"}
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	a.SetGate(gate)

	tool, ok := a.Registry().Get("bash")
	if !ok {
		t.Fatal("bash tool not registered")
	}
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"command": "rm -rf /"},
	})
	if res.Err == nil {
		t.Fatal("denied operation must error")
	}
	if !strings.Contains(res.Err.Error(), "denied") {
		t.Errorf("err = %v", res.Err)
	}
	// The audit trail must record the denial.
	audits, err := a.store.ListAudit(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) == 0 {
		t.Fatal("denial must be audited")
	}
	found := false
	for _, e := range audits {
		if e.Action == "bash" && !e.Approved && e.Verdict == "high" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit = %+v", audits)
	}
}

func TestGateAllowsApprovedTool(t *testing.T) {
	gate := &stubGate{approved: true, verdict: "low"}
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	a.SetGate(gate)

	tool, _ := a.Registry().Get("write")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"path": "a.txt", "content": "hello"},
	})
	if res.Err != nil {
		t.Fatalf("approved write failed: %v", res.Err)
	}
	if len(gate.asks) != 1 {
		t.Errorf("gate asks = %v", gate.asks)
	}
}

// TestSessionDetailReturnsBudget: the session view loads history once and
// returns both the transcript slice and its live window budget.
func TestSessionDetailReturnsBudget(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("hi")}})
	_, sid, err := a.Run(context.Background(), "", "hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	msgs, budget, err := a.SessionDetail(context.Background(), sid, 10)
	if err != nil {
		t.Fatalf("SessionDetail: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("transcript is empty")
	}
	if budget.ContextLimit <= 0 || budget.HistoryTokens <= 0 {
		t.Fatalf("budget = %+v", budget)
	}
}

// TestApprovalTimeoutStopsRun: an unanswered ask aborts the turn instead of
// feeding a denied tool result back to the model.
func TestApprovalTimeoutStopsRun(t *testing.T) {
	gate := &stubGate{err: core.ErrApprovalTimeout}
	backend := &stubBackend{responses: []core.Response{{
		Message: core.Message{
			Role:    core.RoleAssistant,
			Content: []core.Part{{Type: "text", Text: "run"}},
			ToolCalls: []core.ToolCall{{
				ID: "c1", Name: "bash", Input: map[string]any{"command": "ls"},
			}},
		},
	}, textResp("should not be requested")}}
	a := newTestApp(t, backend)
	a.SetGate(gate)

	_, _, err := a.Run(context.Background(), "", "go")
	if !errors.Is(err, core.ErrApprovalTimeout) {
		t.Fatalf("run err = %v, want ErrApprovalTimeout", err)
	}
	if backend.calls != 1 {
		t.Fatalf("backend calls = %d, want 1 (turn aborted after the ask)", backend.calls)
	}
}

// TestGateRequestCarriesSession: the ask is tied to its conversation so the
// PWA can render the pending approval inline in the transcript.
func TestGateRequestCarriesSession(t *testing.T) {
	gate := &stubGate{approved: true, verdict: "low"}
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	a.SetGate(gate)

	tool, ok := a.registryFor("sess-1", nil, a.workDir).Get("bash")
	if !ok {
		t.Fatal("bash tool not registered")
	}
	tool.Execute(context.Background(), core.ToolCall{Input: map[string]any{"command": "ls"}})
	if len(gate.asks) != 1 || gate.asks[0].SessionID != "sess-1" {
		t.Fatalf("gate asks = %+v, want session sess-1", gate.asks)
	}
}

func TestReadOnlyToolsBypassGate(t *testing.T) {
	gate := &stubGate{approved: false, verdict: "high"}
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	a.SetGate(gate)

	// read/list/grep/recall never ask; a read must succeed even with a
	// denying gate (read-only is not a dangerous operation).
	tool, _ := a.Registry().Get("read")
	res := tool.Execute(context.Background(), core.ToolCall{Input: map[string]any{"path": "a.txt"}})
	if res.Err != nil && strings.Contains(res.Err.Error(), "denied") {
		t.Errorf("read must bypass the gate: %v", res.Err)
	}
	if len(gate.asks) != 0 {
		t.Errorf("read-only tools must not ask the gate: %v", gate.asks)
	}
}

func TestGateErrorSurfaces(t *testing.T) {
	gate := &stubGate{err: errors.New("reviewer down")}
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	a.SetGate(gate)
	tool, _ := a.Registry().Get("bash")
	res := tool.Execute(context.Background(), core.ToolCall{Input: map[string]any{"command": "ls"}})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "approval error") {
		t.Errorf("gate error must surface: %v", res.Err)
	}
}

func TestNoGateMeansPassThrough(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	// No gate installed: tools behave exactly as in P2.
	tool, _ := a.Registry().Get("write")
	res := tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"path": "b.txt", "content": "x"},
	})
	if res.Err != nil {
		t.Fatalf("ungated write failed: %v", res.Err)
	}
}

func TestResourceAndDetailHelpers(t *testing.T) {
	call := core.ToolCall{Name: "bash", Input: map[string]any{"command": "ls -l"}}
	if got := resourceOf(call); got != "ls -l" {
		t.Errorf("resourceOf = %q", got)
	}
	if got := detailOf(call); !strings.Contains(got, "command=ls -l") {
		t.Errorf("detailOf = %q", got)
	}
	// No known key falls back to the tool name.
	if got := resourceOf(core.ToolCall{Name: "x", Input: map[string]any{"zzz": 1}}); got != "x" {
		t.Errorf("resourceOf fallback = %q", got)
	}
}

func TestTaskToolRegisteredAndDispatches(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	tool, ok := a.Registry().Get("task")
	if !ok {
		t.Fatal("task tool must be registered (architecture 4.1.1)")
	}
	// Missing prompt is rejected before any dispatch.
	res := tool.Execute(context.Background(), core.ToolCall{Input: map[string]any{}})
	if res.Err == nil {
		t.Error("task without prompt must fail")
	}
	// A dispatch creates a child session linked to the parent (empty parent
	// here) and reports only the summary.
	res = tool.Execute(context.Background(), core.ToolCall{
		Input: map[string]any{"prompt": "do something"},
	})
	if res.Err != nil {
		t.Fatalf("task dispatch failed: %v", res.Err)
	}
	if !strings.Contains(res.Content, "子 Agent 结果") {
		t.Errorf("content = %q", res.Content)
	}
	sessions, err := a.store.ListSessions(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundChild := false
	for _, s := range sessions {
		if s.ParentID == "" && s.Agent == "" && s.ID != "" {
			// the dispatch's child session exists with a real id
			foundChild = true
		}
	}
	if !foundChild {
		t.Errorf("child session not persisted: %+v", sessions)
	}
}

func TestSessionSinkIsolatesSessions(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	s1 := &sessionSink{app: a, sessionID: "ses-one"}
	s2 := &sessionSink{app: a, sessionID: "ses-two"}
	msg := core.Message{Role: core.RoleAssistant, Content: []core.Part{{Type: "text", Text: "hi"}}}
	if err := s1.AppendMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := s2.AppendMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	one, err := a.store.ListMessages(context.Background(), "ses-one", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	two, err := a.store.ListMessages(context.Background(), "ses-two", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || len(two) != 1 {
		t.Errorf("sessions must not share messages: one=%d two=%d", len(one), len(two))
	}
	if one[0].SessionID != "ses-one" || two[0].SessionID != "ses-two" {
		t.Errorf("wrong session routing: %q %q", one[0].SessionID, two[0].SessionID)
	}
}

// TestBuildBackendFallbackChain wires [[llm.fallback]] entries into the
// router as ordered chain specs (FR-1).
func TestBuildBackendFallbackChain(t *testing.T) {
	cfg := config.Config{LLM: config.LLM{
		BaseURL: "https://primary.example/v1", Model: "main-model",
		Fallback: []config.LLMEndpoint{
			{BaseURL: "https://backup.example/v1", APIKey: "k", Model: "backup-model"},
			{BaseURL: "", Model: "broken-entry"}, // skipped, never widens
		},
	}}
	backend, err := buildBackend(cfg, secret.New(t.TempDir()))
	if err != nil {
		t.Fatalf("buildBackend: %v", err)
	}
	r, ok := backend.(*llm.Router)
	if !ok {
		t.Fatalf("backend type = %T", backend)
	}
	specs := r.Specs()
	if len(specs) != 2 {
		t.Fatalf("chain specs = %d, want 2 (broken entry skipped)", len(specs))
	}
	if specs[0].Provider != "primary" || specs[0].Model != "main-model" {
		t.Errorf("primary spec = %+v", specs[0])
	}
	if specs[1].Model != "backup-model" {
		t.Errorf("fallback spec = %+v", specs[1])
	}
}

// TestWindowResolutionWired checks the FR-1 layered window reaches the
// context manager: a pinned context_limit must win over the fallback default.
func TestWindowResolutionWired(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.toml")
	body := "[llm]\nbase_url = \"https://api.example/v1\"\nmodel = \"m\"\n[context]\ncontext_limit = 200000\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		ConfigPath: cfgPath, Backend: &stubBackend{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, sid, err := a.Run(context.Background(), "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	b, err := a.Status(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if b.ContextLimit != 200000 {
		t.Errorf("context limit = %d, want 200000 (config layer)", b.ContextLimit)
	}
}

// TestRunFallsBackToBackupProvider drives the real chain: the primary
// endpoint 500s and the fallback answers (FR-1).
func TestRunFallsBackToBackupProvider(t *testing.T) {
	var calls int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "backup-model",
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "from backup"}}},
		})
	}))
	defer live.Close()
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.toml")
	body := fmt.Sprintf("[llm]\nbase_url = %q\nmodel = \"main-model\"\n\n[[llm.fallback]]\nbase_url = %q\nmodel = \"backup-model\"\n", dead.URL, live.URL)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		ConfigPath: cfgPath, Store: store.NewMemory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, sid, err := a.Run(context.Background(), "", "hello")
	if err != nil {
		t.Fatalf("run must succeed via fallback: %v", err)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Error("primary must have been tried first")
	}
	_ = sid
}

// TestConcurrentRunsDoNotCrossSessions: five concurrent RunStream calls must
// each persist their user message in their own session. Before the fix, all
// went through the shared a.currentSession and landed in one session (the
// daemon's background tasks then answered someone else's prompt).
func TestConcurrentRunsDoNotCrossSessions(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{
		textResp("a"), textResp("b"), textResp("c"), textResp("d"), textResp("e"),
	}}
	a := newTestApp(t, backend)
	const n = 5
	var wg sync.WaitGroup
	sids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, sid, err := a.RunStream(context.Background(), "",
				fmt.Sprintf("prompt-%d", i), nil)
			if err != nil {
				t.Errorf("run %d: %v", i, err)
				return
			}
			sids[i] = sid
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, sid := range sids {
		if sid == "" {
			t.Fatalf("run %d has no session", i)
		}
		if seen[sid] {
			t.Fatalf("duplicate session id %q across concurrent runs", sid)
		}
		seen[sid] = true
		msgs, err := a.ReadSession(context.Background(), sid, 10)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range msgs {
			for _, p := range m.Content {
				if p.Text == fmt.Sprintf("prompt-%d", i) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("session %s lost its own prompt-%d (got %+v)", sid, i, msgs)
		}
	}
}

// TestRunTitlesFirstTurn: after the first turn the session gets an LLM
// generated title, requested with reasoning off (a title never pays for
// thinking).
func TestRunTitlesFirstTurn(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{
		textResp("answer"), textResp("“修复登录问题”"),
	}}
	a := newTestApp(t, backend)
	_, sid, err := a.Run(context.Background(), "", "fix the login bug please")
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.store.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "修复登录问题" {
		t.Errorf("title = %q, want %q (quotes stripped)", s.Title, "修复登录问题")
	}
	if backend.lastRequest.ReasoningEffort != "none" {
		t.Errorf("title call reasoning_effort = %q, want none", backend.lastRequest.ReasoningEffort)
	}
}

// TestRunTitlesOnlyFirstTurn: later turns never rename the session — the
// title is decided once, at the end of the opening turn.
func TestRunTitlesOnlyFirstTurn(t *testing.T) {
	backend := &stubBackend{responses: []core.Response{
		textResp("one"), textResp("初轮标题"), textResp("two"), textResp("不该出现"),
	}}
	a := newTestApp(t, backend)
	_, sid, err := a.Run(context.Background(), "", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Run(context.Background(), sid, "second"); err != nil {
		t.Fatal(err)
	}
	s, err := a.store.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "初轮标题" {
		t.Errorf("title = %q, want %q (second turn must not rename)", s.Title, "初轮标题")
	}
}

// TestTitleClampedTo20Runes: an over-long model reply is cut to the 20-char
// budget (rune-safe for CJK titles).
func TestTitleClampedTo20Runes(t *testing.T) {
	long := strings.Repeat("长", 35)
	backend := &stubBackend{responses: []core.Response{
		textResp("ok"), textResp(long),
	}}
	a := newTestApp(t, backend)
	_, sid, err := a.Run(context.Background(), "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.store.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if r := []rune(s.Title); len(r) != 20 {
		t.Errorf("title length = %d runes, want 20 (%q)", len(r), s.Title)
	}
	if got := clampRunes("abcdef", 3); got != "abc" {
		t.Errorf("clampRunes = %q, want abc", got)
	}
}

// TestSessionUsageRecordsModelAndCost: session rows must carry the effective
// model and the billed counters; otherwise listings and exports show a blank
// model and zero cost even after a real run.
func TestSessionUsageRecordsModelAndCost(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("ok")}})
	a.cfg.LLM.Model = "cfg-model"
	a.SetModel("run-model")
	sid, err := a.resolveSession(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.store.GetSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.Model != "run-model" {
		t.Errorf("created model = %q, want the per-run override", s.Model)
	}

	if err := a.addSessionUsage(ctx, sid, core.ProgressEvent{
		Model: "actual-model", TokensIn: 100, TokensOut: 20,
		CacheReadTokens: 80, ReasoningTokens: 5, CostUSD: 0.25,
	}); err != nil {
		t.Fatal(err)
	}
	s, err = a.store.GetSession(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.Model != "actual-model" || s.TokensIn != 100 || s.TokensOut != 20 ||
		s.CacheReadTokens != 80 || s.ReasoningTokens != 5 || s.CostUSD != 0.25 {
		t.Errorf("usage row = %+v", s)
	}
}

// TestTitleFailureKeepsFallback: a failed or empty title call must leave the
// title untouched — listings fall back to the first user message.
func TestTitleFailureKeepsFallback(t *testing.T) {
	a := newTestApp(t, errBackend{})
	sid, err := a.resolveSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	a.titleSession(context.Background(), sid, "hello",
		core.Message{Role: core.RoleAssistant, Content: []core.Part{{Type: "text", Text: "hi"}}})
	s, err := a.store.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "" {
		t.Errorf("title = %q, want empty after failed title call", s.Title)
	}

	// An empty model reply is equally ignored.
	b := &stubBackend{responses: []core.Response{textResp("   ")}}
	a2 := newTestApp(t, b)
	sid2, err := a2.resolveSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	a2.titleSession(context.Background(), sid2, "hello",
		core.Message{Role: core.RoleAssistant, Content: []core.Part{{Type: "text", Text: "hi"}}})
	s2, err := a2.store.GetSession(context.Background(), sid2)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Title != "" {
		t.Errorf("title = %q, want empty after blank title reply", s2.Title)
	}
}

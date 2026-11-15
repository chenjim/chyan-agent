package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

// mockLLMServer is a minimal OpenAI-compatible endpoint that scripts one
// tool call then a final answer. This exercises the REAL llm.OpenAIClient →
// app → tools → store chain (stubBackend tests skip that whole path).
type mockLLMServer struct {
	toolName string
	toolArgs map[string]any
	final    string
	calls    int
}

func (m *mockLLMServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.calls++
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools  []any `json:"tools"`
		Stream bool  `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// First call: request the tool. Later calls (tool results present): finish.
	hasToolResult := false
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			hasToolResult = true
		}
	}

	// Tool-less request = the session-title pass: reply plain text only.
	if len(req.Tools) == 0 {
		reply := map[string]any{
			"model": "mock-model",
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": "mock title"},
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3},
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range splitChunks("mock title") {
				frame, _ := json.Marshal(map[string]any{
					"choices": []map[string]any{{"delta": map[string]any{"content": chunk}}},
				})
				_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
		return
	}

	var reply map[string]any
	if !hasToolResult {
		args, _ := json.Marshal(m.toolArgs)
		reply = map[string]any{
			"model": "mock-model",
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant", "content": "",
					"tool_calls": []map[string]any{{
						"id": "call-1", "type": "function",
						"function": map[string]any{"name": m.toolName, "arguments": string(args)},
					}},
				},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		}
	} else {
		reply = map[string]any{
			"model": "mock-model",
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": m.final},
			}},
			"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 5},
		}
	}
	// Streaming requests expect SSE delta frames; non-streaming gets plain
	// JSON. The two shapes differ (message vs delta), mirroring real APIs.
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		// Tool-call turn: stream the tool_call delta (parseSSE aggregates).
		if !hasToolResult {
			args, _ := json.Marshal(m.toolArgs)
			frame, _ := json.Marshal(map[string]any{
				"model": "mock-model",
				"choices": []map[string]any{{"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": 0, "id": "call-1", "type": "function",
						"function": map[string]any{"name": m.toolName, "arguments": string(args)},
					}},
				}}},
			})
			_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		// Text turn: stream content in small chunks to exercise aggregation.
		for _, chunk := range splitChunks(m.final) {
			frame, _ := json.Marshal(map[string]any{
				"choices": []map[string]any{{"delta": map[string]any{"content": chunk}}},
			})
			_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// splitChunks splits text into small deltas to exercise the aggregator.
func splitChunks(s string) []string {
	if s == "" {
		return nil
	}
	const size = 4
	var out []string
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

// startMockLLM boots the scripted OpenAI-compatible server.
func startMockLLM(t *testing.T, m *mockLLMServer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return srv
}

// configFor writes a config.toml pointing at the mock LLM server.
func configFor(t *testing.T, home, baseURL string) string {
	t.Helper()
	cfg := "[llm]\nbase_url = \"" + baseURL + "\"\nmodel = \"mock-model\"\napi_key = \"\"\n"
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestE2ERealLLMClientChain is the critical-path acceptance test (P2 exit):
// fake LLM over HTTP + real filesystem. It covers the llm.OpenAIClient
// request/response path that stubBackend tests cannot see.
func TestE2ERealLLMClientChain(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	mock := &mockLLMServer{
		toolName: "bash",
		toolArgs: map[string]any{"command": "echo e2e-ok > e2e.txt"},
		final:    "task complete: wrote e2e.txt",
	}
	srv := startMockLLM(t, mock)
	cfgPath := configFor(t, home, srv.URL)

	a, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true,
		Home:           home, MemoryRoot: t.TempDir(), WorkDir: workDir,
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// The agent must drive: LLM → bash tool → real file → final reply.
	msg, sid, err := a.Run(context.Background(), "", "write e2e file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := text(msg.Content); !strings.Contains(got, "task complete") {
		t.Errorf("reply = %q", got)
	}
	if sid == "" {
		t.Error("session id missing")
	}
	// The bash tool must have really written the file.
	data, err := os.ReadFile(filepath.Join(workDir, "e2e.txt"))
	if err != nil {
		t.Fatalf("tool output file missing: %v", err)
	}
	if !strings.Contains(string(data), "e2e-ok") {
		t.Errorf("file content = %q", data)
	}
	// Two loop calls (tool request + follow-up) plus the title pass prove the
	// whole chain ran — and the title landed in the store.
	if mock.calls != 3 {
		t.Errorf("LLM calls = %d, want 3 (2 loop + 1 title)", mock.calls)
	}
	if s, err := a.store.GetSession(context.Background(), sid); err != nil || s.Title != "mock title" {
		t.Errorf("session title = %q (err %v), want %q", s.Title, err, "mock title")
	}
	// The transcript must persist both turns (user + assistant + tool).
	recs, err := a.ReadSession(context.Background(), sid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 3 {
		t.Errorf("stored messages = %d, want >= 3", len(recs))
	}
}

// TestE2ECompactExtractsIntent drives a real turn then compacts, asserting
// the handoff note captures the user intent (the bug class found in P2
// acceptance: missing role column broke intent extraction).
func TestE2ECompactExtractsIntent(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	mock := &mockLLMServer{
		toolName: "bash",
		toolArgs: map[string]any{"command": "echo x > out.txt"},
		final:    "done",
	}
	srv := startMockLLM(t, mock)
	cfgPath := configFor(t, home, srv.URL)

	a, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true,
		Home:           home, MemoryRoot: t.TempDir(), WorkDir: workDir,
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	_, sid, err := a.Run(context.Background(), "", "please refactor the auth module")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	note, err := a.Compact(context.Background(), sid)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if note.Intent != "please refactor the auth module" {
		t.Errorf("intent = %q", note.Intent)
	}
}

// TestE2ESessionContinuity runs two turns against one session and asserts
// the second turn sees the first (history replay through the store).
func TestE2ESessionContinuity(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	mock := &mockLLMServer{
		toolName: "bash",
		toolArgs: map[string]any{"command": "echo a > a.txt"},
		final:    "done",
	}
	srv := startMockLLM(t, mock)
	cfgPath := configFor(t, home, srv.URL)

	a, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true,
		Home:           home, MemoryRoot: t.TempDir(), WorkDir: workDir,
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	_, sid, err := a.Run(context.Background(), "", "first turn")
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	_, sid2, err := a.Run(context.Background(), sid, "second turn")
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if sid2 != sid {
		t.Fatalf("session changed: %q -> %q", sid, sid2)
	}
	recs, err := a.ReadSession(context.Background(), sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	// 2 turns x (user + assistant w/ tool_calls + tool result + assistant final)
	if len(recs) < 6 {
		t.Errorf("stored messages = %d, want >= 6 (two full turns)", len(recs))
	}
}

// TestE2EMCPDegradesGracefully boots with an unreachable MCP server and
// asserts startup still succeeds with built-in tools intact (architecture
// 4.8: one server's failure must not block the others).
func TestE2EMCPDegradesGracefully(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	mock := &mockLLMServer{toolName: "bash", toolArgs: map[string]any{"command": "echo hi > hi.txt"}, final: "ok"}
	srv := startMockLLM(t, mock)
	cfgPath := configFor(t, home, srv.URL)

	// Append an unreachable remote MCP server to the config.
	cfg := "[llm]\nbase_url = \"" + srv.URL + "\"\nmodel = \"mock-model\"\napi_key = \"\"\n" +
		"[mcp.servers.ghost]\ntype = \"remote\"\nurl = \"http://127.0.0.1:1/mcp\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true,
		Home:           home, MemoryRoot: t.TempDir(), WorkDir: workDir,
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("New must not fail on an unreachable MCP server: %v", err)
	}
	defer a.Close()

	// Built-in tools must still be registered and the agent runnable.
	if _, ok := a.Registry().Get("bash"); !ok {
		t.Error("built-in tools must survive MCP degradation")
	}
	if _, _, err := a.Run(context.Background(), "", "run anyway"); err != nil {
		t.Errorf("Run after MCP degradation: %v", err)
	}
}

// TestE2ERunStreamForwardsDeltas verifies live token streaming reaches the
// sink (TUI/PWA realtime output, architecture 4.7).
func TestE2ERunStreamForwardsDeltas(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	mock := &mockLLMServer{
		toolName: "bash",
		toolArgs: map[string]any{"command": "echo s > s.txt"},
		final:    "streamed reply",
	}
	srv := startMockLLM(t, mock)
	cfgPath := configFor(t, home, srv.URL)

	a, err := New(Options{
		SkipGate:       true,
		SkipExtensions: true,
		Home:           home, MemoryRoot: t.TempDir(), WorkDir: workDir,
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	var got []string
	_, _, err = a.RunStream(context.Background(), "", "go", func(text string) {
		got = append(got, text)
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	// The final reply must have streamed through the sink as a delta.
	if joined := strings.Join(got, ""); !strings.Contains(joined, "streamed reply") {
		t.Errorf("stream deltas = %q", got)
	}
}

// TestE2ESkillsAndSlashCommands covers the extension surfaces: skills list
// in the system prompt and slash-command expansion in the user input.
func TestE2ESkillsAndSlashCommands(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	mock := &mockLLMServer{toolName: "bash", toolArgs: map[string]any{"command": "echo x > x.txt"}, final: "ok"}
	srv := startMockLLM(t, mock)
	cfgPath := configFor(t, home, srv.URL)

	// Seed a project skill and a slash command under .agents/.
	skillDir := filepath.Join(workDir, ".agents", "skills", "explorer")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: explorer\ndescription: read-only code exploration\n---\nExplore code read-only."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o600); err != nil {
		t.Fatal(err)
	}
	cmdDir := filepath.Join(workDir, ".agents", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmdMD := "---\ndescription: review a change\n---\nReview this change: $ARGUMENTS"
	if err := os.WriteFile(filepath.Join(cmdDir, "review.md"), []byte(cmdMD), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := New(Options{
		SkipGate:      true,
		ExtensionsDir: filepath.Join(workDir, ".agents"),
		Home:          home, MemoryRoot: t.TempDir(), WorkDir: workDir,
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// Skill inventory appears in the system prompt.
	prompt := a.systemPrompt()
	if !strings.Contains(prompt, "explorer") || !strings.Contains(prompt, "read-only code exploration") {
		t.Errorf("system prompt missing skill inventory: %q", prompt)
	}

	// Slash command expands to its template with $ARGUMENTS substituted.
	expanded, err := a.expandCommand("/review the auth change")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expanded, "Review this change: the auth change") {
		t.Errorf("expanded = %q", expanded)
	}
	// Unknown slash and plain input pass through unchanged.
	if got, _ := a.expandCommand("/nope foo"); got != "/nope foo" {
		t.Errorf("unknown slash = %q", got)
	}
	if got, _ := a.expandCommand("hello"); got != "hello" {
		t.Errorf("plain input = %q", got)
	}
}

// text joins message part texts (test helper).
func text(parts []core.Part) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// scriptedBackend scripts one tool call then a final answer (daemon e2e).
type scriptedBackend struct {
	toolName string
	toolArgs map[string]any
	final    string
	turn     int
}

func (b *scriptedBackend) Chat(_ context.Context, req core.Request) (core.Response, error) {
	b.turn++
	hasToolResult := false
	for _, m := range req.Messages {
		if m.Role == core.RoleTool {
			hasToolResult = true
		}
	}
	if !hasToolResult {
		return core.Response{Message: core.Message{Role: core.RoleAssistant,
			ToolCalls: []core.ToolCall{{
				ID: "call-1", Name: b.toolName, Input: b.toolArgs,
			}}}}, nil
	}
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: b.final}}}}, nil
}

func (b *scriptedBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		for _, p := range r.Message.Content {
			onDelta(core.Delta{Text: p.Text})
		}
	}
	return r, err
}

// newApprovalE2EServer builds a server wired like production: the real
// ReviewGate (ai mode, default heuristics) talking to a real ApprovalHub,
// exposed over the HTTP approval endpoints.
func newApprovalE2EServer(t *testing.T, backend core.LLMBackend, cfgTOML string) (*Server, string, *ApprovalHub) {
	t.Helper()
	home := t.TempDir()
	workDir := t.TempDir()
	if cfgTOML != "" {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfgTOML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st := store.NewMemory()
	hub := NewApprovalHub(nil)
	a, err := app.New(app.Options{
		SkipExtensions: true, Approver: hub,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: workDir,
		Backend: backend, Store: st,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{Home: home, App: a, Store: st, Token: "test-token", Gate: hub})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, workDir, hub
}

// pollApprovals waits for n pending tickets via the HTTP endpoint.
func pollApprovals(t *testing.T, h http.Handler, want int) []api.ApprovalTicket {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w, req := doJSON(t, http.MethodGet, "/api/v1/approvals", nil, "test-token")
		h.ServeHTTP(w, req)
		var tickets []api.ApprovalTicket
		decodeInto(t, w, &tickets)
		if len(tickets) >= want {
			return tickets
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("approval tickets never reached %d", want)
	return nil
}

// decodeInto decodes a recorder body into v.
func decodeInto(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

// TestApprovalE2EApproveWritesFile is the P3 exit evidence "dangerous
// operation goes through approval": HTTP task -> real ReviewGate (ai mode)
// -> uncertain ask escalates -> ticket with diff -> human approves -> tool
// really writes the file.
func TestApprovalE2EApproveWritesFile(t *testing.T) {
	backend := &scriptedBackend{
		toolName: "write",
		toolArgs: map[string]any{"path": "a.txt", "content": "approved content"},
		final:    "wrote a.txt",
	}
	s, workDir, _ := newApprovalE2EServer(t, backend, "")
	h := s.Handler()

	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "write the file"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create task = %d %s", w.Code, w.Body.String())
	}

	tickets := pollApprovals(t, h, 1)
	tk := tickets[0]
	if tk.Action != "write" || tk.Resource != "a.txt" {
		t.Errorf("ticket = %+v", tk)
	}
	if !strings.Contains(tk.Diff, "approved content") {
		t.Errorf("ticket diff must preview the pending change, got %q", tk.Diff)
	}

	w, req = doJSON(t, http.MethodPost, "/api/v1/approvals/"+tk.ID,
		api.ApprovalDecision{Decision: "approve", Reason: "looks fine"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body.String())
	}

	// The tool must really run after approval.
	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		if data, _ = os.ReadFile(filepath.Join(workDir, "a.txt")); data != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(data), "approved content") {
		t.Fatalf("file not written after approval: %q", data)
	}
}

// TestApprovalE2EDenyBlocksWrite: human denial stops the tool; no file lands.
func TestApprovalE2EDenyBlocksWrite(t *testing.T) {
	backend := &scriptedBackend{
		toolName: "write",
		toolArgs: map[string]any{"path": "b.txt", "content": "should not land"},
		final:    "done",
	}
	s, workDir, _ := newApprovalE2EServer(t, backend, "")
	h := s.Handler()

	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "write the file"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create task = %d %s", w.Code, w.Body.String())
	}

	tickets := pollApprovals(t, h, 1)
	w, req = doJSON(t, http.MethodPost, "/api/v1/approvals/"+tickets[0].ID,
		api.ApprovalDecision{Decision: "deny", Reason: "not now"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d %s", w.Code, w.Body.String())
	}

	time.Sleep(200 * time.Millisecond) // let the denial propagate to the tool
	if _, err := os.Stat(filepath.Join(workDir, "b.txt")); err == nil {
		t.Fatal("denied write must not create the file")
	}
}

// TestApprovalE2EDenyRuleHardBlock: an explicit deny rule blocks in every
// review mode without any human round-trip (architecture 4.2).
func TestApprovalE2EDenyRuleHardBlock(t *testing.T) {
	backend := &scriptedBackend{
		toolName: "write",
		toolArgs: map[string]any{"path": "secret.txt", "content": "leak"},
		final:    "done",
	}
	cfg := "[[permission]]\naction = \"write\"\nresource = \"secret.txt\"\neffect = \"deny\"\n"
	s, workDir, _ := newApprovalE2EServer(t, backend, cfg)
	h := s.Handler()

	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks",
		api.CreateTaskRequest{Prompt: "write the secret"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create task = %d %s", w.Code, w.Body.String())
	}

	// No ticket may be created: deny is a hard block before review.
	time.Sleep(300 * time.Millisecond)
	w, req = doJSON(t, http.MethodGet, "/api/v1/approvals", nil, "test-token")
	h.ServeHTTP(w, req)
	var tickets []api.ApprovalTicket
	decodeInto(t, w, &tickets)
	if len(tickets) != 0 {
		t.Fatalf("deny rule must not create tickets: %+v", tickets)
	}
	if _, err := os.Stat(filepath.Join(workDir, "secret.txt")); err == nil {
		t.Fatal("deny rule must block the write")
	}
}

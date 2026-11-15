// Export/import tests: full-field envelope roundtrip, idempotent import,
// multi-session isolation, corrupted JSON and the optional opencode importer.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// nanos exercises RFC3339Nano time precision through store roundtrips.
var nanos = baseTime.Add(123456789 * time.Nanosecond)

// seedExportSession creates a fully populated session for roundtrip tests.
func seedExportSession(t *testing.T, st Store, id string) {
	t.Helper()
	ctx := context.Background()
	sess := Session{
		ID: id, ParentID: "parent-1", Agent: "coder", Title: "导出测试", Model: "m1",
		CostUSD: 1.25, TokensIn: 100, TokensOut: 200,
		CreatedAt: baseTime, UpdatedAt: nanos,
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	msgs := []Message{
		{ID: id + "-m1", SessionID: id, Role: "user",
			Content: []byte(`[{"Type":"text","Text":"hi"}]`), CreatedAt: baseTime},
		{ID: id + "-m2", SessionID: id, Role: "assistant",
			Content:   []byte(`{"content":[{"Type":"text","Text":"hello"}],"tool_calls":[{"ID":"c1","Name":"bash","Input":{"cmd":"ls"}}]}`),
			Truncated: true, CreatedAt: nanos},
		{ID: id + "-m3", SessionID: id, Role: "tool",
			Content: []byte(`{"content":[{"Type":"text","Text":"ok"}],"tool_call_id":"c1"}`), CreatedAt: baseTime.Add(2 * time.Second)},
		{ID: id + "-m4", SessionID: id, Role: "system", CreatedAt: baseTime.Add(3 * time.Second)},
	}
	for _, m := range msgs {
		if err := st.AppendMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	audits := []AuditEntry{
		{SessionID: id, Action: "bash", Resource: "rm -rf", Effect: "ask", ReviewMode: "ai",
			Verdict: "uncertain", Approved: false, Actor: "user", CreatedAt: baseTime},
		{SessionID: id, Action: "edit", Resource: "a.go", Effect: "allow", ReviewMode: "human",
			Verdict: "ok", Approved: true, Actor: "subagent:coder", CreatedAt: nanos},
	}
	for _, a := range audits {
		if err := st.AppendAudit(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
}

// assertEnvelopeEqual compares two envelopes field by field, ignoring only
// regenerated ids (session/message ids) and the export timestamp.
func assertEnvelopeEqual(t *testing.T, want, got *SessionExport) {
	t.Helper()
	w, g := want.Session, got.Session
	if w.ParentID != g.ParentID || w.Agent != g.Agent || w.Title != g.Title ||
		w.Model != g.Model || w.CostUSD != g.CostUSD || w.TokensIn != g.TokensIn || w.TokensOut != g.TokensOut {
		t.Errorf("session metadata mismatch:\n want %+v\n got  %+v", w, g)
	}
	mustTimeEqual(t, w.CreatedAt, g.CreatedAt, "session.created_at")
	mustTimeEqual(t, w.UpdatedAt, g.UpdatedAt, "session.updated_at")
	if len(want.Messages) != len(got.Messages) {
		t.Fatalf("message count: want %d, got %d", len(want.Messages), len(got.Messages))
	}
	for i := range want.Messages {
		wm, gm := want.Messages[i], got.Messages[i]
		if wm.Role != gm.Role || wm.Truncated != gm.Truncated ||
			!bytes.Equal([]byte(wm.Content), []byte(gm.Content)) {
			t.Errorf("message %d mismatch:\n want %+v\n got  %+v", i, wm, gm)
		}
		mustTimeEqual(t, wm.CreatedAt, gm.CreatedAt, "message.created_at")
	}
	if len(want.Audit) != len(got.Audit) {
		t.Fatalf("audit count: want %d, got %d", len(want.Audit), len(got.Audit))
	}
	for i := range want.Audit {
		wa, ga := want.Audit[i], got.Audit[i]
		if wa.Action != ga.Action || wa.Resource != ga.Resource || wa.Effect != ga.Effect ||
			wa.ReviewMode != ga.ReviewMode || wa.Verdict != ga.Verdict ||
			wa.Approved != ga.Approved || wa.Actor != ga.Actor {
			t.Errorf("audit %d mismatch:\n want %+v\n got  %+v", i, wa, ga)
		}
		mustTimeEqual(t, wa.CreatedAt, ga.CreatedAt, "audit.created_at")
	}
}

// runExportRoundtrip exercises the full export → JSON → import → export cycle
// with all-field assertions on one implementation.
func runExportRoundtrip(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()
	seedExportSession(t, s, "s1")

	ex1, err := ExportSession(ctx, s, "s1")
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if ex1.FormatVersion != ExportFormatVersion || ex1.ExportedAt.IsZero() {
		t.Errorf("envelope header: %+v", ex1)
	}
	if ex1.Session.ID != "s1" || len(ex1.Messages) != 4 || len(ex1.Audit) != 2 {
		t.Fatalf("envelope shape: session=%q messages=%d audit=%d", ex1.Session.ID, len(ex1.Messages), len(ex1.Audit))
	}

	// JSON roundtrip in between: the file format is the contract.
	raw, err := json.Marshal(ex1)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSessionExport(raw)
	if err != nil {
		t.Fatalf("ParseSessionExport: %v", err)
	}

	newID, err := ImportSession(ctx, s, parsed)
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if newID == "s1" {
		t.Fatal("import must generate a fresh session id")
	}
	// Message ids must be regenerated too and unique within the new session.
	msgs, _ := s.ListMessages(ctx, newID, 0, 0)
	seen := map[string]bool{}
	for _, m := range msgs {
		if seen[m.ID] {
			t.Fatalf("duplicate message id %q", m.ID)
		}
		seen[m.ID] = true
		if m.SessionID != newID {
			t.Errorf("message %s bound to %q, want %q", m.ID, m.SessionID, newID)
		}
	}

	ex2, err := ExportSession(ctx, s, newID)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	assertEnvelopeEqual(t, ex1, ex2)

	// The source session must be untouched.
	orig, err := ExportSession(ctx, s, "s1")
	if err != nil {
		t.Fatal(err)
	}
	assertEnvelopeEqual(t, ex1, orig)
}

func TestExportImportRoundtrip(t *testing.T) {
	t.Run("memory", func(t *testing.T) { runExportRoundtrip(t, openMemory) })
	t.Run("sqlite", func(t *testing.T) { runExportRoundtrip(t, openSQLite) })
}

func TestExportImportEmptySession(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()
	if err := s.CreateSession(ctx, Session{ID: "e1", CreatedAt: baseTime, UpdatedAt: baseTime}); err != nil {
		t.Fatal(err)
	}
	ex, err := ExportSession(ctx, s, "e1")
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if len(ex.Messages) != 0 || len(ex.Audit) != 0 {
		t.Fatalf("empty session export: messages=%d audit=%d", len(ex.Messages), len(ex.Audit))
	}
	id, err := ImportSession(ctx, s, ex)
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if got, err := s.GetSession(ctx, id); err != nil || got.Title != "" {
		t.Fatalf("imported empty session: %+v, %v", got, err)
	}
}

func TestExportImportMultiSessionIsolation(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()
	seedExportSession(t, s, "a1")
	seedExportSession(t, s, "b1")

	exA, err := ExportSession(ctx, s, "a1")
	if err != nil {
		t.Fatal(err)
	}
	exB, err := ExportSession(ctx, s, "b1")
	if err != nil {
		t.Fatal(err)
	}
	idA, err := ImportSession(ctx, s, exA)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := ImportSession(ctx, s, exB)
	if err != nil {
		t.Fatal(err)
	}
	msgsA, _ := s.ListMessages(ctx, idA, 0, 0)
	msgsB, _ := s.ListMessages(ctx, idB, 0, 0)
	if len(msgsA) != 4 || len(msgsB) != 4 {
		t.Fatalf("cross contamination: A=%d B=%d", len(msgsA), len(msgsB))
	}
	for _, m := range msgsA {
		if bytes.Contains([]byte(m.Content), []byte(`"hi"`)) && m.Role != "user" {
			t.Errorf("unexpected message in A: %+v", m)
		}
	}
	// Audit trails stay per session.
	auditsA, _ := s.ListAudit(ctx, idA, 0)
	auditsB, _ := s.ListAudit(ctx, idB, 0)
	if len(auditsA) != 2 || len(auditsB) != 2 {
		t.Fatalf("audit isolation: A=%d B=%d", len(auditsA), len(auditsB))
	}
}

// TestImportIdempotentNoOverwrite imports the same envelope repeatedly (and
// against the still-existing source id): every import is a fresh copy, the
// source is never overwritten.
func TestImportIdempotentNoOverwrite(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()
	seedExportSession(t, s, "s1")
	ex, err := ExportSession(ctx, s, "s1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ex)

	id1, err := ImportSession(ctx, s, ex)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := ImportSession(ctx, s, ex) // same envelope again
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 || id1 == "s1" || id2 == "s1" {
		t.Fatalf("ids must be distinct: %q %q", id1, id2)
	}
	// All three copies coexist and are complete.
	for _, id := range []string{"s1", id1, id2} {
		msgs, _ := s.ListMessages(ctx, id, 0, 0)
		if len(msgs) != 4 {
			t.Fatalf("session %s: want 4 messages, got %d", id, len(msgs))
		}
	}
	// The envelope itself is unchanged after imports (idempotent input).
	ex2, err := ParseSessionExport(raw)
	if err != nil {
		t.Fatal(err)
	}
	assertEnvelopeEqual(t, ex, ex2)
}

func TestExportErrors(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()
	if _, err := ExportSession(ctx, s, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("export missing session: want ErrNotFound, got %v", err)
	}
	// Malformed stored content is salvaged as a text part, not a hard error:
	// legacy byte-truncated blobs must not block exporting the whole session.
	if err := s.AppendMessage(ctx, Message{ID: "m1", SessionID: "s1", Content: []byte("not-json")}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, Session{ID: "s1", CreatedAt: baseTime, UpdatedAt: baseTime}); err != nil {
		t.Fatal(err)
	}
	ex, err := ExportSession(ctx, s, "s1")
	if err != nil {
		t.Fatalf("malformed content must not block export: %v", err)
	}
	if len(ex.Messages) != 1 {
		t.Fatalf("messages = %d", len(ex.Messages))
	}
	if !bytes.Contains(ex.Messages[0].Content, []byte("not-json")) {
		t.Errorf("damaged blob should be salvaged as text: %s", ex.Messages[0].Content)
	}
}

func TestParseSessionExportErrors(t *testing.T) {
	if _, err := ParseSessionExport([]byte(`{"format_version":`)); err == nil {
		t.Fatal("corrupted JSON should fail")
	}
	if _, err := ParseSessionExport([]byte(`{"format_version":99,"session":{"id":"x"}}`)); err == nil {
		t.Fatal("unsupported format_version should fail")
	}
	if _, err := ParseSessionExport([]byte(`{"format_version":1}`)); err == nil {
		t.Fatal("missing session id should fail")
	}
	if _, err := ParseSessionExport([]byte(`{"format_version":1,"session":{"id":"x"}}`)); err != nil {
		t.Fatalf("minimal valid envelope: %v", err)
	}
}

func TestImportSessionErrors(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()
	if _, err := ImportSession(ctx, s, nil); err == nil {
		t.Fatal("nil envelope should fail")
	}
	if _, err := ImportSession(ctx, s, &SessionExport{FormatVersion: 99, Session: ExportedSession{ID: "x"}}); err == nil {
		t.Fatal("unsupported format_version should fail")
	}
	if _, err := ImportSession(ctx, s, &SessionExport{FormatVersion: ExportFormatVersion}); err == nil {
		t.Fatal("missing session id should fail")
	}
}

// TestExportJSONFieldNames pins the snake_case wire format.
func TestExportJSONFieldNames(t *testing.T) {
	raw, err := json.Marshal(&SessionExport{
		FormatVersion: ExportFormatVersion, ExportedAt: baseTime,
		Session:  ExportedSession{ID: "s1", ParentID: "p", CreatedAt: baseTime},
		Messages: []ExportedMessage{{ID: "m1", SessionID: "s1", Content: json.RawMessage("null"), CreatedAt: baseTime}},
		Audit:    []ExportedAudit{{Action: "read", CreatedAt: baseTime}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"format_version", "exported_at", "session", "messages", "audit"} {
		if _, ok := top[key]; !ok {
			t.Errorf("missing top-level key %q in %s", key, raw)
		}
	}
	var sess map[string]json.RawMessage
	if err := json.Unmarshal(top["session"], &sess); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "parent_id", "agent", "title", "model", "cost_usd",
		"tokens_in", "tokens_out", "created_at", "updated_at"} {
		if _, ok := sess[key]; !ok {
			t.Errorf("missing session key %q in %s", key, top["session"])
		}
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(top["messages"], &msg); err == nil {
		t.Fatalf("messages must be an array, got %s", top["messages"])
	} else if len(msg) != 0 {
		t.Fatal("unreachable")
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(top["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "session_id", "role", "content", "truncated", "created_at"} {
		if _, ok := msgs[0][key]; !ok {
			t.Errorf("missing message key %q in %s", key, top["messages"])
		}
	}
	var audits []map[string]json.RawMessage
	if err := json.Unmarshal(top["audit"], &audits); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"action", "resource", "effect", "review_mode", "verdict",
		"approved", "actor", "created_at"} {
		if _, ok := audits[0][key]; !ok {
			t.Errorf("missing audit key %q in %s", key, top["audit"])
		}
	}
}

// --- opencode importer ---

// ocFixture is a minimal `opencode session export` document covering every
// message type and part type observed in opencode v2.0.28 output.
const ocFixture = `{
  "info": {
    "id": "ses_oc1",
    "projectID": "p1",
    "agent": "build",
    "model": {"id": "claude-fable-5", "providerID": "anthropic", "variant": "default"},
    "cost": 1.5,
    "tokens": {"input": 100, "output": 200, "reasoning": 0, "cache": {"read": 1, "write": 2}},
    "time": {"created": 1783070185868, "updated": 1783072212438},
    "title": "Quick check-in",
    "location": {"directory": "/tmp"}
  },
  "messages": [
    {"id": "msg_u1", "type": "user", "text": "hi", "time": {"created": 1783070185889}},
    {"id": "msg_a1", "type": "assistant", "agent": "build",
     "model": {"id": "claude-fable-5", "providerID": "anthropic"},
     "time": {"created": 1783070185909, "completed": 1783070196805},
     "cost": 0.5, "tokens": {"input": 31, "output": 18},
     "content": [
       {"type": "text", "text": "let me read"},
       {"type": "reasoning", "text": "needs the file", "time": {"created": 1783070185910}},
       {"type": "tool", "id": "tooluse_1", "name": "read",
        "time": {"created": 1783070212697, "completed": 1783070212709},
        "state": {"status": "completed", "input": {"filePath": "/tmp/a.md"},
                  "content": [{"type": "text", "text": "file body"}], "metadata": {"preview": "file"}}}
     ],
     "snapshot": {"start": "abc", "end": "def"}, "finish": "stop"},
    {"id": "msg_a2", "type": "assistant",
     "time": {"created": 1783070212800},
     "content": [
       {"type": "tool", "id": "tooluse_2", "name": "bash",
        "time": {"created": 1783070212810},
        "state": {"status": "error", "input": {"cmd": "boom"},
                  "error": {"type": "tool.execution", "message": "Tool execution aborted"}}}
     ]},
    {"id": "msg_s1", "type": "synthetic", "text": "The following tool was executed by the user",
     "time": {"created": 1783256551821}},
    {"id": "msg_y1", "type": "system", "text": "summarized", "time": {"created": 1783256551900}}
  ]
}`

func TestImportOpencodeSession(t *testing.T) {
	ctx := context.Background()
	s, closeFn := openSQLite(t)
	defer closeFn()

	id, err := ImportOpencodeSession(ctx, s, []byte(ocFixture))
	if err != nil {
		t.Fatalf("ImportOpencodeSession: %v", err)
	}
	sess, err := s.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "ses_oc1" {
		t.Fatal("import must regenerate the session id")
	}
	if sess.Agent != "build" || sess.Model != "anthropic/claude-fable-5" || sess.Title != "Quick check-in" {
		t.Errorf("session metadata: %+v", sess)
	}
	if sess.CostUSD != 1.5 || sess.TokensIn != 100 || sess.TokensOut != 200 {
		t.Errorf("session totals: %+v", sess)
	}
	mustTimeEqual(t, time.UnixMilli(1783070185868).UTC(), sess.CreatedAt, "created_at")
	mustTimeEqual(t, time.UnixMilli(1783072212438).UTC(), sess.UpdatedAt, "updated_at")

	msgs, err := s.ListMessages(ctx, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant+tool result, assistant+tool result, synthetic→system, system.
	if len(msgs) != 7 {
		t.Fatalf("want 7 rows, got %d", len(msgs))
	}
	roles := []string{"user", "assistant", "tool", "assistant", "tool", "system", "system"}
	for i, want := range roles {
		if msgs[i].Role != want {
			t.Errorf("row %d role: want %q, got %q", i, want, msgs[i].Role)
		}
	}

	var user msgBlob
	if err := json.Unmarshal(msgs[0].Content, &user); err != nil {
		t.Fatal(err)
	}
	if len(user.Content) != 1 || user.Content[0].Text != "hi" {
		t.Errorf("user content: %+v", user)
	}

	var a1 msgBlob
	if err := json.Unmarshal(msgs[1].Content, &a1); err != nil {
		t.Fatal(err)
	}
	if len(a1.Content) != 2 || a1.Content[0].Type != "text" || a1.Content[1].Type != "reasoning" {
		t.Errorf("assistant parts: %+v", a1.Content)
	}
	if len(a1.ToolCalls) != 1 || a1.ToolCalls[0].ID != "tooluse_1" || a1.ToolCalls[0].Name != "read" {
		t.Errorf("assistant tool calls: %+v", a1.ToolCalls)
	}
	if got := a1.ToolCalls[0].Input["filePath"]; got != "/tmp/a.md" {
		t.Errorf("tool input: %+v", a1.ToolCalls[0].Input)
	}

	var r1 msgBlob
	if err := json.Unmarshal(msgs[2].Content, &r1); err != nil {
		t.Fatal(err)
	}
	if r1.ToolCallID != "tooluse_1" || len(r1.Content) != 1 || r1.Content[0].Text != "file body" {
		t.Errorf("tool result: %+v", r1)
	}

	// Error state lands as a trailing text part on the tool result.
	var r2 msgBlob
	if err := json.Unmarshal(msgs[4].Content, &r2); err != nil {
		t.Fatal(err)
	}
	if r2.ToolCallID != "tooluse_2" || len(r2.Content) != 1 || r2.Content[0].Text != "Tool execution aborted" {
		t.Errorf("error tool result: %+v", r2)
	}

	// synthetic maps to system; system text survives.
	var syn, sys msgBlob
	if err := json.Unmarshal(msgs[5].Content, &syn); err != nil {
		t.Fatal(err)
	}
	if len(syn.Content) != 1 || syn.Content[0].Text != "The following tool was executed by the user" {
		t.Errorf("synthetic content: %+v", syn)
	}
	if err := json.Unmarshal(msgs[6].Content, &sys); err != nil {
		t.Fatal(err)
	}
	if len(sys.Content) != 1 || sys.Content[0].Text != "summarized" {
		t.Errorf("system content: %+v", sys)
	}
	// Imported timestamps keep document order even in SQLite's RFC3339Nano
	// text sort: strictly increasing, last nanosecond digit nonzero so no
	// trailing-zero trimming can misorder "...8Z" after "...81Z".
	for i := 1; i < len(msgs); i++ {
		if formatTime(msgs[i-1].CreatedAt) >= formatTime(msgs[i].CreatedAt) {
			t.Errorf("timestamps not text-sort stable at row %d: %q >= %q",
				i, formatTime(msgs[i-1].CreatedAt), formatTime(msgs[i].CreatedAt))
		}
	}
	mustTimeEqual(t, time.UnixMilli(1783070212697).UTC().Add(time.Nanosecond), msgs[2].CreatedAt, "tool created_at")
}

func TestParseOpencodeSessionErrors(t *testing.T) {
	cases := map[string]string{
		"corrupted json":     `{"info":`,
		"missing info.id":    `{"info": {}, "messages": []}`,
		"bad message type":   `{"info":{"id":"s"},"messages":[{"id":"m","type":"nope"}]}`,
		"user with parts":    `{"info":{"id":"s"},"messages":[{"id":"m","type":"user","content":[{"type":"text"}]}]}`,
		"unsupported part":   `{"info":{"id":"s"},"messages":[{"id":"m","type":"assistant","content":[{"type":"image"}]}]}`,
		"tool without state": `{"info":{"id":"s"},"messages":[{"id":"m","type":"assistant","content":[{"type":"tool","id":"t"}]}]}`,
	}
	for name, doc := range cases {
		if _, err := parseOpencodeSession([]byte(doc)); err == nil {
			t.Errorf("%s: should fail", name)
		}
	}
	// Minimal valid document: a single user message.
	ex, err := parseOpencodeSession([]byte(`{"info":{"id":"s"},"messages":[{"id":"m","type":"user","text":"hey"}]}`))
	if err != nil {
		t.Fatalf("minimal document: %v", err)
	}
	if len(ex.Messages) != 1 || ex.Messages[0].Role != "user" {
		t.Fatalf("minimal mapping: %+v", ex.Messages)
	}
	// Model object without provider uses the bare id.
	if got := opencodeModelString(opencodeModel{ID: "m"}); got != "m" {
		t.Errorf("model string: %q", got)
	}
}

// TestImportOpencodeCorrupted fails the import path with garbage input.
func TestImportOpencodeCorrupted(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	if _, err := ImportOpencodeSession(ctx, s, []byte("{oops")); err == nil {
		t.Fatal("corrupted opencode JSON should fail")
	}
}

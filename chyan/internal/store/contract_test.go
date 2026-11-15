// Shared contract test suite run against both Store implementations
// (SQLite and in-memory), per development-plan line B.
package store

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// roller matches ChildStatRoller; both implementations provide it even
// though the frozen Store interface does not.
type roller interface {
	RollupChildStats(ctx context.Context, parentID string) error
}

// openStore returns a fresh Store plus its closer.
type openStore func(t *testing.T) (Store, func())

var baseTime = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

func mustTimeEqual(t *testing.T, want, got time.Time, field string) {
	t.Helper()
	if !want.Equal(got) {
		t.Errorf("%s: want %v, got %v", field, want, got)
	}
}

func assertSessionEqual(t *testing.T, want, got Session) {
	t.Helper()
	if got.ID != want.ID || got.ParentID != want.ParentID || got.Agent != want.Agent ||
		got.Title != want.Title || got.Model != want.Model ||
		got.Status != want.Status ||
		got.CostUSD != want.CostUSD || got.TokensIn != want.TokensIn || got.TokensOut != want.TokensOut {
		t.Errorf("session mismatch:\n want %+v\n got  %+v", want, got)
	}
	mustTimeEqual(t, want.CreatedAt, got.CreatedAt, "created_at")
	mustTimeEqual(t, want.UpdatedAt, got.UpdatedAt, "updated_at")
}

func assertTaskEqual(t *testing.T, want, got Task) {
	t.Helper()
	if got.ID != want.ID || got.SessionID != want.SessionID || got.Kind != want.Kind ||
		got.Schedule != want.Schedule || got.Status != want.Status {
		t.Errorf("task mismatch:\n want %+v\n got  %+v", want, got)
	}
	mustTimeEqual(t, want.LastRunAt, got.LastRunAt, "last_run_at")
	mustTimeEqual(t, want.CreatedAt, got.CreatedAt, "created_at")
}

// runContractTests exercises the full Store interface on one implementation.
func runContractTests(t *testing.T, open openStore) {
	t.Run("sessions", func(t *testing.T) { testSessions(t, open) })
	t.Run("messages", func(t *testing.T) { testMessages(t, open) })
	t.Run("messages-order", func(t *testing.T) { testMessagesSameTimestampOrder(t, open) })
	t.Run("truncation", func(t *testing.T) { testTruncation(t, open) })
	t.Run("tasks", func(t *testing.T) { testTasks(t, open) })
	t.Run("audit", func(t *testing.T) { testAudit(t, open) })
	t.Run("prune", func(t *testing.T) { testPrune(t, open) })
	t.Run("kv", func(t *testing.T) { testKV(t, open) })
	t.Run("rollup", func(t *testing.T) { testRollup(t, open) })
	t.Run("concurrent", func(t *testing.T) { testConcurrent(t, open) })
}

func testSessions(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	sess := Session{ID: "s1", Agent: "coder", Title: "hello", Model: "m1",
		Status:  SessionStatusActive,
		CostUSD: 0.5, TokensIn: 10, TokensOut: 20,
		CreatedAt: baseTime, UpdatedAt: baseTime.Add(time.Minute)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Duplicate ID must fail on both implementations.
	if err := s.CreateSession(ctx, sess); err == nil {
		t.Fatal("duplicate CreateSession should fail")
	}
	if err := s.CreateSession(ctx, Session{}); err == nil {
		t.Fatal("empty id CreateSession should fail")
	}

	got, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	assertSessionEqual(t, sess, got)

	if _, err := s.GetSession(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSession missing: want ErrNotFound, got %v", err)
	}

	sess.Title = "updated"
	sess.CostUSD = 1.5
	sess.Status = SessionStatusInterrupted // run cut short by the user
	sess.UpdatedAt = baseTime.Add(2 * time.Minute)
	if err := s.UpdateSession(ctx, sess); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	got, _ = s.GetSession(ctx, "s1")
	assertSessionEqual(t, sess, got)
	if err := s.UpdateSession(ctx, Session{ID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSession missing: want ErrNotFound, got %v", err)
	}

	// List ordering (updated_at DESC), limit and offset.
	s2 := Session{ID: "s2", CreatedAt: baseTime, UpdatedAt: baseTime.Add(3 * time.Minute)}
	s3 := Session{ID: "s3", CreatedAt: baseTime, UpdatedAt: baseTime.Add(4 * time.Minute)}
	if err := s.CreateSession(ctx, s2); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, s3); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 3 || list[0].ID != "s3" || list[1].ID != "s2" || list[2].ID != "s1" {
		t.Fatalf("ListSessions order: got %v", sessionIDs(list))
	}
	list, _ = s.ListSessions(ctx, 2, 1)
	if len(list) != 2 || list[0].ID != "s2" || list[1].ID != "s1" {
		t.Fatalf("ListSessions limit/offset: got %v", sessionIDs(list))
	}
	// offset without a limit still applies; a negative offset is 0.
	list, _ = s.ListSessions(ctx, 0, 1)
	if len(list) != 2 || list[0].ID != "s2" {
		t.Fatalf("ListSessions offset-only: got %v", sessionIDs(list))
	}
	list, _ = s.ListSessions(ctx, 0, -3)
	if len(list) != 3 || list[0].ID != "s3" {
		t.Fatalf("ListSessions negative offset: got %v", sessionIDs(list))
	}

	// Delete cascades and reports missing.
	if err := s.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted session should be gone")
	}
	if err := s.DeleteSession(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteSession missing: want ErrNotFound, got %v", err)
	}
}

func sessionIDs(list []Session) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return out
}

func testMessages(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	if err := s.AppendMessage(ctx, Message{ID: "m0", SessionID: "s0"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.AppendMessage(ctx, Message{ID: "", SessionID: "s1"}); err == nil {
		t.Fatal("empty message id should fail")
	}
	if err := s.AppendMessage(ctx, Message{ID: "m1"}); err == nil {
		t.Fatal("empty session id should fail")
	}

	payload := []byte(`[{"type":"text","text":"hi"}]`)
	msgs := []Message{
		{ID: "m1", SessionID: "s1", Role: "user", Content: payload, CreatedAt: baseTime},
		{ID: "m2", SessionID: "s1", Role: "assistant", Content: payload, CreatedAt: baseTime.Add(time.Second)},
		{ID: "m3", SessionID: "s2", Role: "tool", Content: payload, CreatedAt: baseTime},
	}
	for _, m := range msgs {
		if err := s.AppendMessage(ctx, m); err != nil {
			t.Fatalf("AppendMessage %s: %v", m.ID, err)
		}
	}

	list, err := s.ListMessages(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(list) != 2 || list[0].ID != "m1" || list[1].ID != "m2" {
		t.Fatalf("ListMessages: got %d rows", len(list))
	}
	if !bytes.Equal(list[0].Content, payload) || list[0].Truncated {
		t.Errorf("message roundtrip mismatch: %q truncated=%v", list[0].Content, list[0].Truncated)
	}
	// Role must round-trip: without it a replayed session loses who said
	// what and tool call/result pairs look orphaned.
	if list[0].Role != "user" || list[1].Role != "assistant" {
		t.Errorf("role roundtrip mismatch: got %q, %q", list[0].Role, list[1].Role)
	}
	mustTimeEqual(t, baseTime, list[0].CreatedAt, "created_at")

	list, _ = s.ListMessages(ctx, "s1", 1, 1)
	if len(list) != 1 || list[0].ID != "m2" {
		t.Fatalf("ListMessages limit/offset: got %v", list)
	}
	// Negative offset behaves like 0, not a panic.
	list, _ = s.ListMessages(ctx, "s1", 0, -1)
	if len(list) != 2 || list[0].ID != "m1" {
		t.Fatalf("ListMessages negative offset: got %v", list)
	}
	if list, _ = s.ListMessages(ctx, "none", 0, 0); len(list) != 0 {
		t.Fatalf("ListMessages unknown session: got %d rows", len(list))
	}

	// Duplicate message ids are rejected globally by both backends.
	if err := s.AppendMessage(ctx, msgs[0]); err == nil {
		t.Fatal("duplicate message id should fail")
	}
	if err := s.AppendMessage(ctx, Message{ID: "m1", SessionID: "s2"}); err == nil {
		t.Fatal("duplicate message id in another session should fail")
	}
	if list, _ := s.ListMessages(ctx, "s2", 0, 0); len(list) != 1 || list[0].ID != "m3" {
		t.Fatalf("rejected duplicate changed target session: got %v", list)
	}

	// Recent tail, still in append order (REPL session-switch view).
	list, err = s.ListRecentMessages(ctx, "s1", 1)
	if err != nil || len(list) != 1 || list[0].ID != "m2" {
		t.Fatalf("ListRecentMessages limit: got %v err=%v", list, err)
	}
	list, _ = s.ListRecentMessages(ctx, "s1", 10)
	if len(list) != 2 || list[0].ID != "m1" || list[1].ID != "m2" {
		t.Fatalf("ListRecentMessages order: got %v", list)
	}
	// limit <= 0 means "the whole session" on both backends.
	if list, _ := s.ListRecentMessages(ctx, "s1", 0); len(list) != 2 {
		t.Fatalf("ListRecentMessages limit=0: got %d rows", len(list))
	}
}

// testMessagesSameTimestampOrder guards insertion order: a tool call and its
// result share the same second, so ordering by (created_at, id) shuffled the
// tool result after the final assistant reply and replayed history was
// rejected ("tool_calls must be followed by tool messages"). rowid keeps the
// write order.
func testMessagesSameTimestampOrder(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	same := baseTime
	// IDs deliberately ordered so an id-based sort reverses them.
	seq := []Message{
		{ID: "m_z_call", SessionID: "s1", Role: "assistant", Content: []byte(`{"tool_calls":[{"ID":"call_1"}]}`), CreatedAt: same},
		{ID: "m_a_result", SessionID: "s1", Role: "tool", Content: []byte(`{"tool_call_id":"call_1"}`), CreatedAt: same},
		{ID: "m_m_final", SessionID: "s1", Role: "assistant", Content: []byte(`{"content":[]}`), CreatedAt: same},
	}
	for _, m := range seq {
		if err := s.AppendMessage(ctx, m); err != nil {
			t.Fatalf("AppendMessage %s: %v", m.ID, err)
		}
	}
	list, err := s.ListMessages(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListMessages: got %d rows", len(list))
	}
	for i, want := range seq {
		if list[i].ID != want.ID {
			t.Fatalf("order[%d] = %s, want %s (insertion order must hold at equal timestamps)", i, list[i].ID, want.ID)
		}
	}
}

// testTruncation covers the tool_output_max_bytes persistence cap.
func testTruncation(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	// Exactly at the cap: stored as-is, not truncated.
	atCap := bytes.Repeat([]byte("a"), MaxMessageBytes)
	if err := s.AppendMessage(ctx, Message{ID: "t1", SessionID: "s", Content: atCap}); err != nil {
		t.Fatal(err)
	}
	// One byte over: cut to cap and flagged.
	over := bytes.Repeat([]byte("b"), MaxMessageBytes+1)
	if err := s.AppendMessage(ctx, Message{ID: "t2", SessionID: "s", Content: over}); err != nil {
		t.Fatal(err)
	}
	// Pre-truncated flag survives a small payload.
	if err := s.AppendMessage(ctx, Message{ID: "t3", SessionID: "s", Content: []byte("x"), Truncated: true}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListMessages(ctx, "s", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 messages, got %d", len(list))
	}
	if list[0].Truncated || len(list[0].Content) != MaxMessageBytes {
		t.Errorf("at-cap: truncated=%v len=%d", list[0].Truncated, len(list[0].Content))
	}
	if !list[1].Truncated || len(list[1].Content) != MaxMessageBytes {
		t.Errorf("over-cap: truncated=%v len=%d", list[1].Truncated, len(list[1].Content))
	}
	if !list[2].Truncated || len(list[2].Content) != 1 {
		t.Errorf("pre-flagged: truncated=%v len=%d", list[2].Truncated, len(list[2].Content))
	}
}

func testTasks(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	if err := s.CreateTask(ctx, Task{ID: ""}); err == nil {
		t.Fatal("empty task id should fail")
	}
	t1 := Task{ID: "tk1", SessionID: "s1", Kind: "cron", Schedule: "0 9 * * *",
		Status: "pending", LastRunAt: baseTime, CreatedAt: baseTime}
	t2 := Task{ID: "tk2", Kind: "dispatch", Status: "running",
		CreatedAt: baseTime.Add(time.Minute)} // zero LastRunAt
	for _, tk := range []Task{t1, t2} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	if err := s.CreateTask(ctx, t1); err == nil {
		t.Fatal("duplicate CreateTask should fail")
	}

	all, err := s.ListTasks(ctx, "")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(all) != 2 || all[0].ID != "tk1" || all[1].ID != "tk2" {
		t.Fatalf("ListTasks order: got %d rows", len(all))
	}
	assertTaskEqual(t, t1, all[0])
	assertTaskEqual(t, t2, all[1]) // zero LastRunAt roundtrip

	pending, err := s.ListTasks(ctx, "pending")
	if err != nil || len(pending) != 1 || pending[0].ID != "tk1" {
		t.Fatalf("ListTasks filter: %v, %v", pending, err)
	}
	if got, _ := s.ListTasks(ctx, "nope"); len(got) != 0 {
		t.Fatalf("ListTasks unknown status: %d rows", len(got))
	}

	t2.Status = "done"
	t2.LastRunAt = baseTime.Add(time.Hour)
	if err := s.UpdateTask(ctx, t2); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	updated, _ := s.ListTasks(ctx, "done")
	if len(updated) != 1 {
		t.Fatalf("updated task not found: %v", updated)
	}
	assertTaskEqual(t, t2, updated[0])
	if err := s.UpdateTask(ctx, Task{ID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateTask missing: want ErrNotFound, got %v", err)
	}
}

func testAudit(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	e1 := AuditEntry{SessionID: "s1", Action: "bash", Resource: "rm -rf",
		Effect: "ask", ReviewMode: "ai", Verdict: "uncertain", Approved: false,
		Actor: "user", CreatedAt: baseTime}
	e2 := AuditEntry{SessionID: "s1", Action: "edit", Resource: "a.go",
		Effect: "allow", ReviewMode: "human", Verdict: "ok", Approved: true,
		Actor: "subagent:coder", CreatedAt: baseTime.Add(time.Minute)}
	e3 := AuditEntry{SessionID: "s2", Action: "read", Effect: "allow", CreatedAt: baseTime}
	for _, e := range []AuditEntry{e1, e2, e3} {
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}

	list, err := s.ListAudit(ctx, "s1", 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListAudit: got %d rows", len(list))
	}
	// Newest first.
	if list[0].ID <= list[1].ID {
		t.Errorf("ListAudit should be newest first: %v", list)
	}
	got := list[0]
	if got.Action != "edit" || got.Resource != "a.go" || got.Effect != "allow" ||
		got.ReviewMode != "human" || got.Verdict != "ok" || !got.Approved ||
		got.Actor != "subagent:coder" {
		t.Errorf("audit fields mismatch: %+v", got)
	}
	mustTimeEqual(t, e2.CreatedAt, got.CreatedAt, "created_at")

	limited, _ := s.ListAudit(ctx, "s1", 1)
	if len(limited) != 1 || limited[0].ID != list[0].ID {
		t.Fatalf("ListAudit limit: %v", limited)
	}
	if got, _ := s.ListAudit(ctx, "none", 0); len(got) != 0 {
		t.Fatalf("ListAudit unknown session: %d rows", len(got))
	}
}

func testKV(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	if _, err := s.GetKV(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetKV missing: want ErrNotFound, got %v", err)
	}
	if err := s.SetKV(ctx, "", []byte("x")); err == nil {
		t.Fatal("empty key should fail")
	}

	if err := s.SetKV(ctx, "k1", []byte("v1")); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	v, err := s.GetKV(ctx, "k1")
	if err != nil || !bytes.Equal(v, []byte("v1")) {
		t.Fatalf("GetKV: %q, %v", v, err)
	}
	if err := s.SetKV(ctx, "k1", []byte("v2")); err != nil {
		t.Fatalf("SetKV overwrite: %v", err)
	}
	v, _ = s.GetKV(ctx, "k1")
	if !bytes.Equal(v, []byte("v2")) {
		t.Fatalf("GetKV after overwrite: %q", v)
	}
}

// testRollup covers B2 child-to-parent stat aggregation.
func testRollup(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()
	r, ok := s.(roller)
	if !ok {
		t.Fatal("store must implement RollupChildStats")
	}

	parent := Session{ID: "p", CostUSD: 1, TokensIn: 100, TokensOut: 50,
		CreatedAt: baseTime, UpdatedAt: baseTime}
	child1 := Session{ID: "c1", ParentID: "p", CostUSD: 0.25, TokensIn: 10, TokensOut: 5,
		CreatedAt: baseTime, UpdatedAt: baseTime}
	child2 := Session{ID: "c2", ParentID: "p", CostUSD: 0.75, TokensIn: 30, TokensOut: 20,
		CreatedAt: baseTime, UpdatedAt: baseTime}
	grandchild := Session{ID: "g", ParentID: "c1", CostUSD: 9, TokensIn: 900, TokensOut: 900,
		CreatedAt: baseTime, UpdatedAt: baseTime}
	for _, sess := range []Session{parent, child1, child2, grandchild} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.RollupChildStats(ctx, "p"); err != nil {
		t.Fatalf("RollupChildStats: %v", err)
	}
	got, _ := s.GetSession(ctx, "p")
	if got.CostUSD != 2.0 || got.TokensIn != 140 || got.TokensOut != 75 {
		t.Fatalf("rollup totals: %+v", got)
	}

	// Grandchild stats roll into c1 only when requested.
	if err := r.RollupChildStats(ctx, "c1"); err != nil {
		t.Fatalf("RollupChildStats c1: %v", err)
	}
	c1, _ := s.GetSession(ctx, "c1")
	if c1.CostUSD != 9.25 || c1.TokensIn != 910 || c1.TokensOut != 905 {
		t.Fatalf("rollup c1 totals: %+v", c1)
	}

	// Empty parent id is a no-op; unknown parent reports ErrNotFound.
	if err := r.RollupChildStats(ctx, ""); err != nil {
		t.Fatalf("rollup empty id: %v", err)
	}
	if err := r.RollupChildStats(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollup missing: want ErrNotFound, got %v", err)
	}

	// Parent with no children: unchanged.
	orphan := Session{ID: "o", CostUSD: 3, CreatedAt: baseTime, UpdatedAt: baseTime}
	if err := s.CreateSession(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if err := r.RollupChildStats(ctx, "o"); err != nil {
		t.Fatal(err)
	}
	o, _ := s.GetSession(ctx, "o")
	if o.CostUSD != 3 {
		t.Fatalf("childless parent changed: %+v", o)
	}
}

// testConcurrent writes from many goroutines (dispatch-safety).
func testConcurrent(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n*3)
	for i := 0; i < n; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			errs <- s.AppendMessage(ctx, Message{ID: "m-" + strconv.Itoa(i), SessionID: "s", Content: []byte("m")})
		}(i)
		go func(i int) {
			defer wg.Done()
			errs <- s.SetKV(ctx, "k"+strconv.Itoa(i), []byte("v"))
		}(i)
		go func(i int) {
			defer wg.Done()
			errs <- s.AppendAudit(ctx, AuditEntry{SessionID: "s", Action: "bash", CreatedAt: baseTime})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	}
	msgs, _ := s.ListMessages(ctx, "s", 0, 0)
	if len(msgs) != n {
		t.Fatalf("messages: want %d, got %d", n, len(msgs))
	}
	audits, _ := s.ListAudit(ctx, "s", 0)
	if len(audits) != n {
		t.Fatalf("audits: want %d, got %d", n, len(audits))
	}
}

// testPrune covers the retention boundary: dry-run counts without deleting,
// and a real prune removes the session, its messages and old audit rows.
func testPrune(t *testing.T, open openStore) {
	ctx := context.Background()
	s, closeFn := open(t)
	defer closeFn()

	old := Session{ID: "old", Status: SessionStatusActive,
		CreatedAt: baseTime, UpdatedAt: baseTime}
	newer := Session{ID: "new", Status: SessionStatusActive,
		CreatedAt: baseTime.Add(time.Hour), UpdatedAt: baseTime.Add(time.Hour)}
	for _, sess := range []Session{old, newer} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession %s: %v", sess.ID, err)
		}
	}
	payload := []byte(`[{"type":"text","text":"hi"}]`)
	for _, m := range []Message{
		{ID: "m-old", SessionID: "old", Content: payload, CreatedAt: baseTime},
		{ID: "m-new", SessionID: "new", Content: payload, CreatedAt: baseTime.Add(time.Hour)},
	} {
		if err := s.AppendMessage(ctx, m); err != nil {
			t.Fatalf("AppendMessage %s: %v", m.ID, err)
		}
	}
	cutoff := baseTime.Add(30 * time.Minute)

	n, err := s.PruneSessions(ctx, cutoff, true)
	if err != nil {
		t.Fatalf("PruneSessions dry-run: %v", err)
	}
	if n != 1 {
		t.Fatalf("dry-run count = %d, want 1", n)
	}
	if _, err := s.GetSession(ctx, "old"); err != nil {
		t.Fatal("dry-run must not delete")
	}

	n, err = s.PruneSessions(ctx, cutoff, false)
	if err != nil {
		t.Fatalf("PruneSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("prune count = %d, want 1", n)
	}
	if _, err := s.GetSession(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old session should be pruned")
	}
	if msgs, _ := s.ListMessages(ctx, "old", 0, 0); len(msgs) != 0 {
		t.Fatalf("old session messages survived: %d", len(msgs))
	}
	if _, err := s.GetSession(ctx, "new"); err != nil {
		t.Fatal("new session must survive")
	}
	if msgs, _ := s.ListMessages(ctx, "new", 0, 0); len(msgs) != 1 {
		t.Fatalf("new session messages = %d, want 1", len(msgs))
	}

	for _, e := range []AuditEntry{
		{SessionID: "old", Action: "bash", Effect: "allow", CreatedAt: baseTime},
		{SessionID: "new", Action: "read", Effect: "allow", CreatedAt: baseTime.Add(time.Hour)},
	} {
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}
	if n, err := s.PruneAudit(ctx, cutoff, true); err != nil || n != 1 {
		t.Fatalf("PruneAudit dry-run = %d, %v", n, err)
	}
	if n, err := s.PruneAudit(ctx, cutoff, false); err != nil || n != 1 {
		t.Fatalf("PruneAudit = %d, %v", n, err)
	}
	if rows, _ := s.ListAudit(ctx, "old", 0); len(rows) != 0 {
		t.Fatalf("old audit rows survived: %d", len(rows))
	}
	if rows, _ := s.ListAudit(ctx, "new", 0); len(rows) != 1 {
		t.Fatalf("new audit rows = %d, want 1", len(rows))
	}
}

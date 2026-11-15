package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// makeCtx builds a manager with the heuristic counter for threshold tests.
// Thresholds are driven directly (like the legacy tests) so cases port 1:1.
func makeCtx(t *testing.T, cfg ContextConfig) *ContextManagerImpl {
	t.Helper()
	return NewContextManager(cfg, "You are a test agent.", nil, ContextDeps{SessionID: "s1"})
}

func turn(u, a string) []Message {
	return []Message{
		textMessage(RoleUser, u),
		textMessage(RoleAssistant, a),
	}
}

func TestBuildMessagesEmpty(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000})
	msgs, err := ctx.BuildMessages("hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2 (system + user)", len(msgs))
	}
	if msgs[0].Role != RoleSystem || msgs[len(msgs)-1].Role != RoleUser {
		t.Fatalf("roles = %v", msgs[0].Role)
	}
	if msgs[len(msgs)-1].Content[0].Text != "hello" {
		t.Fatalf("user text = %q", msgs[len(msgs)-1].Content[0].Text)
	}
}

func TestAppendNoHardTrim(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000, MaxRecentTurns: 2})
	for i := 0; i < 5; i++ {
		if err := ctx.Append(turn("q", "a")...); err != nil {
			t.Fatal(err)
		}
	}
	// Append never trims: compression is deferred to BuildMessages.
	if len(ctx.recent) != 10 {
		t.Fatalf("recent = %d, want 10", len(ctx.recent))
	}
}

func TestBudgetProperties(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000, ReservedTokens: 3500, TriggerRatio: 0.7})
	usable := 10000 - ctx.fixedCost() - 3500
	if ctx.usableTokens() != usable {
		t.Errorf("usable = %d, want %d", ctx.usableTokens(), usable)
	}
	if ctx.softLimitTokens() != int(float64(usable)*0.7) {
		t.Errorf("soft = %d, want %d", ctx.softLimitTokens(), int(float64(usable)*0.7))
	}
	if ctx.cfg.TriggerBytes != 204800 {
		t.Errorf("trigger bytes = %d, want 204800", ctx.cfg.TriggerBytes)
	}
}

func TestTokenTriggerCompresses(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000, MaxRecentTurns: 2})
	// Drive thresholds directly (legacy test technique).
	ctx.cfg.TriggerRatio = 0.0001 // soft limit ~0
	ctx.cfg.TriggerBytes = 1 << 30
	for i := 0; i < 4; i++ {
		_ = ctx.Append(turn(strings.Repeat("long question ", 20), strings.Repeat("long answer ", 20))...)
	}
	if _, err := ctx.BuildMessages("new input"); err != nil {
		t.Fatal(err)
	}
	if ctx.note == nil || ctx.note.Summary == "" {
		t.Fatal("compression should have produced a handoff note with summary")
	}
	// Protected recent turns survive (max_recent_turns=2 -> up to 4 messages).
	if len(ctx.recent) > 4 {
		t.Fatalf("recent = %d, want <= 4 (protected turns)", len(ctx.recent))
	}
}

func TestByteTriggerCompresses(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000, MaxRecentTurns: 2})
	ctx.cfg.TriggerRatio = 1.0 // soft limit huge
	ctx.cfg.TriggerBytes = 50  // byte line is the only trigger
	for i := 0; i < 4; i++ {
		_ = ctx.Append(turn("question-"+strings.Repeat("x", 40), "answer-"+strings.Repeat("y", 40))...)
	}
	if _, err := ctx.BuildMessages(strings.Repeat("z", 40)); err != nil {
		t.Fatal(err)
	}
	if ctx.note == nil || ctx.note.Summary == "" {
		t.Fatal("byte trigger should have compressed")
	}
}

func TestProtectedRecentTurns(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000, MaxRecentTurns: 3})
	ctx.cfg.TriggerRatio = 0.0001
	ctx.cfg.TriggerBytes = 1 << 30
	for i := 0; i < 10; i++ {
		_ = ctx.Append(turn("question "+strings.Repeat("x", 10), "answer "+strings.Repeat("y", 10))...)
	}
	if _, err := ctx.BuildMessages("final"); err != nil {
		t.Fatal(err)
	}
	// At least the protected window (3 turns = 6 messages) stays, and the
	// newest turn is intact.
	if len(ctx.recent) < 6 {
		t.Fatalf("recent = %d, want >= 6", len(ctx.recent))
	}
	last := ctx.recent[len(ctx.recent)-1]
	if last.Role != RoleAssistant || !strings.HasPrefix(last.Content[0].Text, "answer") {
		t.Fatalf("last message = %+v, want trailing assistant answer", last)
	}
}

// stubCompressor counts calls and returns a fixed summary.
type stubCompressor struct {
	calls int
	out   string
	err   error
}

// toolMsg builds a tool result message with one text part.
func toolMsg(id, text string) Message {
	return Message{Role: RoleTool, ToolCallID: id,
		Content: []Part{{Type: "text", Text: text}}, Timestamp: nowUTC()}
}

// TestFoldOldToolOutputs: oversized tool results from earlier turns become
// head/tail digests in the model window; the live turn (after the last user
// message) and small outputs stay complete; the store-side recent is untouched.
func TestFoldOldToolOutputs(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 100000})
	big := strings.Repeat("A", 6000) + strings.Repeat("中", 800) + strings.Repeat("B", 6000)
	_ = ctx.Append(
		Message{Role: RoleUser, Content: []Part{{Type: "text", Text: "旧问题"}}, Timestamp: nowUTC()},
		Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1"}}, Timestamp: nowUTC()},
		toolMsg("c1", big),
		textMessage(RoleAssistant, "旧回答"),
	)
	// Live turn: user + big tool result.
	_ = ctx.Append(
		Message{Role: RoleUser, Content: []Part{{Type: "text", Text: "新问题"}}, Timestamp: nowUTC()},
		Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c2"}}, Timestamp: nowUTC()},
		toolMsg("c2", big),
	)

	msgs, err := ctx.BuildMessages("")
	if err != nil {
		t.Fatal(err)
	}
	var old, live *Message
	for i := range msgs {
		if msgs[i].Role == RoleTool {
			if msgs[i].ToolCallID == "c1" {
				old = &msgs[i]
			} else {
				live = &msgs[i]
			}
		}
	}
	if old == nil || live == nil {
		t.Fatal("tool messages missing from window")
	}
	// Old: folded — head and tail kept, middle labeled, well under the
	// original ~15KB.
	if len(old.Content[0].Text) >= len(big) {
		t.Error("old tool output was not folded")
	}
	if !strings.Contains(old.Content[0].Text, "已折叠") ||
		!strings.HasPrefix(old.Content[0].Text, strings.Repeat("A", 700)) ||
		!strings.HasSuffix(old.Content[0].Text, strings.Repeat("B", 200)) {
		t.Errorf("folded digest lost head/tail: %q", old.Content[0].Text[:80])
	}
	// CJK tail intact (no mojibake from byte cuts).
	if !strings.Contains(old.Content[0].Text, "已折叠") && strings.Contains(old.Content[0].Text, "\xff") {
		t.Error("fold cut mid-rune")
	}
	// Live: complete.
	if live.Content[0].Text != big {
		t.Error("live turn tool output must stay complete")
	}
	// Store side untouched.
	if ctx.recent[2].Content[0].Text != big {
		t.Error("recent history must keep the full output (data layer never folds)")
	}
	// Disable: negative FoldToolBytes keeps everything verbatim.
	ctx.cfg.FoldToolBytes = -1
	msgs, _ = ctx.BuildMessages("")
	for _, m := range msgs {
		if m.Role == RoleTool && m.Content[0].Text != big {
			t.Error("folding must be disabled with a negative threshold")
		}
	}
}

func (s *stubCompressor) Compress(_ context.Context, msgs []Message, _ float64) (string, error) {
	s.calls++
	return s.out, s.err
}

func TestCompressionBatchesSingleCompressCall(t *testing.T) {
	comp := &stubCompressor{out: "summary"}
	ctx := NewContextManager(ContextConfig{ContextLimit: 10000, MaxRecentTurns: 1},
		"You are a test agent.", nil, ContextDeps{Counter: HeuristicCounter{}, Compressor: comp, SessionID: "s1"})
	ctx.cfg.TriggerRatio = 0.0001
	ctx.cfg.TriggerBytes = 1 << 30
	for i := 0; i < 5; i++ {
		_ = ctx.Append(turn(strings.Repeat("q", 100), strings.Repeat("a", 100))...)
	}
	if _, err := ctx.BuildMessages("new"); err != nil {
		t.Fatal(err)
	}
	// The fold must batch into one compressor call, not one per turn.
	if comp.calls != 1 {
		t.Fatalf("compressor calls = %d, want 1 (batched)", comp.calls)
	}
	if ctx.note == nil || ctx.note.Summary != "summary" {
		t.Fatalf("note summary = %+v", ctx.note)
	}
}

func TestSummarySecondaryCompression(t *testing.T) {
	ctx := NewContextManager(ContextConfig{ContextLimit: 10000, SummaryMaxTokens: 50},
		"You are a test agent.", nil, ContextDeps{Counter: HeuristicCounter{}, SessionID: "s1"})
	parts := make([]string, 400)
	for i := range parts {
		parts[i] = "fact"
	}
	ctx.note = &HandoffNote{Summary: strings.Join(parts, " ")}
	if ctx.summaryTokens() <= 50 {
		t.Fatalf("summary tokens = %d, want > 50", ctx.summaryTokens())
	}
	if changed := ctx.shrinkSummary(); !changed {
		t.Fatal("shrinkSummary should report change")
	}
	if ctx.summaryTokens() > 50 {
		t.Fatalf("after shrink tokens = %d, want <= 50", ctx.summaryTokens())
	}
	if !strings.HasPrefix(ctx.note.Summary, "…") {
		t.Fatalf("trimmed summary should start with ellipsis: %q", ctx.note.Summary[:10])
	}
}

func TestSummaryCapAutoRatio(t *testing.T) {
	ctx := NewContextManager(ContextConfig{ContextLimit: 10000, ReservedTokens: 3500, SummaryMaxRatio: 0.1},
		"You are a test agent.", nil, ContextDeps{Counter: HeuristicCounter{}, SessionID: "s1"})
	want := 1024
	if got := int(float64(ctx.usableTokens()) * 0.1); got > want {
		want = got
	}
	if ctx.summaryCapTokens() != want {
		t.Errorf("summary cap = %d, want %d", ctx.summaryCapTokens(), want)
	}
}

func TestTargetWatermarkDerivesFromTrigger(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{
		ContextLimit: 10000, ReservedTokens: 3500,
		TriggerRatio: 0.7, TriggerBytes: 204800, CompressTargetRatio: 0.5,
	})
	if ctx.targetTokens() != int(float64(ctx.softLimitTokens())*0.5) {
		t.Errorf("target tokens = %d", ctx.targetTokens())
	}
	if ctx.targetBytes() != int(204800*0.5) {
		t.Errorf("target bytes = %d", ctx.targetBytes())
	}
}

func TestTurnBoundarySkipsWhenToolPairIncomplete(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000, MaxRecentTurns: 1})
	ctx.cfg.TriggerRatio = 0.0001
	ctx.cfg.TriggerBytes = 1 << 30
	// Assistant message with a tool call whose result has not arrived yet.
	assistant := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}}
	_ = ctx.Append(textMessage(RoleUser, strings.Repeat("q", 200)), assistant)
	if _, err := ctx.BuildMessages("next"); err != nil {
		t.Fatal(err)
	}
	// Compression must NOT fire mid-tool: recent stays intact.
	if ctx.note != nil {
		t.Fatal("compression must not trigger with an unpaired tool call")
	}
	if len(ctx.recent) != 2 {
		t.Fatalf("recent = %d, want 2 (untouched)", len(ctx.recent))
	}
}

// TestBuildMessagesRepairsOrphanToolCalls: a tool call left without a result
// (crash, cancel) must not reach the model as an unanswered call.
func TestBuildMessagesRepairsOrphanToolCalls(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000})
	assistant := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}}
	_ = ctx.Append(textMessage(RoleUser, "run it"), assistant)

	out, err := ctx.BuildMessages("next")
	if err != nil {
		t.Fatal(err)
	}
	answered := false
	for i, m := range out {
		if len(m.ToolCalls) > 0 {
			if i+1 >= len(out) || out[i+1].Role != RoleTool || out[i+1].ToolCallID != "c1" {
				t.Fatalf("orphan call not repaired: %v", out)
			}
			answered = true
		}
	}
	if !answered {
		t.Fatal("assistant tool call missing from payload")
	}
	// The repair is a view: stored history stays untouched.
	if len(ctx.recent) != 2 {
		t.Fatalf("recent mutated by repair: %d", len(ctx.recent))
	}
}

// TestPairsCompleteRequiresResults: a call without a result also blocks the
// compression gate (previously only orphan results were detected).
func TestPairsCompleteRequiresResults(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000})
	_ = ctx.Append(
		Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1"}, {ID: "c2"}}},
		Message{Role: RoleTool, ToolCallID: "c1"},
	)
	if ctx.pairsComplete() {
		t.Fatal("c2 has no result: pairs must be incomplete")
	}
	_ = ctx.Append(Message{Role: RoleTool, ToolCallID: "c2"})
	if !ctx.pairsComplete() {
		t.Fatal("all calls answered: pairs must be complete")
	}
}

func TestTurnBoundaryCompressesWhenPairsComplete(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000, MaxRecentTurns: 1})
	ctx.cfg.TriggerRatio = 0.0001
	ctx.cfg.TriggerBytes = 1 << 30
	// Complete tool call/result pair.
	assistant := Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}}
	result := Message{Role: RoleTool, ToolCallID: "c1"}
	_ = ctx.Append(textMessage(RoleUser, strings.Repeat("q", 200)), assistant, result)
	if _, err := ctx.BuildMessages("next"); err != nil {
		t.Fatal(err)
	}
	if ctx.note == nil {
		t.Fatal("compression should fire when tool pairs are complete")
	}
}

func TestPopOldestTurnKeepsToolPairsIntact(t *testing.T) {
	msgs := []Message{
		textMessage(RoleUser, "u1"),
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: RoleTool, ToolCallID: "c1"},
		textMessage(RoleAssistant, "done1"),
		textMessage(RoleUser, "u2"),
		textMessage(RoleAssistant, "a2"),
	}
	moved := popOldestTurn(&msgs)
	// First turn (u1 + its assistant/tool run) is moved as one unit.
	if len(moved) != 4 {
		t.Fatalf("moved = %d messages, want 4 (u1 + assistant + tool + assistant)", len(moved))
	}
	if len(msgs) != 2 {
		t.Fatalf("remaining = %d, want 2 (u2 + a2)", len(msgs))
	}
	// No tool result may be separated from its call.
	for i, m := range moved {
		if m.Role == RoleTool && (i == 0 || moved[i-1].Role != RoleAssistant) {
			t.Fatal("tool result split from its assistant call")
		}
	}
}

func TestCompactProducesNoteAndClearsRecent(t *testing.T) {
	comp := &stubCompressor{out: "summary text"}
	ctx := NewContextManager(ContextConfig{ContextLimit: 10000, MaxRecentTurns: 1},
		"You are a test agent.", nil, ContextDeps{Counter: HeuristicCounter{}, Compressor: comp, SessionID: "s1"})
	_ = ctx.Append(turn("please do the thing", "done: the thing")...)
	note, err := ctx.Compact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if note.Intent != "please do the thing" {
		t.Errorf("intent = %q", note.Intent)
	}
	if len(note.Done) != 1 || note.Done[0] != "the thing" {
		t.Errorf("done = %v", note.Done)
	}
	if note.Summary != "summary text" {
		t.Errorf("summary = %q", note.Summary)
	}
	if len(ctx.recent) != 0 {
		t.Fatalf("recent = %d, want 0 after compact", len(ctx.recent))
	}
	// The note is now part of the LLM payload.
	msgs, err := ctx.BuildMessages("next")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		if m.Role == RoleSystem && strings.Contains(m.Content[0].Text, "handoff") {
			found = true
		}
	}
	if !found {
		t.Fatal("handoff note must be injected into the message window")
	}
}

// stubWriter records handoff writes; stubMirror records memory mirrors.
type stubWriter struct{ notes []HandoffNote }

func (w *stubWriter) WriteHandoff(_ context.Context, _ string, note HandoffNote) error {
	w.notes = append(w.notes, note)
	return nil
}

type stubMirror struct {
	kinds, texts []string
	err          error
}

func (m *stubMirror) Mirror(_ context.Context, kind, text string) error {
	m.kinds = append(m.kinds, kind)
	m.texts = append(m.texts, text)
	return m.err
}

func TestCompactPersistsAndMirrors(t *testing.T) {
	w := &stubWriter{}
	mir := &stubMirror{}
	ctx := NewContextManager(ContextConfig{ContextLimit: 10000},
		"You are a test agent.", nil,
		ContextDeps{Counter: HeuristicCounter{}, Writer: w, Mirror: mir, SessionID: "s1"})
	_ = ctx.Append(turn("goal", "decision: use Go\n correction: not Rust")...)
	if _, err := ctx.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.notes) != 1 {
		t.Fatalf("handoff writes = %d, want 1", len(w.notes))
	}
	// Decision and correction are double-written to memory; lessons are not.
	kinds := strings.Join(mir.kinds, ",")
	if !strings.Contains(kinds, "decision") || !strings.Contains(kinds, "correction") {
		t.Fatalf("mirrored kinds = %v, want decision + correction", mir.kinds)
	}
	if len(mir.texts) != 2 {
		t.Fatalf("mirrored texts = %v, want exactly 2 (no lessons)", mir.texts)
	}
}

func TestCompactDoesNotRemirrorCarriedForward(t *testing.T) {
	w := &stubWriter{}
	mir := &stubMirror{}
	ctx := NewContextManager(ContextConfig{ContextLimit: 10000},
		"You are a test agent.", nil,
		ContextDeps{Counter: HeuristicCounter{}, Writer: w, Mirror: mir, SessionID: "s1"})
	_ = ctx.Append(turn("goal", "decision: use Go")...)
	if _, err := ctx.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := len(mir.texts)
	// Second compact with no new content must not re-mirror the same decision.
	if _, err := ctx.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mir.texts) != first {
		t.Fatalf("mirrored texts grew from %d to %d; carried-forward entries must not re-mirror",
			first, len(mir.texts))
	}
}

func TestCompactPropagatesErrors(t *testing.T) {
	comp := &stubCompressor{err: errors.New("boom")}
	ctx := NewContextManager(ContextConfig{ContextLimit: 10000},
		"You are a test agent.", nil,
		ContextDeps{Counter: HeuristicCounter{}, Compressor: comp, SessionID: "s1"})
	_ = ctx.Append(turn("u", "a")...)
	if _, err := ctx.Compact(context.Background()); err == nil {
		t.Fatal("compressor error must propagate")
	}
}

func TestStatusReflectsUsage(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 100000, ReservedTokens: 4096})
	_ = ctx.Append(turn("hello world", "hi there")...)
	b := ctx.Status()
	if b.ContextLimit != 100000 {
		t.Errorf("context limit = %d", b.ContextLimit)
	}
	if b.HistoryTokens <= 0 {
		t.Errorf("history tokens = %d, want > 0", b.HistoryTokens)
	}
	if b.HistoryBytes <= 0 {
		t.Errorf("history bytes = %d, want > 0", b.HistoryBytes)
	}
	if b.SoftLimit != int(float64(b.HardLimit)*0.7) {
		t.Errorf("soft limit = %d", b.SoftLimit)
	}
}

func TestResetClearsState(t *testing.T) {
	ctx := makeCtx(t, ContextConfig{ContextLimit: 10000})
	_ = ctx.Append(turn("u", "a")...)
	ctx.note = &HandoffNote{Summary: "s"}
	ctx.Reset()
	if ctx.note != nil || len(ctx.recent) != 0 {
		t.Fatalf("reset left note=%v recent=%d", ctx.note, len(ctx.recent))
	}
}

func TestWithDefaults(t *testing.T) {
	zero := ContextConfig{}
	c := zero.withDefaults()
	if c.ReservedTokens != 4096 || c.TriggerRatio != 0.7 || c.TriggerBytes != 204800 ||
		c.CompressTargetRatio != 0.8 || c.MaxRecentTurns != 4 || c.SummaryRatio != 0.3 ||
		c.SummaryMaxRatio != 0.15 {
		t.Errorf("defaults wrong: %+v", c)
	}
}

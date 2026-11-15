package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"chyan.tech/chyan/internal/trace"
)

func openTraceSQLite(t *testing.T) *SQLite {
	t.Helper()
	s, err := NewSQLite(filepath.Join(t.TempDir(), "chyan.db"))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTraceSpansRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTraceSQLite(t)
	start := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	turn := trace.Span{
		ID: "t1", TraceID: "tr1", Kind: trace.KindTurn, Name: "turn",
		SessionID: "ses1", TaskID: "task1", AgentID: "ses1",
		Start: start, End: start.Add(3 * time.Second), Status: trace.StatusOK,
		Attrs: map[string]any{"model": "m1", "tokens_in": float64(42)},
	}
	tool := trace.Span{
		ID: "x1", TraceID: "tr1", ParentID: "t1", Kind: trace.KindToolExecution,
		Name: "bash", SessionID: "ses1", AgentID: "ses1",
		Start: start.Add(time.Second), End: start.Add(2 * time.Second),
		Status: trace.StatusError, ErrorClass: trace.ClassTimeout,
		Attrs: map[string]any{"result_bytes": float64(1234), "truncated": true},
	}
	for _, sp := range []trace.Span{turn, tool} {
		if err := s.AppendSpan(ctx, sp); err != nil {
			t.Fatalf("AppendSpan: %v", err)
		}
	}

	got, err := s.ListSpans(ctx, SpanQuery{TraceID: "tr1"})
	if err != nil {
		t.Fatalf("ListSpans: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("spans = %d", len(got))
	}
	if got[0].ID != "t1" || got[0].Attrs["model"] != "m1" {
		t.Errorf("turn row = %+v", got[0])
	}
	if got[1].ErrorClass != trace.ClassTimeout || got[1].Attrs["truncated"] != true {
		t.Errorf("tool row = %+v", got[1])
	}
	if d := got[0].Duration(); d != 3*time.Second {
		t.Errorf("duration = %s", d)
	}
	if bySession, err := s.ListSpans(ctx, SpanQuery{SessionID: "ses1"}); err != nil || len(bySession) != 2 {
		t.Errorf("session query = %d, %v", len(bySession), err)
	}
	if byTask, err := s.ListSpans(ctx, SpanQuery{TaskID: "task1"}); err != nil || len(byTask) != 1 {
		t.Errorf("task query = %d, %v", len(byTask), err)
	}
}

func TestAppendSpanValidatesIDs(t *testing.T) {
	s := openTraceSQLite(t)
	if err := s.AppendSpan(context.Background(), trace.Span{Kind: trace.KindTurn}); err == nil {
		t.Fatal("missing ids should fail")
	}
}

// TestListSpansNewestKeepsRecentSpans: with a limit, Newest must keep the most
// recent rows (the viewer's "latest turn" case) while the default order stays
// chronological for the CLI.
func TestListSpansNewestKeepsRecentSpans(t *testing.T) {
	ctx := context.Background()
	s := openTraceSQLite(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		sp := trace.Span{
			ID: "s" + string(rune('0'+i)), TraceID: "tr", Kind: trace.KindLLM,
			SessionID: "ses", Start: at, End: at.Add(time.Second), Status: trace.StatusOK,
		}
		if err := s.AppendSpan(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	oldest, err := s.ListSpans(ctx, SpanQuery{SessionID: "ses", Limit: 2})
	if err != nil || len(oldest) != 2 || oldest[0].ID != "s0" || oldest[1].ID != "s1" {
		t.Fatalf("chronological limit = %+v, %v", oldest, err)
	}
	newest, err := s.ListSpans(ctx, SpanQuery{SessionID: "ses", Limit: 2, Newest: true})
	if err != nil || len(newest) != 2 || newest[0].ID != "s4" || newest[1].ID != "s3" {
		t.Fatalf("newest limit = %+v, %v", newest, err)
	}
}

func TestPruneSpans(t *testing.T) {
	ctx := context.Background()
	s := openTraceSQLite(t)
	old := trace.Span{ID: "old", TraceID: "tr", Kind: trace.KindTurn,
		Start: time.Now().Add(-48 * time.Hour), End: time.Now().Add(-47 * time.Hour), Status: trace.StatusOK}
	fresh := trace.Span{ID: "new", TraceID: "tr", Kind: trace.KindTurn,
		Start: time.Now().Add(-time.Hour), End: time.Now().Add(-30 * time.Minute), Status: trace.StatusOK}
	if err := s.AppendSpan(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSpan(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	n, err := s.PruneSpans(ctx, cutoff, true)
	if err != nil || n != 1 {
		t.Fatalf("dry-run = %d, %v", n, err)
	}
	n, err = s.PruneSpans(ctx, cutoff, false)
	if err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}
	left, err := s.ListSpans(ctx, SpanQuery{TraceID: "tr"})
	if err != nil || len(left) != 1 || left[0].ID != "new" {
		t.Fatalf("left = %+v, %v", left, err)
	}
}

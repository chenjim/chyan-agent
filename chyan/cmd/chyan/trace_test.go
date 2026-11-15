package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/internal/trace"
)

type fakeSpanLister struct {
	spans   []trace.Span
	queries []store.SpanQuery
}

func (f *fakeSpanLister) ListSpans(_ context.Context, q store.SpanQuery) ([]trace.Span, error) {
	f.queries = append(f.queries, q)
	for _, sp := range f.spans {
		if q.TraceID != "" && sp.TraceID == q.TraceID {
			return f.spans, nil
		}
		if q.SessionID != "" && sp.SessionID == q.SessionID {
			return f.spans, nil
		}
		if q.TaskID != "" && sp.TaskID == q.TaskID {
			return f.spans, nil
		}
	}
	return nil, nil
}

func TestResolveSpansFallsBackFromTraceToSession(t *testing.T) {
	l := &fakeSpanLister{spans: []trace.Span{{ID: "x", TraceID: "tr", SessionID: "ses"}}}
	spans, err := resolveSpans(context.Background(), l, "ses", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 || spans[0].ID != "x" {
		t.Fatalf("spans = %+v", spans)
	}
	if len(l.queries) != 2 {
		t.Fatalf("queries = %d, want trace then session", len(l.queries))
	}
	if l.queries[1].SessionID != "ses" {
		t.Errorf("second query = %+v", l.queries[1])
	}
}

func TestTraceCmdJSONFromStore(t *testing.T) {
	home := t.TempDir()
	st, err := store.NewSQLite(home + "/chyan.db")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Second)
	for _, sp := range []trace.Span{
		{ID: "t", TraceID: "tr", Kind: trace.KindTurn, SessionID: "ses", TaskID: "task",
			Start: start, End: start.Add(time.Second), Status: trace.StatusOK},
		{ID: "l", TraceID: "tr", ParentID: "t", Kind: trace.KindLLM, SessionID: "ses", TaskID: "task",
			Start: start, End: start.Add(300 * time.Millisecond), Status: trace.StatusError, ErrorClass: trace.ClassTimeout},
	} {
		if err := st.AppendSpan(context.Background(), sp); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()

	cmd := traceCmd()
	cmd.SetArgs([]string{"task", "--home", home, "--json"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
	})
	for _, want := range []string{`"summary"`, `"spans"`, `"trace_id": "tr"`, `"error_class": "timeout"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in JSON output", want)
		}
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns the
// captured text (the trace command prints directly for shell use).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

func TestTraceCmdMissing(t *testing.T) {
	home := t.TempDir()
	if _, err := store.NewSQLite(home + "/chyan.db"); err != nil {
		t.Fatal(err)
	}
	cmd := traceCmd()
	cmd.SetArgs([]string{"nope", "--home", home})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected no-spans error")
	}
}

func TestFormatHelpers(t *testing.T) {
	if got := formatErrors(map[string]int{"timeout": 2, "internal": 1}); got != "internal×1, timeout×2" {
		t.Errorf("formatErrors = %q", got)
	}
	if got := shortID("abcdefghijklmn"); got != "abcdefghij" {
		t.Errorf("shortID = %q", got)
	}
	if got := ms(900); got != "900ms" {
		t.Errorf("ms(900) = %q", got)
	}
	if got := ms(1500); got != "1.5s" {
		t.Errorf("ms(1500) = %q", got)
	}
	if got := ms(61000); got != "1m01s" {
		t.Errorf("ms(61000) = %q", got)
	}
}

func TestPrintTraceShowsAttributionBuckets(t *testing.T) {
	base := time.Now()
	spans := []trace.Span{
		{ID: "t", TraceID: "tr", Kind: trace.KindTurn, Start: base, End: base.Add(4 * time.Second)},
		{ID: "l", TraceID: "tr", ParentID: "t", Kind: trace.KindLLM, Start: base, End: base.Add(2 * time.Second),
			Attrs: map[string]any{"tokens_in": 100, "attempts": 2}},
	}
	out := captureStdout(t, func() { printTrace(trace.Summarize(spans), spans) })
	for _, want := range []string{"trace tr", "模型", "工具", "等人工", "编排", "llm_request"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

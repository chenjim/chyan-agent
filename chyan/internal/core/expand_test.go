package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRefReader struct {
	res       RefContent
	err       error
	gotRef    string
	gotOffset int
	gotLimit  int
}

func (f *fakeRefReader) ExpandRef(_ context.Context, ref string, offset, limit int) (RefContent, error) {
	f.gotRef, f.gotOffset, f.gotLimit = ref, offset, limit
	return f.res, f.err
}

func TestExpandToolWindowAndContinuation(t *testing.T) {
	r := &fakeRefReader{res: RefContent{
		Text: "0123456789", Total: 20, Next: []string{"artifact:next.txt"},
	}}
	tool := NewExpandTool(r)
	out := tool.Execute(context.Background(), ToolCall{
		Name:  "expand",
		Input: map[string]any{"ref": " artifact:a.txt ", "offset": float64(0), "limit": float64(10)},
	})
	if out.Err != nil {
		t.Fatalf("expand: %v", out.Err)
	}
	if r.gotRef != "artifact:a.txt" || r.gotOffset != 0 || r.gotLimit != 10 {
		t.Fatalf("reader args = %q, %d, %d", r.gotRef, r.gotOffset, r.gotLimit)
	}
	if !strings.Contains(out.Content, `[more available: expand ref="artifact:a.txt" offset=10]`) {
		t.Fatalf("missing continuation hint: %q", out.Content)
	}
	if !strings.Contains(out.Content, "[next ref: artifact:next.txt]") {
		t.Fatalf("missing next ref: %q", out.Content)
	}
}

func TestExpandToolEndsWhenComplete(t *testing.T) {
	r := &fakeRefReader{res: RefContent{Text: "all of it", Total: 9}}
	out := NewExpandTool(r).Execute(context.Background(), ToolCall{
		Name: "expand", Input: map[string]any{"ref": "artifact:a.txt"},
	})
	if out.Err != nil {
		t.Fatalf("expand: %v", out.Err)
	}
	if strings.Contains(out.Content, "more available") {
		t.Fatalf("complete content must not hint at more: %q", out.Content)
	}
}

func TestExpandToolErrors(t *testing.T) {
	if out := NewExpandTool(nil).Execute(context.Background(), ToolCall{
		Name: "expand", Input: map[string]any{"ref": "artifact:a"},
	}); out.Err == nil {
		t.Fatal("nil reader must error")
	}
	if out := NewExpandTool(&fakeRefReader{}).Execute(context.Background(), ToolCall{
		Name: "expand", Input: map[string]any{},
	}); out.Err == nil || !strings.Contains(out.Err.Error(), "ref is required") {
		t.Fatalf("empty ref error = %v", out.Err)
	}
	r := &fakeRefReader{err: errors.New("boom")}
	if out := NewExpandTool(r).Execute(context.Background(), ToolCall{
		Name: "expand", Input: map[string]any{"ref": "artifact:a"},
	}); out.Err == nil || !strings.Contains(out.Err.Error(), "boom") {
		t.Fatalf("reader error = %v", out.Err)
	}
}

package app

import (
	"context"
	"strings"
	"sync"
	"testing"

	"chyan.tech/chyan/internal/core"
)

// lessonBackend records every request (title generation runs in the
// background and would otherwise overwrite a single lastRequest slot).
type lessonBackend struct {
	mu   sync.Mutex
	reqs []core.Request
}

func (b *lessonBackend) Chat(_ context.Context, req core.Request) (core.Response, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	b.mu.Unlock()
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: "ok"}}}}, nil
}

func (b *lessonBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	res, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: "ok"})
	}
	return res, err
}

// saw reports whether any recorded message contains want.
func (b *lessonBackend) saw(want string) bool {
	return b.sawRole("", want)
}

// sawSystem reports whether a system message contains want. Lessons must
// never appear there: retrieved memory stays on the user channel.
func (b *lessonBackend) sawSystem(want string) bool {
	return b.sawRole(core.RoleSystem, want)
}

// sawRole scans recorded messages, optionally restricted to one role ("" =
// any role).
func (b *lessonBackend) sawRole(role core.Role, want string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, req := range b.reqs {
		for _, m := range req.Messages {
			if role != "" && m.Role != role {
				continue
			}
			for _, p := range m.Content {
				if p.Type == "text" && strings.Contains(p.Text, want) {
					return true
				}
			}
		}
	}
	return false
}

// TestRunInjectsTriggeredLessons: a remembered trigger lesson surfaces on the
// user channel when the input matches, without an explicit recall.
func TestRunInjectsTriggeredLessons(t *testing.T) {
	backend := &lessonBackend{}
	a := newTestApp(t, backend)
	ctx := context.Background()
	if _, err := a.Remember(ctx, core.KindDirective, "部署前检查",
		[]string{"deploy"}, []string{"deploy 服务"}, "先跑 go test 再部署"); err != nil {
		t.Fatalf("remember: %v", err)
	}

	if _, _, err := a.RunOpts(ctx, "", "please deploy 服务 now", RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !backend.saw("关联经验") || !backend.saw("先跑 go test 再部署") {
		t.Fatal("lesson missing from the user turn")
	}
	if backend.sawSystem("关联经验") {
		t.Fatal("lesson leaked into the system prompt")
	}
}

// TestRunSkipsUnrelatedLessons: no trigger match, no injection.
func TestRunSkipsUnrelatedLessons(t *testing.T) {
	backend := &lessonBackend{}
	a := newTestApp(t, backend)
	ctx := context.Background()
	if _, err := a.Remember(ctx, core.KindDirective, "部署前检查",
		[]string{"deploy"}, []string{"deploy 服务"}, "先跑 go test 再部署"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, _, err := a.RunOpts(ctx, "", "hello there", RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if backend.saw("关联经验") {
		t.Fatal("unrelated prompt injected a lesson")
	}
}

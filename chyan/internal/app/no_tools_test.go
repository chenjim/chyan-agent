package app

import (
	"context"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

// runRequest returns the recorded agent request (the one carrying a system
// prompt) whose user text contains want. Title generation also records a
// request with the same user text but no system message, so it is skipped.
func runRequest(t *testing.T, b *lessonBackend, want string) core.Request {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, req := range b.reqs {
		if len(req.Messages) == 0 || req.Messages[0].Role != core.RoleSystem {
			continue
		}
		for _, m := range req.Messages {
			if m.Role != core.RoleUser {
				continue
			}
			for _, p := range m.Content {
				if p.Type == "text" && strings.Contains(p.Text, want) {
					return req
				}
			}
		}
	}
	t.Fatalf("no agent request carrying %q", want)
	return core.Request{}
}

// systemText flattens the system message of one request.
func systemText(req core.Request) string {
	for _, m := range req.Messages {
		if m.Role != core.RoleSystem {
			continue
		}
		var b strings.Builder
		for _, p := range m.Content {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// TestRunOptsNoToolsUsesOverridePrompt: a router-style turn gets no tools and
// exactly the caller-supplied prompt (no skills appendix, no lessons).
func TestRunOptsNoToolsUsesOverridePrompt(t *testing.T) {
	backend := &lessonBackend{}
	a := newTestApp(t, backend)
	ctx := context.Background()
	if _, err := a.Remember(ctx, core.KindDirective, "部署前检查",
		[]string{"deploy"}, []string{"deploy 服务"}, "LESSON-MARKER"); err != nil {
		t.Fatalf("remember: %v", err)
	}

	const prompt = "ROUTER-PROMPT-ONLY"
	if _, _, err := a.RunOpts(ctx, "", "please deploy 服务 now", RunOptions{
		NoTools: true, SystemOverride: prompt,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	req := runRequest(t, backend, "please deploy")
	if len(req.Tools) != 0 {
		t.Fatalf("tool-less turn exposed %d tools", len(req.Tools))
	}
	if got := systemText(req); got != prompt {
		t.Fatalf("system prompt = %q, want %q", got, prompt)
	}
}

// TestRunOptsDefaultKeepsTools: the default turn is unchanged (tools flow to
// the model), guarding against NoTools leaking into normal runs.
func TestRunOptsDefaultKeepsTools(t *testing.T) {
	backend := &lessonBackend{}
	a := newTestApp(t, backend)
	if _, _, err := a.RunOpts(context.Background(), "", "DEFAULT-TURN", RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	req := runRequest(t, backend, "DEFAULT-TURN")
	if len(req.Tools) == 0 {
		t.Fatal("default turn exposed no tools")
	}
}

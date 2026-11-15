// chat_test.go — chat REPL as a daemon client: built-in slash commands, agent
// turns and quit, driven with scripted input against an in-process daemon.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/llm"
)

// TestMain forces the offline token counter so chat tests never fetch BPE data.
func TestMain(m *testing.M) {
	llm.ForceHeuristicForTest()
	os.Exit(m.Run())
}

// chatStub replies with a fixed string and records the prompts it saw.
type chatStub struct {
	seen      []string
	images    int
	titles    int  // session-title passes (reasoning off), not conversation turns
	reasoning bool // attach a reasoning part to replies (history tests)
}

func (c *chatStub) Chat(_ context.Context, req core.Request) (core.Response, error) {
	// The title pass is not part of the conversation: count it separately so
	// prompt assertions keep tracking agent turns only.
	if req.ReasoningEffort == "none" {
		c.titles++
		return core.Response{Message: core.Message{Role: core.RoleAssistant,
			Content: []core.Part{{Type: "text", Text: "stub title"}}}}, nil
	}
	// all = full prompt context (history included) for cross-turn assertions.
	var all string
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == "image" {
				c.images++
			}
			if p.Text != "" {
				all += p.Text + "\n"
			}
		}
	}
	c.seen = append(c.seen, all)
	parts := []core.Part{{Type: "text", Text: "stub-reply"}}
	if c.reasoning {
		parts = append([]core.Part{{Type: "reasoning", Text: "secret thinking trace"}}, parts...)
	}
	return core.Response{Message: core.Message{Role: core.RoleAssistant, Content: parts}}, nil
}

func (c *chatStub) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	resp, err := c.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: "stub-reply"})
	}
	return resp, err
}

func TestChatTalksAndQuits(t *testing.T) {
	backend := &chatStub{}
	c, _ := newChatClient(t, backend)
	var out, errb bytes.Buffer
	err := runChat(context.Background(), c, chatOptions{workDir: t.TempDir()},
		bytes.NewBufferString("hello there\n/quit\n"), &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.seen) != 1 || !bytes.Contains([]byte(backend.seen[0]), []byte("hello there")) {
		t.Errorf("prompts = %v", backend.seen)
	}
	if backend.titles != 1 {
		t.Errorf("title passes = %d, want 1 (first turn names the session)", backend.titles)
	}
	if !bytes.Contains(out.Bytes(), []byte("stub-reply")) {
		t.Errorf("reply not streamed:\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("你：hello there")) {
		t.Errorf("user turn not echoed:\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("再见")) {
		t.Errorf("quit farewell missing:\n%s", out.String())
	}
}

func TestChatBuiltinCommands(t *testing.T) {
	backend := &chatStub{}
	c, _ := newChatClient(t, backend)
	var out, errb bytes.Buffer
	err := runChat(context.Background(), c, chatOptions{workDir: t.TempDir()},
		bytes.NewBufferString("/help\n/status\n/new\n/quit\n"), &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"内置命令", "/compact", "上下文窗口", "已开启新会话"} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if len(backend.seen) != 0 {
		t.Errorf("built-in commands must not hit the LLM: %v", backend.seen)
	}
}

func TestChatContinuesSessionAcrossTurns(t *testing.T) {
	backend := &chatStub{}
	c, _ := newChatClient(t, backend)
	var out, errb bytes.Buffer
	err := runChat(context.Background(), c, chatOptions{workDir: t.TempDir()},
		bytes.NewBufferString("first\nsecond\n/quit\n"), &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.seen) != 2 {
		t.Fatalf("prompts = %v", backend.seen)
	}
	if backend.titles != 1 {
		t.Errorf("title passes = %d, want 1 (second turn keeps the name)", backend.titles)
	}
	// Second turn must carry the first turn in history (same session).
	if !bytes.Contains([]byte(backend.seen[1]), []byte("first")) {
		t.Errorf("history lost across turns: %q", backend.seen[1])
	}
}

func TestSlashName(t *testing.T) {
	if got := slashName("/status"); got != "status" {
		t.Errorf("slashName(/status) = %q", got)
	}
	if got := slashName("/compact now"); got != "compact" {
		t.Errorf("slashName(/compact now) = %q", got)
	}
}

// TestPrintRecentHistory: switching sessions replays the latest user/assistant
// turns; tool rows, empty messages and reasoning traces are skipped.
func TestPrintRecentHistory(t *testing.T) {
	backend := &chatStub{reasoning: true}
	c, _ := newChatClient(t, backend)
	var out bytes.Buffer
	r := &chatRunner{client: c, out: &out, md: newMDRenderer(&out, true)}
	ctx := context.Background()

	// No content yet: the empty notice shows instead of an empty list.
	r.printRecentHistory(ctx, "")
	if !strings.Contains(out.String(), "暂无历史") {
		t.Errorf("empty session output:\n%s", out.String())
	}

	// Two turns in one session, then replay.
	session := ""
	if _, err := r.dispatch(ctx, "hello there", &session); err != nil {
		t.Fatal(err)
	}
	if _, err := r.dispatch(ctx, "second question", &session); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	r.printRecentHistory(ctx, session)
	got := out.String()
	for _, want := range []string{"最近历史", "你：hello there", "助手：stub-reply", "你：second question"} {
		if !strings.Contains(got, want) {
			t.Errorf("history output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "secret thinking trace") {
		t.Errorf("reasoning trace must stay hidden in replay:\n%s", got)
	}
}

// TestShellEscapeEntersContext: "!cmd" runs locally without an LLM call, but
// the command and its output are persisted as a user message — the NEXT turn's
// prompt must contain them (that is what "进上下文" means).
func TestShellEscapeEntersContext(t *testing.T) {
	backend := &chatStub{}
	c, _ := newChatClient(t, backend)
	var out, errb bytes.Buffer
	err := runChat(context.Background(), c, chatOptions{workDir: t.TempDir()},
		bytes.NewBufferString("!echo shell-escape-ok\n!ls /no-such-file-xyz\n!false\n!\n现在引用上面的命令输出\n/quit\n"), &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	// The typed command echoes ($ cmd) and its output shows.
	if !bytes.Contains(out.Bytes(), []byte("$ echo shell-escape-ok")) {
		t.Errorf("command echo missing:\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("shell-escape-ok")) {
		t.Errorf("shell output missing:\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("用法")) {
		t.Errorf("bare ! usage hint missing:\n%s", out.String())
	}
	if bytes.Contains(out.Bytes(), []byte("已记入上下文")) {
		t.Error("silent recording: no context note may be printed")
	}
	// Failed with stderr visible: no extra exit-code line on screen, but
	// recorded for the model. Failed silently: exit code shows on screen.
	if bytes.Count(out.Bytes(), []byte("[退出码")) != 1 {
		t.Errorf("exit code should appear exactly once (silent failure only):\n%s", out.String())
	}
	// The escapes never call the LLM: exactly one agent turn (the reference
	// question) plus one title pass after it.
	if len(backend.seen) != 1 || backend.titles != 1 {
		t.Errorf("LLM calls = %d (want 1 turn), titles = %d (want 1)", len(backend.seen), backend.titles)
	}
	// That turn's prompt sees the shell command and its output.
	if !bytes.Contains([]byte(backend.seen[0]), []byte("[shell] $ echo shell-escape-ok")) ||
		!bytes.Contains([]byte(backend.seen[0]), []byte("shell-escape-ok")) {
		t.Errorf("next prompt lost the shell note:\n%s", backend.seen[0])
	}
}

// TestChatCompletionsListsCommands: the Tab-completion candidate list covers
// the built-ins plus project commands.
func TestChatCompletionsListsCommands(t *testing.T) {
	c, _ := newChatClient(t, &chatStub{})
	r := &chatRunner{client: c}
	got := r.completions()
	for _, want := range []string{"/help", "/sessions", "/theme", "/quit"} {
		if !slices.Contains(got, want) {
			t.Errorf("completions missing %q: %v", want, got)
		}
	}
}

// TestChatReloadReloadsConfig: /reload calls the daemon endpoint so [review]
// and other settings take effect without restarting the REPL (U3).
func TestChatReloadReloadsConfig(t *testing.T) {
	backend := &chatStub{}
	c, a := newChatClient(t, backend)
	home := a.Home()
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[review]\nmode = \"human\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := &chatRunner{client: c, out: io.Discard, err: io.Discard, md: newMDRenderer(io.Discard, true)}
	r.out = &out
	session := ""
	if _, err := r.dispatch(context.Background(), "/reload", &session); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "已重新加载") || !strings.Contains(got, "审核 human") {
		t.Fatalf("reload output = %q", got)
	}
	// A broken config surfaces as an error and keeps the loaded settings.
	if err := os.WriteFile(cfgPath, []byte("[review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.dispatch(context.Background(), "/reload", &session); err == nil {
		t.Fatal("malformed config must report an error")
	}
	if got := a.ReviewMode(); got != core.ReviewHuman {
		t.Fatalf("mode changed after failed reload: %q", got)
	}
	if len(backend.seen) != 0 {
		t.Errorf("/reload must not hit the LLM: %v", backend.seen)
	}
}

// scriptedReader plays back fixed lines/aborts for loop tests.
type scriptedReader struct {
	lines []string
	errs  []error // per-ReadLine error override (nil = normal)
	i     int
}

func (s *scriptedReader) ReadLine(func() string) (string, error) {
	idx := s.i
	s.i++
	if idx < len(s.errs) && s.errs[idx] != nil {
		return "", s.errs[idx]
	}
	if idx < len(s.lines) {
		return s.lines[idx], nil
	}
	return "", io.EOF
}

func (s *scriptedReader) Close() error { return nil }

// TestLoopCancelKeepsSession: a canceled draft (Ctrl-C) prints a notice and
// shows the next prompt; it never kills the session.
func TestLoopCancelKeepsSession(t *testing.T) {
	c, _ := newChatClient(t, &chatStub{})
	var out bytes.Buffer
	r := &chatRunner{client: c, out: &out, err: &out,
		md: newMDRenderer(&out, true), banner: true,
		reader: &scriptedReader{
			lines: []string{"", "/quit"},
			errs:  []error{errCanceled, nil, nil},
		}}
	if err := r.loop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "已取消") {
		t.Errorf("cancel notice missing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "再见") {
		t.Errorf("session must survive cancel:\n%s", out.String())
	}
}

// TestChatSessionsSwitch: /sessions lists recent sessions and switching by
// index or id prefix continues that conversation.
func TestChatSessionsSwitch(t *testing.T) {
	backend := &chatStub{}
	c, _ := newChatClient(t, backend)
	ctx := context.Background()

	// Two conversations: "first" then /new then "second" creates two sessions.
	var out, errb bytes.Buffer
	r := &chatRunner{client: c, out: &out, err: &errb,
		md: newMDRenderer(&out, true), banner: true}
	session := ""
	if _, err := r.dispatch(ctx, "first question", &session); err != nil {
		t.Fatal(err)
	}
	first := session
	if _, err := r.dispatch(ctx, "/new", &session); err != nil {
		t.Fatal(err)
	}
	if _, err := r.dispatch(ctx, "second question", &session); err != nil {
		t.Fatal(err)
	}
	second := session
	if first == second {
		t.Fatal("/new must start a distinct session")
	}

	// List shows both sessions and marks the live one.
	out.Reset()
	if _, err := r.dispatch(ctx, "/sessions", &session); err != nil {
		t.Fatal(err)
	}
	listed := out.String()
	if !strings.Contains(listed, first) || !strings.Contains(listed, second) {
		t.Errorf("listing missing sessions:\n%s", listed)
	}
	if !strings.Contains(listed, "*") {
		t.Errorf("listing must mark the current session:\n%s", listed)
	}

	// Switch by index back to the older session (list is newest first).
	out.Reset()
	if _, err := r.dispatch(ctx, "/sessions 2", &session); err != nil {
		t.Fatal(err)
	}
	if session != first {
		t.Errorf("after /sessions 2: session=%s, want %s", session, first)
	}
	// And back by id prefix.
	out.Reset()
	if _, err := r.dispatch(ctx, "/sessions "+second[:12], &session); err != nil {
		t.Fatal(err)
	}
	if session != second {
		t.Errorf("prefix switch: session=%s, want %s", session, second)
	}
	// Bad index errors without changing the session.
	if _, err := r.dispatch(ctx, "/sessions 99", &session); err == nil {
		t.Error("out-of-range index must error")
	}
	if session != second {
		t.Errorf("failed switch must keep session, got %s", session)
	}
}

// TestChatImageCommand: /image attaches a picture to the turn (FR-10).
func TestChatImageCommand(t *testing.T) {
	backend := &chatStub{}
	c, _ := newChatClient(t, backend)
	png := filepath.Join(t.TempDir(), "shot.png")
	data, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(png, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	input := "/image " + png + " 这是什么\n/quit\n"
	if err := runChat(context.Background(), c, chatOptions{workDir: t.TempDir()},
		bytes.NewBufferString(input), &out, &errb); err != nil {
		t.Fatal(err)
	}
	if backend.images != 1 {
		t.Fatalf("image parts seen = %d, want 1", backend.images)
	}
	if !bytes.Contains(out.Bytes(), []byte("stub-reply")) {
		t.Fatalf("reply missing:\n%s", out.String())
	}
}

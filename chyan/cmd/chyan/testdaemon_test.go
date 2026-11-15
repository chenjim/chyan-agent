// testdaemon_test.go — in-process daemon for CLI client tests. The CLI no
// longer assembles an app: these tests exercise the real HTTP+SSE path the
// binary uses, with a scripted backend behind the daemon.
package main

import (
	"net/http/httptest"
	"testing"

	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/daemon"
	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/internal/tui"
)

// newChatClient builds an app + daemon over memory storage and returns a client
// wired to it. The app is returned for config assertions (e.g. review mode).
func newChatClient(t *testing.T, backend core.LLMBackend) (*tui.Client, *app.App) {
	t.Helper()
	home := t.TempDir()
	st := store.NewMemory()
	a, err := app.New(app.Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: backend, Store: st,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	s, err := daemon.New(daemon.Options{
		Home: home, App: a, Store: st, Token: "test-token",
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = a.Close()
	})
	return tui.NewClient(srv.URL, "test-token"), a
}

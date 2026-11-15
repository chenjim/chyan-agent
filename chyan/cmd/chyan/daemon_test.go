// daemon_test.go — CLI auto-start plumbing: health probing, readiness wait,
// failure diagnostics and the "off" switch.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chyan.tech/chyan/internal/tui"
)

func TestProbeDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	if err := probeDaemon(context.Background(), tui.NewClient(srv.URL, "")); err != nil {
		t.Fatalf("probe: %v", err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	if err := probeDaemon(context.Background(), tui.NewClient(dead.URL, "")); err == nil {
		t.Fatal("500 health must not count as ready")
	}
}

// TestWaitDaemonBecomesReady: a daemon that comes up after two probes is
// accepted; the call also proves the wait is bounded by the deadline.
func TestWaitDaemonBecomesReady(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := tui.NewClient(srv.URL, "")
	if err := waitDaemonUntil(context.Background(), c, t.TempDir(), 3*time.Second, nil); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if hits < 2 {
		t.Errorf("probes = %d", hits)
	}
}

// TestWaitDaemonTimeoutShowsLog: a dead daemon yields a timeout that points at
// the spawn log tail, so a failed auto-start is diagnosable.
func TestWaitDaemonTimeoutShowsLog(t *testing.T) {
	home := t.TempDir()
	entry := "listen unix " + filepath.Join(home, "chyan.sock") + ": address already in use\n"
	if err := os.WriteFile(filepath.Join(home, daemonLogName), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	// A localhost port that is not listening (listener closed immediately).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	err := waitDaemonUntil(context.Background(), tui.NewClient(url, ""), home, 300*time.Millisecond, nil)
	if err == nil {
		t.Fatal("dead daemon must time out")
	}
	if !strings.Contains(err.Error(), "启动超时") || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("timeout error = %v", err)
	}
}

// TestWaitDaemonChildExit: a child that dies immediately (bad config) fails
// fast with the log tail instead of waiting for the deadline.
func TestWaitDaemonChildExit(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, daemonLogName),
		[]byte("error: [llm].base_url is not configured\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	exited := make(chan error, 1)
	exited <- os.ErrInvalid
	start := time.Now()
	err := waitDaemonUntil(context.Background(), tui.NewClient(url, ""), home, 5*time.Second, exited)
	if err == nil {
		t.Fatal("dead child must fail")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("failure was not fast: %s", time.Since(start))
	}
	if !strings.Contains(err.Error(), "启动失败") || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("error = %v", err)
	}
}

// TestWaitDaemonSocketRace: when this CLI's child loses the bind race, the
// watcher must keep probing for the winner instead of reporting a startup
// failure.
func TestWaitDaemonSocketRace(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, daemonLogName),
		[]byte("listen unix /x/chyan.sock: bind: address already in use\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	time.AfterFunc(250*time.Millisecond, func() { ready.Store(true) })

	exited := make(chan error, 1)
	exited <- os.ErrDeadlineExceeded
	if err := waitDaemonUntil(context.Background(), tui.NewClient(srv.URL, ""), home, 3*time.Second, exited); err != nil {
		t.Fatalf("winner must be awaited: %v", err)
	}
}

// TestConnectDaemonAutostartOff: with the spawn disabled a missing socket is a
// clear error, not a 15s hang.
func TestConnectDaemonAutostartOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket path is not the Windows face")
	}
	t.Setenv("CHYAN_NO_AUTOSTART", "")
	home := t.TempDir()
	start := time.Now()
	_, err := connectDaemon(context.Background(), home, false)
	if err == nil || !strings.Contains(err.Error(), "daemon 未启动") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("probe did not fail fast: %s", time.Since(start))
	}
}

// TestTCPAddrOf: CHYAN_URL's host:port drives the spawned serve listener.
func TestTCPAddrOf(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:31415", "127.0.0.1:31415"},
		{"http://192.168.1.5:40000/", "192.168.1.5:40000"},
		{"http://127.0.0.1", ""}, // no port: keep serve's default
		{"", ""},
		{"::bad", ""},
	} {
		if got := tcpAddrOf(tc.in); got != tc.want {
			t.Errorf("tcpAddrOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTailLogLimitsLines keeps the failure output readable.
func TestTailLogLimitsLines(t *testing.T) {
	home := t.TempDir()
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("line")
		b.WriteByte(byte('0' + i%10))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(home, daemonLogName), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailLog(home, 5)
	if lines := strings.Count(got, "\n") + 1; lines != 5 {
		t.Errorf("lines = %d:\n%s", lines, got)
	}
}

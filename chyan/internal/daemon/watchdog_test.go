// Watchdog self-healing scan: stalled tasks, stale sessions, audit surges and
// the /healthz probe (P0).
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"chyan.tech/chyan/internal/store"
)

// TestWatchdogInterruptsStalledTask: a dispatch that stopped making progress
// past its budget is interrupted and reported.
func TestWatchdogInterruptsStalledTask(t *testing.T) {
	backend := newPoolBackend()
	defer close(backend.release)
	s, _ := newStabilityServer(t, backend, 2, time.Minute)
	h := s.Handler()

	task := createTask(t, h)
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("stalled task never reached the backend")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got, ok := s.getTask(task.ID); ok && got.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never reached running: %+v", task)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.tasksMu.Lock()
	ts := s.tasks[task.ID]
	s.tasksMu.Unlock()
	if ts == nil {
		t.Fatal("live task state missing")
	}
	ts.mu.Lock()
	ts.task.UpdatedAt = time.Now().UTC().Add(-time.Hour)
	ts.mu.Unlock()

	alerts := s.watchdogScan(context.Background(), time.Now().UTC())
	if len(alerts) == 0 || alerts[0].Kind != "task_stalled" {
		t.Fatalf("alerts = %+v, want task_stalled", alerts)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		got, _ := s.getTask(task.ID)
		if got.Status == "interrupted" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task status = %q, want interrupted", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWatchdogClosesStaleSessions: active sessions without updates for a day
// are closed; fresh ones are left alone.
func TestWatchdogClosesStaleSessions(t *testing.T) {
	backend := newPoolBackend()
	s, st := newStabilityServer(t, backend, 2, time.Minute)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	for _, se := range []store.Session{
		{ID: "stale1", Status: store.SessionStatusActive, CreatedAt: old, UpdatedAt: old},
		{ID: "fresh1", Status: store.SessionStatusActive, CreatedAt: now, UpdatedAt: now},
	} {
		if err := st.CreateSession(ctx, se); err != nil {
			t.Fatal(err)
		}
	}

	alerts := s.watchdogScan(ctx, now)
	if len(alerts) != 1 || alerts[0].Kind != "session_stale" || alerts[0].ID != "stale1" {
		t.Fatalf("alerts = %+v, want one session_stale", alerts)
	}
	stale, err := st.GetSession(ctx, "stale1")
	if err != nil || stale.Status != store.SessionStatusInterrupted {
		t.Fatalf("stale session = %+v, %v", stale, err)
	}
	fresh, err := st.GetSession(ctx, "fresh1")
	if err != nil || fresh.Status != store.SessionStatusActive {
		t.Fatalf("fresh session = %+v, %v", fresh, err)
	}
}

// TestWatchdogAuditDenySurge: a running session with repeated denied gated
// operations raises an alert.
func TestWatchdogAuditDenySurge(t *testing.T) {
	backend := newPoolBackend()
	defer close(backend.release)
	s, st := newStabilityServer(t, backend, 2, time.Minute)

	task := createTask(t, s.Handler())
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("task never reached the backend")
	}
	s.tasksMu.Lock()
	ts := s.tasks[task.ID]
	s.tasksMu.Unlock()
	ts.mu.Lock()
	ts.task.SessionID = "sess-deny"
	ts.mu.Unlock()

	now := time.Now().UTC()
	for i := 0; i < auditDenySurge; i++ {
		if err := st.AppendAudit(context.Background(), store.AuditEntry{
			SessionID: "sess-deny", Action: "bash", Resource: "rm -rf", Effect: "deny",
			CreatedAt: now.Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	alerts := s.watchdogScan(context.Background(), now)
	found := false
	for _, a := range alerts {
		if a.Kind == "audit_deny_surge" && a.ID == "sess-deny" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alerts = %+v, want audit_deny_surge", alerts)
	}
}

// TestHealthz: the probe reports liveness and live task counts without auth.
func TestHealthz(t *testing.T) {
	s, _, _ := newTestServer(t)
	w, req := doJSON(t, http.MethodGet, "/healthz", nil, "")
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz status = %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("healthz body: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("healthz status body = %v", got)
	}
	if _, ok := got["uptime_seconds"]; !ok {
		t.Fatalf("healthz lacks uptime: %v", got)
	}
}

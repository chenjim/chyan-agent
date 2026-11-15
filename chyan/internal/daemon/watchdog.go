// Watchdog: a periodic self-healing scan. It interrupts dispatches that
// stopped making progress, closes sessions left active with no updates, and
// raises an alert when one session accumulates deny verdicts. Every finding
// is published to SSE and pushed to the approval IM chat when configured.
package daemon

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/channels"
	"chyan.tech/chyan/internal/store"
)

const (
	// watchdogGrace tolerates a task that is exactly at its own timeout: the
	// per-task context cancels first, the watchdog only catches runs that
	// ignore cancellation.
	watchdogGrace = 5 * time.Minute
	// staleSessionAfter closes sessions that have had no update for this long.
	staleSessionAfter = 24 * time.Hour
	// auditDenyWindow / auditDenySurge flag one session with an unusual
	// number of denied gated operations inside a single window.
	auditDenyWindow = 10 * time.Minute
	auditDenySurge  = 5
)

// watchdogAlert is one self-healing action taken by a scan.
type watchdogAlert struct {
	Kind   string `json:"kind"` // task_stalled | session_stale | audit_deny_surge
	ID     string `json:"id"`
	Detail string `json:"detail"`
	// SessionID ties the alert to a session so the notice can be delivered
	// straight back to the IM thread that owns it.
	SessionID string `json:"session_id,omitempty"`
}

// watchdogLoop scans until the daemon stops. A non-positive interval disables
// the watchdog (tests keep default limits).
func (s *Server) watchdogLoop(ctx context.Context) {
	if s.watchdogInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, a := range s.watchdogScan(ctx, time.Now().UTC()) {
				s.notice(a)
			}
		}
	}
}

// watchdogScan performs one scan and returns the actions taken. Exported to
// tests through the same package.
func (s *Server) watchdogScan(ctx context.Context, now time.Time) []watchdogAlert {
	var alerts []watchdogAlert

	s.tasksMu.Lock()
	states := make([]*taskState, 0, len(s.tasks))
	for _, ts := range s.tasks {
		states = append(states, ts)
	}
	s.lastWatchdog = now
	s.tasksMu.Unlock()

	for _, ts := range states {
		snap := ts.snapshot()
		if snap.Status != "running" {
			continue
		}
		if s.taskTimeout > 0 && now.Sub(snap.UpdatedAt) > s.taskTimeout+watchdogGrace {
			if _, err := s.CancelTask(snap.ID); err != nil {
				log.Printf("watchdog: interrupt stalled task %s: %v", snap.ID, err)
				continue
			}
			alerts = append(alerts, watchdogAlert{
				Kind: "task_stalled", ID: snap.ID, SessionID: snap.SessionID,
				Detail: "任务 " + snap.ID + " 超过 " + (s.taskTimeout + watchdogGrace).String() + " 无进展，已中断",
			})
			continue
		}
		if snap.SessionID != "" && s.auditDenyCount(ctx, snap.SessionID, now) >= auditDenySurge {
			alerts = append(alerts, watchdogAlert{
				Kind: "audit_deny_surge", ID: snap.SessionID, SessionID: snap.SessionID,
				Detail: "会话 " + snap.SessionID + " " + auditDenyWindow.String() + " 内被拒绝操作达到 " + strconv.Itoa(auditDenySurge) + " 次",
			})
		}
	}

	sessions, err := s.store.ListSessions(ctx, 1000, 0)
	if err != nil {
		log.Printf("watchdog: list sessions: %v", err)
		return alerts
	}
	for _, se := range sessions {
		if se.Status != "" && se.Status != store.SessionStatusActive {
			continue
		}
		if se.UpdatedAt.IsZero() || now.Sub(se.UpdatedAt) < staleSessionAfter {
			continue
		}
		se.Status = store.SessionStatusInterrupted
		if err := s.store.UpdateSession(ctx, se); err != nil {
			log.Printf("watchdog: close stale session %s: %v", se.ID, err)
			continue
		}
		alerts = append(alerts, watchdogAlert{
			Kind: "session_stale", ID: se.ID, SessionID: se.ID,
			Detail: "会话 " + se.ID + " 超过 " + staleSessionAfter.String() + " 无更新，已标记为 interrupted",
		})
	}
	return alerts
}

// auditDenyCount counts denied gated operations for one session in the recent
// audit window.
func (s *Server) auditDenyCount(ctx context.Context, sessionID string, now time.Time) int {
	entries, err := s.store.ListAudit(ctx, sessionID, 50)
	if err != nil {
		log.Printf("watchdog: audit for %s: %v", sessionID, err)
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.Effect != "deny" {
			continue
		}
		if now.Sub(e.CreatedAt) <= auditDenyWindow {
			count++
		}
	}
	return count
}

// notice publishes one watchdog finding to SSE and pushes it to the approval
// IM chat when one is configured (best-effort: SSE is the source of truth).
func (s *Server) notice(a watchdogAlert) {
	log.Printf("watchdog: %s %s: %s", a.Kind, a.ID, a.Detail)
	s.hub.publish(Event{Name: api.EventNotice, Data: map[string]any{
		"kind": a.Kind, "id": a.ID, "detail": a.Detail, "at": time.Now().UTC(),
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	text := "[自检] " + a.Kind + "\n" + a.Detail
	// Prefer the IM thread that owns the session: it is where the user asked
	// for the work, not the approval chat.
	if s.deliverSession(ctx, a.SessionID, text) {
		return
	}
	if s.imApprover == nil || s.imApprover.defaultChat == "" {
		return
	}
	if err := s.imApprover.reg.Send(ctx, s.imApprover.channel, channels.Outgoing{
		ChatID: s.imApprover.defaultChat, Text: text,
	}); err != nil {
		log.Printf("watchdog: IM notice degraded: %v", err)
	}
}

// handleHealthz reports liveness and the live task counts
// (GET /healthz, unauthenticated like the PWA shell: it carries no data).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	s.tasksMu.Lock()
	running, pending := 0, 0
	for _, ts := range s.tasks {
		switch ts.snapshot().Status {
		case "running":
			running++
		case "pending":
			pending++
		}
	}
	lastWatchdog := s.lastWatchdog
	s.tasksMu.Unlock()
	out := map[string]any{
		"status": "ok", "tasks_running": running, "tasks_pending": pending,
	}
	if !s.startedAt.IsZero() {
		out["uptime_seconds"] = int64(now.Sub(s.startedAt).Seconds())
	}
	if !lastWatchdog.IsZero() {
		out["last_watchdog"] = lastWatchdog
	}
	writeJSON(w, http.StatusOK, out)
}

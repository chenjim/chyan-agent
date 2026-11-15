// Package daemon: ApprovalHub — pending approval tickets bridging the core
// Approver contract to remote HTTP/TUI resolution.
package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/core"
)

// ApprovalHub is the production ApprovalGate: it registers each ask-effect
// request as a pending ticket, then blocks until a human resolves it over the
// API (or ctx ends). It implements core.Approver so the permission engine can
// push decisions to humans (phone/TUI) with the change diff attached.
type ApprovalHub struct {
	mu      sync.Mutex
	pending map[string]*approvalEntry
	notify  func(Event)
}

// approvalEntry is one in-flight approval: its ticket plus the delivery
// channel back to the blocked Approve call.
type approvalEntry struct {
	ticket api.ApprovalTicket
	ch     chan approvalResult
}

// approvalResult is what Resolve hands back to the waiting Approve call.
type approvalResult struct{ approved bool }

// NewApprovalHub builds a hub; notify (may be nil) receives events for each
// new and resolved ticket.
func NewApprovalHub(notify func(Event)) *ApprovalHub {
	return &ApprovalHub{pending: map[string]*approvalEntry{}, notify: notify}
}

// SetNotify attaches the event sink (Server wires this to the SSE hub).
func (h *ApprovalHub) SetNotify(notify func(Event)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notify = notify
}

// newApprovalID generates a ticket identifier (same shape as newTaskID).
func newApprovalID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("apr%016x", buf), nil
}

// Begin registers one request as pending and returns its ticket id plus a
// wait func blocking until Resolve decides or ctx ends. Callers that need to
// act between ticket creation and waiting (e.g. IMApprover pushing the ticket
// to a chat) use this two-phase form; Approve is the one-shot wrapper.
func (h *ApprovalHub) Begin(req core.ApprovalRequest) (string, func(context.Context) (bool, error), error) {
	id, err := newApprovalID()
	if err != nil {
		return "", nil, err
	}
	// Verdict stays empty: the AI risk verdict is audited by the app layer,
	// not carried on the ticket.
	ticket := api.ApprovalTicket{
		ID: id, SessionID: req.SessionID, Action: req.Action, Resource: req.Resource,
		Diff: req.Diff, CreatedAt: time.Now().UTC(),
	}
	entry := &approvalEntry{ticket: ticket, ch: make(chan approvalResult, 1)}

	h.mu.Lock()
	h.pending[id] = entry
	notify := h.notify
	h.mu.Unlock()
	if notify != nil {
		notify(Event{Name: api.EventApprovalNew, Data: ticket})
	}
	return id, func(ctx context.Context) (bool, error) {
		select {
		case r := <-entry.ch:
			return r.approved, nil
		case <-ctx.Done():
			h.mu.Lock()
			if cur, ok := h.pending[id]; ok && cur == entry {
				delete(h.pending, id)
			}
			h.mu.Unlock()
			return false, ctx.Err()
		}
	}, nil
}

// Approve registers one request as pending and blocks until Resolve decides
// it or ctx ends. It implements core.Approver.
func (h *ApprovalHub) Approve(ctx context.Context, req core.ApprovalRequest) (bool, error) {
	_, wait, err := h.Begin(req)
	if err != nil {
		return false, err
	}
	return wait(ctx)
}

// Pending returns a creation-ordered snapshot of unresolved tickets.
func (h *ApprovalHub) Pending() []api.ApprovalTicket {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]api.ApprovalTicket, 0, len(h.pending))
	for _, e := range h.pending {
		out = append(out, e.ticket)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Resolve delivers a human decision to the blocked Approve call and drops the
// ticket. Unknown or already-resolved ids return an error (HTTP 409: the
// request is well-formed, the ticket state is gone).
func (h *ApprovalHub) Resolve(id, decision, reason string) error {
	if decision != "approve" && decision != "deny" {
		return errors.New("decision must be approve|deny")
	}
	h.mu.Lock()
	e, ok := h.pending[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("approval %s not pending", id)
	}
	delete(h.pending, id)
	notify := h.notify
	h.mu.Unlock()

	e.ch <- approvalResult{approved: decision == "approve"}
	if notify != nil {
		notify(Event{Name: api.EventApprovalDone,
			Data: api.ApprovalDecision{Decision: decision, Reason: reason}})
	}
	return nil
}

// compile-time check: the hub is the daemon gate and the core human approver.
var _ ApprovalGate = (*ApprovalHub)(nil)
var _ core.Approver = (*ApprovalHub)(nil)

package daemon

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/core"
)

// approverResult captures one blocking Approve outcome.
type approverResult struct {
	approved bool
	err      error
}

// startApprove runs Approve in the background and returns its result channel.
func startApprove(ctx context.Context, a core.Approver, req core.ApprovalRequest) <-chan approverResult {
	ch := make(chan approverResult, 1)
	go func() {
		ok, err := a.Approve(ctx, req)
		ch <- approverResult{approved: ok, err: err}
	}()
	return ch
}

// TestApprovalHubBegin covers the two-phase API: ticket registers first,
// then the wait func blocks until Resolve.
func TestApprovalHubBegin(t *testing.T) {
	h := NewApprovalHub(nil)
	id, wait, err := h.Begin(core.ApprovalRequest{
		Action: "bash", Resource: "x", Diff: "x", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if id == "" {
		t.Fatal("Begin must return a ticket id")
	}
	if p := h.Pending(); len(p) != 1 || p[0].ID != id {
		t.Fatalf("pending = %+v, want the begun ticket", p)
	} else if p[0].SessionID != "s-1" {
		t.Fatalf("ticket session = %q, want s-1", p[0].SessionID)
	}

	res := make(chan approverResult, 1)
	go func() {
		ok, werr := wait(context.Background())
		res <- approverResult{approved: ok, err: werr}
	}()
	if err := h.Resolve(id, "approve", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	select {
	case r := <-res:
		if r.err != nil || !r.approved {
			t.Fatalf("wait = (%v, %v), want approved", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after Resolve")
	}
	if len(h.Pending()) != 0 {
		t.Error("resolved ticket must leave pending")
	}
}

// TestApprovalHubBeginWaitCancel: an abandoned wait cleans the ticket up.
func TestApprovalHubBeginWaitCancel(t *testing.T) {
	h := NewApprovalHub(nil)
	_, wait, err := h.Begin(core.ApprovalRequest{Action: "write", Resource: "f"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan approverResult, 1)
	go func() {
		ok, werr := wait(ctx)
		res <- approverResult{approved: ok, err: werr}
	}()
	// Wait for registration so the cancel path is real.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(h.Pending()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case r := <-res:
		if r.err == nil || r.approved {
			t.Fatalf("cancelled wait = (%v, %v), want error", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after cancel")
	}
	if len(h.Pending()) != 0 {
		t.Error("cancelled ticket must leave pending")
	}
}

func TestApprovalHubResolveApproveAndDeny(t *testing.T) {
	for _, tc := range []struct {
		decision string
		approved bool
	}{
		{"approve", true},
		{"deny", false},
	} {
		h := NewApprovalHub(nil)
		res := startApprove(context.Background(), h, core.ApprovalRequest{Action: "bash", Resource: "rm x"})

		// Wait for registration, then resolve.
		var id string
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if p := h.Pending(); len(p) == 1 {
				id = p[0].ID
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if id == "" {
			t.Fatal("ticket was never registered")
		}
		if err := h.Resolve(id, tc.decision, "why"); err != nil {
			t.Fatalf("Resolve(%s): %v", tc.decision, err)
		}
		select {
		case r := <-res:
			if r.err != nil {
				t.Fatalf("Approve(%s): %v", tc.decision, r.err)
			}
			if r.approved != tc.approved {
				t.Errorf("Approve(%s) approved = %v", tc.decision, r.approved)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Approve(%s) did not return", tc.decision)
		}
		if len(h.Pending()) != 0 {
			t.Error("resolved ticket must leave pending")
		}
	}
}

func TestApprovalHubCtxCancel(t *testing.T) {
	h := NewApprovalHub(nil)
	ctx, cancel := context.WithCancel(context.Background())
	res := startApprove(ctx, h, core.ApprovalRequest{Action: "write", Resource: "f"})

	// Wait for registration so the cancel path is real, not pre-registration.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(h.Pending()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if len(h.Pending()) != 1 {
		t.Fatalf("pending = %d", len(h.Pending()))
	}
	cancel()
	select {
	case r := <-res:
		if r.err == nil || r.approved {
			t.Fatalf("cancelled Approve = (%v, %v), want error", r.approved, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after cancel")
	}
	if len(h.Pending()) != 0 {
		t.Error("cancelled ticket must leave pending")
	}
}

func TestApprovalHubResolveErrors(t *testing.T) {
	h := NewApprovalHub(nil)
	if err := h.Resolve("apr0000000000000000", "approve", ""); err == nil {
		t.Error("unknown id must error")
	}
	if err := h.Resolve("apr0000000000000000", "maybe", ""); err == nil {
		t.Error("invalid decision must error")
	}

	res := startApprove(context.Background(), h, core.ApprovalRequest{Action: "bash", Resource: "x"})
	var id string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if p := h.Pending(); len(p) == 1 {
			id = p[0].ID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := h.Resolve(id, "approve", ""); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	<-res
	if err := h.Resolve(id, "approve", ""); err == nil {
		t.Error("second Resolve must error")
	}
}

func TestApprovalHubPendingSnapshot(t *testing.T) {
	h := NewApprovalHub(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := []core.ApprovalRequest{
		{Action: "edit", Resource: "a.go", Diff: "--- a.go\n+++ a.go\n+one"},
		{Action: "bash", Resource: "rm -rf x", Diff: "rm -rf x"},
	}
	for _, req := range reqs {
		startApprove(ctx, h, req)
	}
	deadline := time.Now().Add(time.Second)
	var snap []api.ApprovalTicket
	for time.Now().Before(deadline) {
		if snap = h.Pending(); len(snap) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(snap) != 2 {
		t.Fatalf("pending = %+v", snap)
	}
	if !snap[0].CreatedAt.Before(snap[1].CreatedAt) && snap[0].CreatedAt != snap[1].CreatedAt {
		t.Error("snapshot must be creation-ordered")
	}
	byAction := map[string]string{snap[0].Action: snap[0].Diff, snap[1].Action: snap[1].Diff}
	if byAction["edit"] != reqs[0].Diff {
		t.Errorf("edit diff = %q", byAction["edit"])
	}
	if byAction["bash"] != reqs[1].Diff {
		t.Errorf("bash diff = %q", byAction["bash"])
	}
	for _, tk := range snap {
		if tk.ID == "" || tk.Verdict != "" {
			t.Errorf("ticket = %+v, want id and empty verdict", tk)
		}
	}
}

func TestApprovalHubNotifyEvents(t *testing.T) {
	var mu sync.Mutex
	var names []string
	h := NewApprovalHub(func(e Event) {
		mu.Lock()
		names = append(names, e.Name)
		mu.Unlock()
	})
	res := startApprove(context.Background(), h, core.ApprovalRequest{Action: "bash", Resource: "x"})

	var id string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if p := h.Pending(); len(p) == 1 {
			id = p[0].ID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := h.Resolve(id, "deny", "no"); err != nil {
		t.Fatal(err)
	}
	<-res
	mu.Lock()
	defer mu.Unlock()
	if len(names) != 2 || names[0] != api.EventApprovalNew || names[1] != api.EventApprovalDone {
		t.Errorf("notify names = %v, want [%s %s]", names, api.EventApprovalNew, api.EventApprovalDone)
	}
}

// TestApprovalHubWithReviewGate proves the hub plugs into the core review
// engine: a human resolve over the hub yields Approved=true.
func TestApprovalHubWithReviewGate(t *testing.T) {
	h := NewApprovalHub(nil)
	g := core.NewReviewGate(core.ReviewHuman, nil, h, 2*time.Second)

	type decision struct {
		d   core.GateDecision
		err error
	}
	out := make(chan decision, 1)
	go func() {
		d, err := g.DecideRequest(context.Background(), core.ApprovalRequest{
			Action: "bash", Resource: "deploy.sh", Diff: "deploy --prod",
		})
		out <- decision{d, err}
	}()

	// Find the pushed ticket and approve it as the human.
	var id string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if p := h.Pending(); len(p) == 1 {
			id = p[0].ID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("no ticket pushed")
	}
	if err := h.Resolve(id, "approve", "looks good"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("DecideRequest: %v", r.err)
		}
		if !r.d.Approved {
			t.Errorf("decision = %+v, want Approved", r.d)
		}
		if r.d.Source != "human" {
			t.Errorf("source = %q, want human", r.d.Source)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DecideRequest did not return")
	}
}

// TestServerWiresApprovalHubNotifier checks Server.New attaches the SSE hub
// to an *ApprovalHub gate.
func TestServerWiresApprovalHubNotifier(t *testing.T) {
	h := NewApprovalHub(nil)
	s, _, _ := newTestServerWithGate(t, h)
	ch, cancel := s.hub.subscribe()
	defer cancel()
	res := startApprove(context.Background(), h, core.ApprovalRequest{Action: "bash", Resource: "x"})

	select {
	case e := <-ch:
		if e.Name != api.EventApprovalNew {
			t.Fatalf("event = %q", e.Name)
		}
		tk, ok := e.Data.(api.ApprovalTicket)
		if !ok || tk.ID == "" {
			t.Fatalf("event data = %+v", e.Data)
		}
		if err := h.Resolve(tk.ID, "approve", ""); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval.requested not published")
	}
	<-res
	select {
	case e := <-ch:
		if e.Name != api.EventApprovalDone {
			t.Fatalf("event = %q", e.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("approval.resolved not published")
	}
}

// TestResolveAlreadyResolvedReturns409: a second decision on the same ticket is
// a state conflict (409), so the Web can refresh silently instead of showing
// "approval ... not pending" as if the click were malformed.
func TestResolveAlreadyResolvedReturns409(t *testing.T) {
	s, _, _ := newTestServer(t)
	hub := NewApprovalHub(nil)
	s.gate = hub
	h := s.Handler()

	go func() {
		_, _ = hub.Approve(context.Background(), core.ApprovalRequest{Action: "bash", Resource: "ls"})
	}()
	var id string
	for i := 0; i < 100; i++ {
		if tickets := hub.Pending(); len(tickets) > 0 {
			id = tickets[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("ticket never became pending")
	}

	w, req := doJSON(t, http.MethodPost, "/api/v1/approvals/"+id,
		api.ApprovalDecision{Decision: "approve"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first resolve = %d body=%s", w.Code, w.Body.String())
	}
	w, req = doJSON(t, http.MethodPost, "/api/v1/approvals/"+id,
		api.ApprovalDecision{Decision: "approve"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("second resolve = %d body=%s, want 409", w.Code, w.Body.String())
	}
}

// TestApprovalEndpointsNilGate keeps the nil-gate behaviour: empty list and 501.
func TestApprovalEndpointsNilGate(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.gate = nil
	h := s.Handler()

	w, req := doJSON(t, http.MethodGet, "/api/v1/approvals", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("nil gate list = %d %s", w.Code, w.Body.String())
	}
	w, req = doJSON(t, http.MethodPost, "/api/v1/approvals/t1",
		api.ApprovalDecision{Decision: "approve"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("nil gate resolve status = %d", w.Code)
	}
}

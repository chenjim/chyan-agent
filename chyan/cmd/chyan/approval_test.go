package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/tui"
)

// TestApprovalPromptDecision: y approves, n/EOF denies, and the diff preview
// reaches the human reviewer.
func TestApprovalPromptDecision(t *testing.T) {
	var out bytes.Buffer
	p := newApprovalPrompt(strings.NewReader("y\n"), &out, false)
	ok := p.decide(context.Background(), api.ApprovalTicket{
		Action: "bash", Resource: "rm -rf /tmp/x", Diff: "-old\n+new",
	})
	if !ok {
		t.Fatal("y must approve")
	}
	if !strings.Contains(out.String(), "-old") || !strings.Contains(out.String(), "rm -rf /tmp/x") {
		t.Errorf("preview missing from prompt:\n%s", out.String())
	}

	out.Reset()
	p = newApprovalPrompt(strings.NewReader("n\n"), &out, false)
	if p.decide(context.Background(), api.ApprovalTicket{Action: "bash", Resource: "ls"}) {
		t.Error("n must deny")
	}

	out.Reset()
	p = newApprovalPrompt(strings.NewReader(""), &out, false) // EOF: non-interactive deny
	if p.decide(context.Background(), api.ApprovalTicket{Action: "bash", Resource: "ls"}) {
		t.Error("EOF must deny")
	}
}

// TestApprovalPromptAutoAndCancel: --auto approves without reading, and a
// cancelled context settles the wait.
func TestApprovalPromptAutoAndCancel(t *testing.T) {
	p := newApprovalPrompt(strings.NewReader(""), io.Discard, true)
	if !p.decide(context.Background(), api.ApprovalTicket{Action: "bash", Resource: "ls"}) {
		t.Error("auto must approve")
	}

	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p = newApprovalPrompt(pr, io.Discard, false)
	if p.decide(ctx, api.ApprovalTicket{Action: "bash", Resource: "ls"}) {
		t.Error("cancelled ctx must deny")
	}
}

// TestSettleApprovalPostsDecision: the ticket decision goes over HTTP, and a
// ticket already resolved elsewhere (409) is not an error.
func TestSettleApprovalPostsDecision(t *testing.T) {
	var got api.ApprovalDecision
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/approvals/apr1" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := tui.NewClient(srv.URL, "")
	ticket := api.ApprovalTicket{ID: "apr1", Action: "bash", Resource: "rm x"}
	ctx := context.Background()

	if err := settleApproval(ctx, c, ticket, func(context.Context, api.ApprovalTicket) bool { return true }); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.Decision != "approve" {
		t.Errorf("decision = %q", got.Decision)
	}

	// A second click on a resolved ticket: 409 is silently accepted.
	status = http.StatusConflict
	if err := settleApproval(ctx, c, ticket, func(context.Context, api.ApprovalTicket) bool { return false }); err != nil {
		t.Fatalf("409 must be tolerated: %v", err)
	}
	if got.Decision != "deny" {
		t.Errorf("decision = %q", got.Decision)
	}
}

// TestTerminalError: interrupted maps to the exit-130 sentinel; failed carries
// the daemon's message.
func TestTerminalError(t *testing.T) {
	if err := terminalError(api.Task{Status: "interrupted"}); !errors.Is(err, core.ErrInterrupted) {
		t.Errorf("interrupted = %v, want ErrInterrupted", err)
	}
	err := terminalError(api.Task{Status: "failed", Err: "boom"})
	if err == nil || err.Error() != "boom" {
		t.Errorf("failed = %v", err)
	}
	if err := terminalError(api.Task{Status: "done"}); err != nil {
		t.Errorf("done = %v", err)
	}
}

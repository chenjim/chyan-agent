package app

import (
	"context"
	"testing"

	"chyan.tech/chyan/internal/core"
)

func TestSetModelOverridesConfig(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	a.SetModel("big-model")
	// modelOverride feeds buildAgent's AgentConfig; assert the field is set
	// and cleared on empty.
	if a.modelOverride != "big-model" {
		t.Errorf("modelOverride = %q", a.modelOverride)
	}
	a.SetModel("")
	if a.modelOverride != "" {
		t.Errorf("modelOverride after reset = %q", a.modelOverride)
	}
}

func TestForkSessionCopiesTranscript(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("hi")}})
	_, src, err := a.Run(context.Background(), "", "first turn")
	if err != nil {
		t.Fatal(err)
	}
	fork, err := a.ForkSession(context.Background(), src)
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if fork == "" || fork == src {
		t.Fatalf("fork = %q, source = %q", fork, src)
	}
	// Source transcript is preserved and duplicated into the fork.
	srcMsgs, err := a.store.ListMessages(context.Background(), src, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	forkMsgs, err := a.store.ListMessages(context.Background(), fork, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcMsgs) == 0 || len(forkMsgs) != len(srcMsgs) {
		t.Fatalf("source=%d fork=%d messages", len(srcMsgs), len(forkMsgs))
	}
	// Message ids differ between source and fork (no shared rows).
	for i := range forkMsgs {
		if forkMsgs[i].ID == srcMsgs[i].ID {
			t.Errorf("fork shares message id %q with source", forkMsgs[i].ID)
		}
		if forkMsgs[i].SessionID != fork {
			t.Errorf("fork message routed to %q", forkMsgs[i].SessionID)
		}
	}
	// Fork is the active session now.
	if a.currentSession != fork {
		t.Errorf("currentSession = %q, want fork %q", a.currentSession, fork)
	}
}

func TestForkSessionMissingSource(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	if _, err := a.ForkSession(context.Background(), "ses-ghost"); err == nil {
		t.Error("forking a missing session must fail")
	}
}

func TestForkEmptySession(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	// A freshly created empty session forks to another empty session.
	sid, err := a.resolveSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	fork, err := a.ForkSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if fork == sid {
		t.Error("fork must be a new session")
	}
}

// client_extra_test.go — daemon-client surface added for the CLI: session
// bootstrap/compact/notes, command listing, task polling, health and SSE
// readiness.
package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"chyan.tech/chyan/internal/api"
)

func TestCreateSession(t *testing.T) {
	var got api.CreateSessionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, "tok")
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusCreated, api.SessionSummary{ID: "ses1"})
	}))
	defer srv.Close()

	se, err := NewClient(srv.URL, "tok").CreateSession(context.Background(),
		api.CreateSessionRequest{Agent: "reviewer"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if se.ID != "ses1" || got.Agent != "reviewer" {
		t.Errorf("session = %+v, body = %+v", se, got)
	}
}

func TestSessionContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v1/sessions/ses%201/context" {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		writeJSON(w, http.StatusOK, api.ContextUsage{ContextLimit: 100, Used: 40})
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "").SessionContext(context.Background(), "ses 1")
	if err != nil {
		t.Fatalf("SessionContext: %v", err)
	}
	if got.ContextLimit != 100 || got.Used != 40 {
		t.Errorf("usage = %+v", got)
	}
}

func TestCompactSessionAndAppendNote(t *testing.T) {
	var noteBody api.AppendNoteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions/ses1/compact":
			if r.Method != http.MethodPost {
				t.Errorf("compact method = %s", r.Method)
			}
			writeJSON(w, http.StatusOK, api.CompactResult{
				Note: api.HandoffNote{Intent: "finish the CLI"},
			})
		case "/api/v1/sessions/ses1/note":
			if err := json.NewDecoder(r.Body).Decode(&noteBody); err != nil {
				t.Fatal(err)
			}
			writeJSON(w, http.StatusOK, api.AppendNoteResult{SessionID: "ses1"})
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	note, err := c.CompactSession(context.Background(), "ses1")
	if err != nil || note.Intent != "finish the CLI" {
		t.Fatalf("compact = %+v, %v", note, err)
	}
	sid, err := c.AppendNote(context.Background(), "ses1", "ls", "a.go\n")
	if err != nil || sid != "ses1" {
		t.Fatalf("note = %q, %v", sid, err)
	}
	if noteBody.Command != "ls" || noteBody.Output != "a.go\n" {
		t.Errorf("note body = %+v", noteBody)
	}
}

func TestListCommandsAndGetTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/commands":
			writeJSON(w, http.StatusOK, []api.CommandInfo{{Name: "review", Description: "审查"}})
		case "/api/v1/tasks/tsk1":
			writeJSON(w, http.StatusOK, api.Task{ID: "tsk1", Status: "done", Reply: "ok"})
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	cmds, err := c.ListCommands(context.Background())
	if err != nil || len(cmds) != 1 || cmds[0].Name != "review" {
		t.Fatalf("commands = %+v, %v", cmds, err)
	}
	task, err := c.GetTask(context.Background(), "tsk1")
	if err != nil || task.Status != "done" || task.Reply != "ok" {
		t.Fatalf("task = %+v, %v", task, err)
	}
}

func TestPingHealthz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "tok").Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestEventsWithReady: ready fires only after the stream is accepted.
func TestEventsWithReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: task.done\ndata: {\"id\":\"tsk1\"}\n\n"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan Event, 4)
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- NewClient(srv.URL, "").EventsWithReady(ctx, out, func() { ready <- struct{}{} })
	}()
	select {
	case <-ready:
	case <-done:
		t.Fatal("stream ended before ready")
	}
	ev := <-out
	if ev.Name != api.EventTaskDone {
		t.Errorf("event = %q", ev.Name)
	}
	cancel()
	<-done
}

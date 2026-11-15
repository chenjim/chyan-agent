// task_test.go — CLI task streaming over the daemon API.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/tui"
)

// TestRunDaemonTaskIgnoresTerminalSnapshot: the daemon publishes a final
// task.output carrying the accumulated reply; only running deltas are text.
func TestRunDaemonTaskIgnoresTerminalSnapshot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("tasks method = %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(api.Task{ID: "tsk1", SessionID: "ses1", Status: "pending"})
	})
	mux.HandleFunc("/api/v1/approvals", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`event: task.output` + "\n" + `data: {"id":"tsk1","status":"running","text":"a"}` + "\n\n",
			`event: task.output` + "\n" + `data: {"id":"tsk1","status":"done","text":"ab"}` + "\n\n",
			`event: task.done` + "\n" + `data: {"id":"tsk1","session_id":"ses1","status":"done","reply":"ab"}` + "\n\n",
		}
		for _, f := range frames {
			_, _ = fmt.Fprint(w, f)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var got strings.Builder
	task, err := runDaemonTask(context.Background(), tui.NewClient(srv.URL, ""),
		api.CreateTaskRequest{Prompt: "hello"},
		taskHooks{onText: func(s string) { got.WriteString(s) }})
	if err != nil {
		t.Fatalf("runDaemonTask: %v", err)
	}
	if task.Status != "done" || task.Reply != "ab" {
		t.Fatalf("task = %+v", task)
	}
	if got.String() != "a" {
		t.Fatalf("streamed = %q, want only the running delta", got.String())
	}
}

// TestRunDaemonTaskAsksOncePerTicket: a ticket visible in both the pending
// snapshot and the (already subscribed) SSE stream must prompt only once.
func TestRunDaemonTaskAsksOncePerTicket(t *testing.T) {
	var prompts, posts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Task{ID: "tsk1", SessionID: "ses1", Status: "pending"})
	})
	mux.HandleFunc("/api/v1/approvals", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]api.ApprovalTicket{{ID: "apr1", SessionID: "ses1", Action: "bash"}})
	})
	mux.HandleFunc("/api/v1/approvals/apr1", func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w,
			`event: approval.requested`+"\n"+
				`data: {"id":"apr1","session_id":"ses1","action":"bash"}`+"\n\n"+
				`event: task.done`+"\n"+
				`data: {"id":"tsk1","session_id":"ses1","status":"done","reply":"ok"}`+"\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := runDaemonTask(context.Background(), tui.NewClient(srv.URL, ""),
		api.CreateTaskRequest{Prompt: "hello"},
		taskHooks{approve: func(context.Context, api.ApprovalTicket) bool {
			prompts.Add(1)
			return true
		}})
	if err != nil {
		t.Fatalf("runDaemonTask: %v", err)
	}
	if prompts.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("prompts=%d posts=%d, want exactly one each", prompts.Load(), posts.Load())
	}
}

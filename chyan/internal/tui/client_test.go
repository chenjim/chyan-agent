// client_test.go — httptest fake daemon covering the REST client and SSE.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"chyan.tech/chyan/internal/api"
)

// writeJSON writes a JSON response body.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// checkAuth asserts the Bearer token when the test server was given one.
func checkAuth(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if want == "" {
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+want {
		t.Errorf("Authorization = %q, want Bearer %q", got, want)
	}
}

func TestListSessions(t *testing.T) {
	want := []api.SessionSummary{
		{ID: "ses1", Title: "first"},
		{ID: "ses2", Title: "second"},
	}
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, "tok")
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, want)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "tok") // trailing slash must be trimmed
	got, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if gotPath != "/api/v1/sessions" {
		t.Errorf("path = %q", gotPath)
	}
	if len(got) != 2 || got[0].ID != "ses1" {
		t.Errorf("got %+v", got)
	}
}

func TestGetSession(t *testing.T) {
	sess := api.Session{
		SessionSummary: api.SessionSummary{ID: "ses 1"},
		Messages:       []api.Message{{ID: "m1", Role: "user", Text: "hi"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/ses%201" && r.URL.Path != "/api/v1/sessions/ses 1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, sess)
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, "").GetSession(context.Background(), "ses 1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Text != "hi" {
		t.Errorf("got %+v", got)
	}
}

func TestCreateTask(t *testing.T) {
	var body api.CreateTaskRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeJSON(w, http.StatusCreated, api.Task{ID: "task1", Status: "pending"})
	}))
	defer srv.Close()

	req := api.CreateTaskRequest{Prompt: "do it", SessionID: "ses1"}
	task, err := NewClient(srv.URL, "").CreateTask(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.ID != "task1" {
		t.Errorf("task = %+v", task)
	}
	if body.Prompt != "do it" || body.SessionID != "ses1" {
		t.Errorf("body = %+v", body)
	}
}

func TestListApprovalsAndDecide(t *testing.T) {
	tickets := []api.ApprovalTicket{{ID: "ap1", Action: "bash", Resource: "rm -rf"}}
	var decided api.ApprovalDecision
	var decidePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, tickets)
		case r.Method == http.MethodPost:
			decidePath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&decided)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	list, err := c.ListApprovals(context.Background())
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(list) != 1 || list[0].Diff == "" && list[0].Action != "bash" {
		t.Errorf("list = %+v", list)
	}
	if err := c.DecideApproval(context.Background(), "ap1",
		api.ApprovalDecision{Decision: "approve", Reason: "ok"}); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if decidePath != "/api/v1/approvals/ap1" {
		t.Errorf("decidePath = %q", decidePath)
	}
	if decided.Decision != "approve" || decided.Reason != "ok" {
		t.Errorf("decided = %+v", decided)
	}
}

func TestGetStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, api.Status{
			Version: "0.1", Sessions: 3, PendingApprovals: 1, ReviewMode: "human",
		})
	}))
	defer srv.Close()

	s, err := NewClient(srv.URL, "").GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if s.PendingApprovals != 1 || s.ReviewMode != "human" {
		t.Errorf("status = %+v", s)
	}
}

func TestErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, api.Error{Error: "bad token"})
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "").GetStatus(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("err = %v", err)
	}

	// Non-JSON error body falls back to the status text.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv2.Close()
	if _, err := NewClient(srv2.URL, "").GetStatus(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
}

func TestNilClientUnavailable(t *testing.T) {
	var c *Client
	ctx := context.Background()
	if _, err := c.ListSessions(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ListSessions err = %v", err)
	}
	if _, err := c.GetSession(ctx, "x"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("GetSession err = %v", err)
	}
	if _, err := c.CreateTask(ctx, api.CreateTaskRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("CreateTask err = %v", err)
	}
	if _, err := c.ListApprovals(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ListApprovals err = %v", err)
	}
	if err := c.DecideApproval(ctx, "x", api.ApprovalDecision{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("DecideApproval err = %v", err)
	}
	if _, err := c.GetStatus(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("GetStatus err = %v", err)
	}
	if err := c.Events(ctx, nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Events err = %v", err)
	}
}

func TestEventsStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, "tok")
		if r.URL.Path != "/api/v1/events" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, ": keepalive\n\n")
		fmt.Fprint(w, "event: task.started\ndata: {\"id\":\"t1\",\"status\":\"running\"}\n\n")
		fmt.Fprint(w, "event: task.output\ndata: {\"id\":\"t1\",\"text\":\"hello\\nworld\"}\n\n")
		fmt.Fprint(w, "event: task.done\ndata: {\"id\":\"t1\",\"status\":\"done\",\"reply\":\"bye\"}\n\n")
		fmt.Fprint(w, "event: approval.requested\ndata: not-json\n\n")
		fmt.Fprint(w, "event: task.done\ndata: {\"id\":\"t2\",\"status\":\"done\"}") // no trailing blank
		f.Flush()
	}))
	defer srv.Close()

	out := make(chan Event, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := NewClient(srv.URL, "tok").Events(ctx, out)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	close(out)

	var evs []Event
	for ev := range out {
		evs = append(evs, ev)
	}
	if len(evs) != 5 {
		t.Fatalf("got %d events: %+v", len(evs), evs)
	}
	if evs[0].Name != api.EventTaskStarted {
		t.Errorf("evs[0] = %+v", evs[0])
	}
	if evs[1].TaskOutput() != "hello\nworld" {
		t.Errorf("TaskOutput = %q", evs[1].TaskOutput())
	}
	task, err := evs[2].Task()
	if err != nil || task.Reply != "bye" {
		t.Errorf("Task = %+v, %v", task, err)
	}
	if evs[3].Data != nil {
		t.Errorf("non-JSON data should be nil: %+v", evs[3].Data)
	}
	if !evs[4].IsTask() || evs[4].Name != api.EventTaskDone {
		t.Errorf("trailing frame = %+v", evs[4])
	}
}

func TestEventsCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
				fmt.Fprint(w, "event: task.output\ndata: {\"text\":\"tick\"}\n\n")
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	out := make(chan Event, 4)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for range out {
		}
	}()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := NewClient(srv.URL, "").Events(ctx, out)
	if err == nil {
		t.Fatal("want cancellation error")
	}
}

func TestEventsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, api.Error{Error: "no token"})
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "").Events(context.Background(), make(chan Event, 1))
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("err = %v", err)
	}
}

func TestEventHelpers(t *testing.T) {
	ev := Event{Name: "task.output", Data: map[string]any{"output": "streamed"}}
	if !ev.IsTask() {
		t.Error("IsTask = false")
	}
	if ev.TaskOutput() != "streamed" {
		t.Errorf("TaskOutput = %q", ev.TaskOutput())
	}
	ev2 := Event{Name: "approval.requested"}
	if ev2.IsTask() {
		t.Error("approval should not be a task event")
	}
	if ev2.TaskOutput() != "" {
		t.Errorf("TaskOutput = %q", ev2.TaskOutput())
	}
	if _, err := ev2.Task(); err == nil {
		t.Error("Task on nil data should fail")
	}
}

func TestParseEventData(t *testing.T) {
	if parseEventData("") != nil {
		t.Error("empty payload should be nil")
	}
	if parseEventData("plain text") != nil {
		t.Error("non-JSON payload should be nil")
	}
	m := parseEventData(`{"a":1}`)
	if m == nil {
		t.Fatal("JSON object should decode")
	}
}

func TestEventsDataOnlyFrame(t *testing.T) {
	// A data-only frame (no event: line) must not drop the payload.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"a\":1}\n\n")
	}))
	defer srv.Close()

	out := make(chan Event, 4)
	if err := NewClient(srv.URL, "").Events(context.Background(), out); err != nil {
		t.Fatalf("Events: %v", err)
	}
	close(out)
	ev := <-out
	if ev.Name != "" || ev.Data["a"].(float64) != 1 {
		t.Fatalf("ev = %+v", ev)
	}
}

func TestRunAgainstFakeDaemon(t *testing.T) {
	// Run drives a full tea program: fake daemon + immediate quit key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		writeJSON(w, http.StatusOK, []any{})
	}))
	defer srv.Close()

	p := tea.NewProgram(newModel(context.Background(), NewClient(srv.URL, ""), nil),
		tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer())
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.Quit()
	}()
	if _, err := p.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestClientOverUnixSocket: the daemon's local listener is a unix socket;
// the client must dial it and reach the REST surface without a token.
func TestClientOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "chyan.sock")
	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Skipf("unix listen unavailable: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"ses-1","updated_at":"2026-01-01T00:00:00Z"}]`))
	})}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewClient("http://unix"+sock, "")
	list, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions over unix socket: %v", err)
	}
	if len(list) != 1 || list[0].ID != "ses-1" {
		t.Fatalf("sessions = %+v", list)
	}
}

// TestUnixSocketOf: URL parsing for the http://unix/<path> scheme.
func TestUnixSocketOf(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"http://unix/home/u/chyan.sock", "/home/u/chyan.sock", true},
		{"http://unix/tmp/a.sock", "/tmp/a.sock", true},
		{"http://127.0.0.1:31415", "", false},
		{"http://unix", "", false},
		{"http://unix/", "", false},
	} {
		got, ok := unixSocketOf(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("unixSocketOf(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/api"
)

func waitTaskDone(t *testing.T, s *Server, id string) api.Task {
	t.Helper()
	h := s.Handler()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w, req := doJSON(t, http.MethodGet, "/api/v1/tasks/"+id, nil, "test-token")
		h.ServeHTTP(w, req)
		var got api.Task
		if err := json.Unmarshal(w.Body.Bytes(), &got); err == nil && got.Status == "done" {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not complete")
	return api.Task{}
}

func TestWorkspaceInfoAndTaskSelection(t *testing.T) {
	s, a, st := newTestServer(t)
	ctx := context.Background()
	h := s.Handler()

	// Default workspace is always reported first.
	w, req := doJSON(t, http.MethodGet, "/api/v1/workspace", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("workspace status = %d", w.Code)
	}
	var info api.WorkspaceInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Path != a.WorkDir() || len(info.Recent) == 0 || info.Recent[0] != a.WorkDir() {
		t.Errorf("workspace info = %+v", info)
	}

	// A bad explicit path fails the request before enqueueing.
	w, req = doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{
		Prompt: "x", WorkDir: filepath.Join(t.TempDir(), "missing"),
	}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad workspace status = %d body=%s", w.Code, w.Body.String())
	}

	alt := t.TempDir()
	w, req = doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{
		Prompt: "hello", WorkDir: alt,
	}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	var created api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	done := waitTaskDone(t, s, created.ID)
	if done.SessionID == "" {
		t.Fatal("task has no session")
	}
	raw, err := st.GetKV(ctx, workspaceSessionKey(done.SessionID))
	if err != nil || string(raw) != alt {
		t.Fatalf("session workspace = %q, %v", raw, err)
	}

	// Continuing that session reuses its recorded workspace.
	dir, err := s.taskWorkDir(ctx, api.CreateTaskRequest{SessionID: done.SessionID})
	if err != nil || dir != alt {
		t.Fatalf("continued workspace = %q, %v", dir, err)
	}

	// The selection shows up in the recent list.
	info = s.workspaceInfo(ctx)
	found := false
	for _, p := range info.Recent {
		if p == alt {
			found = true
		}
	}
	if !found {
		t.Errorf("recent = %v, missing %s", info.Recent, alt)
	}
}

func TestFilesEndpointUsesSelectedWorkspace(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	alt := t.TempDir()
	if err := os.WriteFile(filepath.Join(alt, "only.txt"), []byte("selected"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, req := doJSON(t, http.MethodGet, "/api/v1/files?workspace="+alt, nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "only.txt") {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}

	w, req = doJSON(t, http.MethodGet, "/api/v1/files/content?workspace="+alt+"&path=only.txt", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "selected") {
		t.Fatalf("content = %d %s", w.Code, w.Body.String())
	}

	w, req = doJSON(t, http.MethodGet, "/api/v1/files?workspace="+filepath.Join(alt, "missing"), nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad workspace status = %d", w.Code)
	}
}

// TestWorkbenchFlow walks the FR-12 A13 loop at the API level: select a
// workspace, run a task, read the transcript, continue the session in the
// same workspace, export it, and download a file from it.
func TestWorkbenchFlow(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	alt := t.TempDir()
	if err := os.WriteFile(filepath.Join(alt, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{
		Prompt: "hello", WorkDir: alt,
	}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	var created api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	done := waitTaskDone(t, s, created.ID)
	if done.SessionID == "" {
		t.Fatal("task has no session")
	}

	// Transcript is readable and exportable.
	w, req = doJSON(t, http.MethodGet, "/api/v1/sessions/"+done.SessionID, nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "stub reply") {
		t.Fatalf("session = %d %s", w.Code, w.Body.String())
	}
	w, req = doJSON(t, http.MethodGet, "/api/v1/sessions/"+done.SessionID+"/export", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"format_version"`) {
		t.Fatalf("export = %d %s", w.Code, w.Body.String())
	}

	// Continuing the session reuses the recorded workspace.
	w, req = doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{
		Prompt: "again", SessionID: done.SessionID,
	}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("continue = %d %s", w.Code, w.Body.String())
	}
	var second api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if got := waitTaskDone(t, s, second.ID); got.SessionID != done.SessionID {
		t.Fatalf("continue session = %q, want %q", got.SessionID, done.SessionID)
	}
	raw, err := s.store.GetKV(context.Background(), workspaceSessionKey(done.SessionID))
	if err != nil || string(raw) != alt {
		t.Fatalf("workspace KV = %q, %v", raw, err)
	}

	// Files in the selected workspace are listable and downloadable.
	w, req = doJSON(t, http.MethodGet, "/api/v1/files?workspace="+alt, nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "notes.txt") {
		t.Fatalf("files = %d %s", w.Code, w.Body.String())
	}
	w, req = doJSON(t, http.MethodGet, "/api/v1/files/download?workspace="+alt+"&path=notes.txt", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Fatalf("download = %d %q", w.Code, w.Body.String())
	}

	// The selection appears in the recent workspace list (default stays first).
	info := s.workspaceInfo(context.Background())
	found := false
	for _, p := range info.Recent {
		if p == alt {
			found = true
		}
	}
	if !found {
		t.Errorf("recent = %v, missing %s", info.Recent, alt)
	}
}

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
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

func seedSession(t *testing.T, st store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateSession(ctx, store.Session{
		ID: id, Title: "原始标题", Model: "gpt-test",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMessage(ctx, store.Message{
		ID: "m1", SessionID: id, Role: "user",
		Content:   []byte(`[{"type":"text","text":"hello"}]`),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateSession(t *testing.T) {
	s, _, st := newTestServer(t)
	seedSession(t, st, "sess1")
	h := s.Handler()

	w, req := doJSON(t, http.MethodPatch, "/api/v1/sessions/sess1", api.UpdateSessionRequest{Title: "  新标题  "}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rename status = %d body=%s", w.Code, w.Body.String())
	}
	var got api.SessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "新标题" {
		t.Errorf("title = %q", got.Title)
	}
	se, err := st.GetSession(context.Background(), "sess1")
	if err != nil || se.Title != "新标题" {
		t.Fatalf("stored title = %q, %v", se.Title, err)
	}

	// Too long (21 runes) is rejected; exactly 20 is allowed.
	long := strings.Repeat("界", 21)
	w, req = doJSON(t, http.MethodPatch, "/api/v1/sessions/sess1", api.UpdateSessionRequest{Title: long}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("long title status = %d", w.Code)
	}

	max := strings.Repeat("界", 20)
	w, req = doJSON(t, http.MethodPatch, "/api/v1/sessions/sess1", api.UpdateSessionRequest{Title: max}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("max title status = %d body=%s", w.Code, w.Body.String())
	}

	w, req = doJSON(t, http.MethodPatch, "/api/v1/sessions/missing", api.UpdateSessionRequest{Title: "x"}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing session status = %d", w.Code)
	}

	// A malformed body fails before touching the store.
	w, req = doJSON(t, http.MethodPatch, "/api/v1/sessions/sess1", "not json", "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d", w.Code)
	}
}

func TestSessionContext(t *testing.T) {
	s, _, st := newTestServer(t)
	seedSession(t, st, "sess1")
	h := s.Handler()

	w, req := doJSON(t, http.MethodGet, "/api/v1/sessions/sess1/context", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("context status = %d body=%s", w.Code, w.Body.String())
	}
	var got api.ContextUsage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	w, req = doJSON(t, http.MethodGet, "/api/v1/sessions/missing/context", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing context status = %d", w.Code)
	}
}

func TestDeleteSession(t *testing.T) {
	s, _, st := newTestServer(t)
	seedSession(t, st, "sess1")
	h := s.Handler()

	w, req := doJSON(t, http.MethodDelete, "/api/v1/sessions/sess1", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", w.Code, w.Body.String())
	}
	if _, err := st.GetSession(context.Background(), "sess1"); err == nil {
		t.Fatal("session still present after delete")
	}

	w, req = doJSON(t, http.MethodDelete, "/api/v1/sessions/sess1", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d", w.Code)
	}

	// A live task pins the session: deleting it mid-run would orphan the run.
	seedSession(t, st, "running")
	s.tasksMu.Lock()
	s.tasks["t-run"] = &taskState{task: api.Task{ID: "t-run", SessionID: "running", Status: "running"}}
	s.tasksMu.Unlock()
	w, req = doJSON(t, http.MethodDelete, "/api/v1/sessions/running", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("running delete status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestExportSession(t *testing.T) {
	s, _, st := newTestServer(t)
	seedSession(t, st, "sess1")
	h := s.Handler()

	w, req := doJSON(t, http.MethodGet, "/api/v1/sessions/sess1/export", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("export status = %d body=%s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "chyan-session-sess1.json") {
		t.Errorf("content-disposition = %q", cd)
	}
	var ex store.SessionExport
	if err := json.Unmarshal(w.Body.Bytes(), &ex); err != nil {
		t.Fatal(err)
	}
	if ex.FormatVersion != store.ExportFormatVersion || len(ex.Messages) != 1 || !strings.Contains(string(ex.Messages[0].Content), "hello") {
		t.Errorf("export envelope = %+v", ex)
	}

	w, req = doJSON(t, http.MethodGet, "/api/v1/sessions/missing/export", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing export status = %d", w.Code)
	}
}

func TestDownloadFile(t *testing.T) {
	s, a, _ := newTestServer(t)
	h := s.Handler()
	if err := os.WriteFile(filepath.Join(a.WorkDir(), "note.txt"), []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, req := doJSON(t, http.MethodGet, "/api/v1/files/download?path=note.txt", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "body" {
		t.Fatalf("download = %d %q", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "note.txt") {
		t.Errorf("content-disposition = %q", cd)
	}

	// Directories are not downloadable.
	w, req = doJSON(t, http.MethodGet, "/api/v1/files/download?path=.", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("directory download status = %d", w.Code)
	}

	// A symlink escaping the workspace is refused.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(a.WorkDir(), "link.txt")); err != nil {
		t.Fatal(err)
	}
	w, req = doJSON(t, http.MethodGet, "/api/v1/files/download?path=link.txt", nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("symlink escape status = %d body=%s", w.Code, w.Body.String())
	}

	// The artifact root is a second sandbox: absolute paths inside it work,
	// anything outside is refused.
	root := a.ArtifactRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(root, "out.txt")
	if err := os.WriteFile(artifact, []byte("spilled"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, req = doJSON(t, http.MethodGet,
		"/api/v1/files/download?source=artifacts&path="+artifact, nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "spilled" {
		t.Fatalf("artifact download = %d %q", w.Code, w.Body.String())
	}

	w, req = doJSON(t, http.MethodGet,
		"/api/v1/files/download?source=artifacts&path="+filepath.Join(outside, "secret.txt"), nil, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("outside artifact status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestAttachmentName(t *testing.T) {
	cases := map[string]string{
		`a"b.txt`:   "a_b.txt",
		"a\nb.txt":  "a_b.txt",
		`a\b.txt`:   "a_b.txt",
		"":          "download",
		".":         "download",
		"..":        "download",
		"normal.go": "normal.go",
	}
	for in, want := range cases {
		if got := attachmentName(in); got != want {
			t.Errorf("attachmentName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusRunningTasks(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.tasksMu.Lock()
	s.tasks["a"] = &taskState{task: api.Task{ID: "a", Status: "running"}}
	s.tasks["b"] = &taskState{task: api.Task{ID: "b", Status: "pending"}}
	s.tasks["c"] = &taskState{task: api.Task{ID: "c", Status: "done"}}
	s.tasksMu.Unlock()

	w, req := doJSON(t, http.MethodGet, "/api/v1/status", nil, "test-token")
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got api.Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RunningTasks != 2 {
		t.Errorf("running tasks = %d, want 2", got.RunningTasks)
	}
	if got.ReviewMode != "ai" {
		t.Errorf("review mode = %q, want %q", got.ReviewMode, "ai")
	}
}

func TestSessionSummaryCarriesUsage(t *testing.T) {
	se := store.Session{
		ID: "s1", CostUSD: 0.0042, TokensIn: 1000, TokensOut: 234,
		CacheReadTokens: 300, ReasoningTokens: 40,
	}
	sum := sessionSummary(se)
	if sum.CostUSD != 0.0042 || sum.TokensIn != 1000 || sum.TokensOut != 234 ||
		sum.CacheReadTokens != 300 || sum.ReasoningTokens != 40 {
		t.Fatalf("usage not carried: %+v", sum)
	}
}

func TestImagePathsExposeArtifacts(t *testing.T) {
	m := core.Message{Role: core.RoleUser, Content: []core.Part{
		{Type: "text", Text: "hi"},
		{Type: "image", Image: &core.Image{Path: "/tmp/a.png"}},
		{Type: "image", Image: &core.Image{}},
	}}
	got := imagePaths(m)
	if len(got) != 1 || got[0] != "/tmp/a.png" {
		t.Fatalf("imagePaths = %v, want [/tmp/a.png]", got)
	}
}

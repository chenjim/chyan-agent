// transcribe_test.go: transcribe tool tests over httptest service stubs (submit/poll full path).
package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTranscribeTest builds a transcribe tool wired to a loopback server.
func newTranscribeTest(t *testing.T, srv *httptest.Server, timeout time.Duration) *TranscribeTool {
	t.Helper()
	tool := NewTranscribeTool(t.TempDir(), timeout, 20000)
	tool.BaseURL = srv.URL
	tool.interval = time.Millisecond
	return tool
}

// writeAudio drops a small audio file into a fresh work dir and returns both.
func writeAudio(t *testing.T) (workDir, file string) {
	t.Helper()
	workDir = t.TempDir()
	file = filepath.Join(workDir, "clip.mp3")
	if err := os.WriteFile(file, []byte("FAKE-AUDIO"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workDir, file
}

// transcribeStub serves the submit/poll protocol; returns done after n polls.
func transcribeStub(t *testing.T, pollsBeforeDone int, markdown string) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/transcribe":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse multipart: %v", err)
			}
			f, hdr, err := r.FormFile("file")
			if err != nil {
				t.Errorf("form file: %v", err)
			}
			f.Close()
			if hdr.Filename != "clip.mp3" {
				t.Errorf("filename = %s", hdr.Filename)
			}
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1", "status": "pending"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/task/t-1":
			n := polls.Add(1)
			if n <= int32(pollsBeforeDone) {
				json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1", "status": "processing"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"task_id": "t-1", "status": "done",
				"result": map[string]any{"title": "clip", "markdown": markdown},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestTranscribeSubmitPollDone(t *testing.T) {
	srv := transcribeStub(t, 2, "# 文稿\n你好")
	defer srv.Close()
	work, file := writeAudio(t)
	tool := NewTranscribeTool(work, 10*time.Second, 20000)
	tool.BaseURL = srv.URL
	tool.interval = time.Millisecond

	out := runVoice(t, tool, map[string]any{"path": file})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if out["task_id"] != "t-1" {
		t.Errorf("task_id = %v", out["task_id"])
	}
	if out["text"] != "# 文稿\n你好" {
		t.Errorf("text = %v", out["text"])
	}
}

func TestTranscribeLanguageForwarded(t *testing.T) {
	var gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			r.ParseMultipartForm(1 << 20)
			gotLang = r.FormValue("language")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"task_id": "t-2", "status": "pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"task_id": "t-2", "status": "done",
			"result": map[string]any{"markdown": "ok"}})
	}))
	defer srv.Close()
	work, file := writeAudio(t)
	tool := NewTranscribeTool(work, 10*time.Second, 20000)
	tool.BaseURL = srv.URL
	tool.interval = time.Millisecond

	out := runVoice(t, tool, map[string]any{"path": file, "language": "zh"})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if gotLang != "zh" {
		t.Errorf("language forwarded = %q, want zh", gotLang)
	}
}

func TestTranscribeTextTruncation(t *testing.T) {
	long := strings.Repeat("长", 5000) // multi-byte to also exercise rune-safe cut
	srv := transcribeStub(t, 0, long)
	defer srv.Close()
	work, file := writeAudio(t)
	tool := NewTranscribeTool(work, 10*time.Second, 100)
	tool.BaseURL = srv.URL
	tool.interval = time.Millisecond

	out := runVoice(t, tool, map[string]any{"path": file})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if out["output_truncated"] != true {
		t.Error("long text must be truncated")
	}
	text, _ := out["text"].(string)
	if len([]rune(text)) >= len([]rune(long)) {
		t.Errorf("text not truncated: %d runes", len([]rune(text)))
	}
}

func TestTranscribeTaskError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"task_id": "t-3", "status": "pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"task_id": "t-3", "status": "error", "error": "cookie expired"})
	}))
	defer srv.Close()
	work, file := writeAudio(t)
	tool := NewTranscribeTool(work, 10*time.Second, 20000)
	tool.BaseURL = srv.URL
	tool.interval = time.Millisecond

	out := runVoice(t, tool, map[string]any{"path": file})
	if out["success"] == true {
		t.Fatal("error state must fail")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "cookie expired") {
		t.Errorf("error = %q", msg)
	}
}

func TestTranscribeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"task_id": "t-4", "status": "pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"task_id": "t-4", "status": "processing"})
	}))
	defer srv.Close()
	work, file := writeAudio(t)
	tool := NewTranscribeTool(work, 30*time.Millisecond, 20000)
	tool.BaseURL = srv.URL
	tool.interval = time.Millisecond

	out := runVoice(t, tool, map[string]any{"path": file})
	if out["success"] == true {
		t.Fatal("timeout must fail")
	}
	if out["timed_out"] != true {
		t.Errorf("timed_out = %v, want true", out["timed_out"])
	}
	if out["task_id"] != "t-4" {
		t.Errorf("task_id = %v, want t-4 so the caller can resume waiting", out["task_id"])
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "processing") {
		t.Errorf("error = %q, want last status", msg)
	}
}

func TestTranscribeBadResponses(t *testing.T) {
	work, file := writeAudio(t)

	badJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "not json at all")
	}))
	defer badJSON.Close()
	tool := newTranscribeTest(t, badJSON, 5*time.Second)
	tool.workDir = work
	out := runVoice(t, tool, map[string]any{"path": file})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "bad JSON") {
		t.Errorf("bad submit JSON: %v", out)
	}

	noTaskID := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"status":"pending"}`)
	}))
	defer noTaskID.Close()
	tool2 := newTranscribeTest(t, noTaskID, 5*time.Second)
	tool2.workDir = work
	out = runVoice(t, tool2, map[string]any{"path": file})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "task_id") {
		t.Errorf("missing task_id: %v", out)
	}

	httpErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "worker down", http.StatusBadGateway)
	}))
	defer httpErr.Close()
	tool3 := newTranscribeTest(t, httpErr, 5*time.Second)
	tool3.workDir = work
	out = runVoice(t, tool3, map[string]any{"path": file})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "service returned 502") {
		t.Errorf("http error: %v", out)
	}

	badPoll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"task_id":"t-5"}`)
			return
		}
		fmt.Fprint(w, "{{{{")
	}))
	defer badPoll.Close()
	tool4 := newTranscribeTest(t, badPoll, 5*time.Second)
	tool4.workDir = work
	out = runVoice(t, tool4, map[string]any{"path": file})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "bad JSON from task endpoint") {
		t.Errorf("bad poll JSON: %v", out)
	}

	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"task_id":"t-6"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer notFound.Close()
	tool5 := newTranscribeTest(t, notFound, 5*time.Second)
	tool5.workDir = work
	out = runVoice(t, tool5, map[string]any{"path": file})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "not found or expired") {
		t.Errorf("404: %v", out)
	}
}

func TestTranscribeServiceUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	work, file := writeAudio(t)
	tool := NewTranscribeTool(work, 2*time.Second, 20000)
	tool.BaseURL = url
	out := runVoice(t, tool, map[string]any{"path": file})
	if out["success"] == true {
		t.Fatal("unreachable service must fail, not panic")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "unreachable") {
		t.Errorf("error = %q", msg)
	}
}

func TestTranscribePathConfinement(t *testing.T) {
	tool := NewTranscribeTool(t.TempDir(), 5*time.Second, 20000)
	out := runVoice(t, tool, map[string]any{"path": "../../etc/passwd"})
	if out["success"] == true {
		t.Fatal("escaping path must be rejected")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "escapes") {
		t.Errorf("error = %q", msg)
	}
	out = runVoice(t, tool, map[string]any{})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "path is required") {
		t.Errorf("missing path: %v", out)
	}
}

func TestTranscribeFileValidation(t *testing.T) {
	work := t.TempDir()
	tool := NewTranscribeTool(work, 5*time.Second, 20000)

	out := runVoice(t, tool, map[string]any{"path": "gone.mp3"})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "open audio") {
		t.Errorf("missing file: %v", out)
	}

	if err := os.WriteFile(filepath.Join(work, "empty.mp3"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out = runVoice(t, tool, map[string]any{"path": "empty.mp3"})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "empty") {
		t.Errorf("empty file: %v", out)
	}

	if err := os.Mkdir(filepath.Join(work, "dir.mp3"), 0o755); err != nil {
		t.Fatal(err)
	}
	out = runVoice(t, tool, map[string]any{"path": "dir.mp3"})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "regular file") {
		t.Errorf("directory input: %v", out)
	}
}

func TestTranscribeSchema(t *testing.T) {
	tool := NewTranscribeTool(t.TempDir(), time.Second, 20000)
	s := tool.Schema()
	req, _ := s["required"].([]any)
	if len(req) != 1 || req[0] != "path" {
		t.Errorf("required = %v", req)
	}
	props, _ := s["properties"].(map[string]any)
	for _, k := range []string{"path", "language"} {
		if _, ok := props[k]; !ok {
			t.Errorf("missing property %s", k)
		}
	}
	if tool.Name() != "transcribe" {
		t.Errorf("name = %s", tool.Name())
	}
}

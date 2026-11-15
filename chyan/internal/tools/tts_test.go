// tts_test.go: tts tool tests over httptest Kokoro stubs (no real network).
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/core"
)

// runVoice executes a tool and decodes the JSON payload.
func runVoice(t *testing.T, tool core.Tool, in map[string]any) map[string]any {
	t.Helper()
	res := tool.Execute(context.Background(), core.ToolCall{ID: "1", Name: tool.Name(), Input: in})
	out := map[string]any{}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result not JSON: %v: %s", err, res.Content)
	}
	return out
}

// newTTSTest builds a tts tool wired to a loopback httptest server.
func newTTSTest(t *testing.T, srv *httptest.Server, maxOut int) *TTSTool {
	t.Helper()
	tool := NewTTSTool(t.TempDir(), t.TempDir(), 5*time.Second, maxOut)
	tool.BaseURL = srv.URL
	return tool
}

func TestTTSSuccessCacheLayout(t *testing.T) {
	audio := []byte("FAKE-MP3-BYTES")
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
		}
		got = payload
		w.Write(audio)
	}))
	defer srv.Close()

	tool := newTTSTest(t, srv, 20000)
	out := runVoice(t, tool, map[string]any{"text": "你好世界"})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if got["model"] != "tts-1" || got["input"] != "你好世界" || got["voice"] != "af_heart" || got["response_format"] != "mp3" {
		t.Errorf("request payload = %v", got)
	}
	path, _ := out["path"].(string)
	sum := sha256.Sum256([]byte("你好世界"))
	prefix := filepath.Join(tool.cacheDir, hex.EncodeToString(sum[:]), "tts")
	if !strings.HasPrefix(path, prefix) {
		t.Errorf("path %q not under cache layout %q", path, prefix)
	}
	if !strings.HasSuffix(path, "-tts.mp3") {
		t.Errorf("name = %s", filepath.Base(path))
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(audio) {
		t.Errorf("file content = %q, err %v", data, err)
	}
	if out["bytes"] != float64(len(audio)) {
		t.Errorf("bytes = %v", out["bytes"])
	}
	if _, ok := out["note"]; !ok {
		t.Error("cache output must carry the cleanup note")
	}
}

func TestTTSCustomPathAndVoiceFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		json.NewDecoder(r.Body).Decode(&payload)
		if payload["voice"] != "zf_xiaoxiao" || payload["response_format"] != "wav" {
			t.Errorf("payload = %v", payload)
		}
		w.Write([]byte("RIFF"))
	}))
	defer srv.Close()

	work := t.TempDir()
	tool := NewTTSTool(work, t.TempDir(), 5*time.Second, 20000)
	tool.BaseURL = srv.URL
	target := filepath.Join(work, "out", "hello.wav")
	out := runVoice(t, tool, map[string]any{"text": "hi", "voice": "zf_xiaoxiao", "format": "wav", "path": target})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if out["path"] != target {
		t.Errorf("path = %v, want %s", out["path"], target)
	}
	if _, ok := out["note"]; ok {
		t.Error("explicit path output must not carry the cache note")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "RIFF" {
		t.Errorf("file = %q err %v", data, err)
	}
}

func TestTTSPathConfinement(t *testing.T) {
	tool := NewTTSTool(t.TempDir(), t.TempDir(), 5*time.Second, 20000)
	out := runVoice(t, tool, map[string]any{"text": "hi", "path": "../escape.mp3"})
	if out["success"] == true {
		t.Fatal("relative path must be rejected")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "absolute") {
		t.Errorf("error = %q", msg)
	}
	out = runVoice(t, tool, map[string]any{"text": "hi", "path": "/etc/out.mp3"})
	if out["success"] == true {
		t.Fatal("path outside workDir must be rejected")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "escapes") {
		t.Errorf("error = %q", msg)
	}
}

func TestTTSArgValidation(t *testing.T) {
	tool := NewTTSTool(t.TempDir(), t.TempDir(), 5*time.Second, 20000)
	out := runVoice(t, tool, map[string]any{})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "text is required") {
		t.Errorf("missing text: %v", out)
	}
	out = runVoice(t, tool, map[string]any{"text": "  "})
	if out["success"] == true {
		t.Error("blank text must be rejected")
	}
	out = runVoice(t, tool, map[string]any{"text": "hi", "format": "flac"})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "mp3 or wav") {
		t.Errorf("bad format: %v", out)
	}
}

func TestTTSTruncationAudioIntact(t *testing.T) {
	big := make([]byte, 10000)
	for i := range big {
		big[i] = 'A'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(big)
	}))
	defer srv.Close()

	// a long cache dir keeps the JSON payload above maxOut+envelopeAllowance
	tool := newTTSTest(t, srv, 40)
	seg := strings.Repeat("c", 200)
	tool.cacheDir = filepath.Join(t.TempDir(), seg, seg, seg)
	res := tool.Execute(context.Background(), core.ToolCall{ID: "1", Name: "tts", Input: map[string]any{"text": "hi"}})
	if !res.Truncated {
		t.Error("small maxOut must mark output truncated")
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("truncated content must stay valid JSON: %v", err)
	}
	// the audio file on disk must be complete despite output truncation:
	// re-run with a generous cap to get the path back and verify contents
	tool.maxOut = 20000
	got := runVoice(t, tool, map[string]any{"text": "hi"})
	path, _ := got["path"].(string)
	if data, err := os.ReadFile(path); err != nil || len(data) != len(big) {
		t.Errorf("audio truncated on disk: %d bytes, err %v", len(data), err)
	}
}

func TestTTSBadResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model warming up", http.StatusInternalServerError)
	}))
	defer srv.Close()
	tool := newTTSTest(t, srv, 20000)
	out := runVoice(t, tool, map[string]any{"text": "hi"})
	if out["success"] == true {
		t.Fatal("500 must fail")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "service returned 500") || !strings.Contains(msg, "model warming up") {
		t.Errorf("error = %q", msg)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer empty.Close()
	tool2 := newTTSTest(t, empty, 20000)
	out = runVoice(t, tool2, map[string]any{"text": "hi"})
	if msg, _ := out["error"].(string); out["success"] == true || !strings.Contains(msg, "empty audio") {
		t.Errorf("empty audio: %v", out)
	}
}

func TestTTSServiceUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	tool := newTTSTest(t, srv, 20000)
	tool.BaseURL = url
	out := runVoice(t, tool, map[string]any{"text": "hi"})
	if out["success"] == true {
		t.Fatal("unreachable service must fail, not panic")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "unreachable") {
		t.Errorf("error = %q", msg)
	}
}

func TestTTSTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test releases; client deadline fires first
	}))
	defer srv.Close()
	defer close(release)
	work := t.TempDir()
	tool := NewTTSTool(work, t.TempDir(), 20*time.Millisecond, 20000)
	tool.BaseURL = srv.URL
	out := runVoice(t, tool, map[string]any{"text": "hi"})
	if out["success"] == true {
		t.Fatal("timeout must fail")
	}
	if out["timed_out"] != true {
		t.Errorf("timed_out = %v, want true", out["timed_out"])
	}
}

func TestTTSSchema(t *testing.T) {
	tool := NewTTSTool(t.TempDir(), t.TempDir(), time.Second, 20000)
	s := tool.Schema()
	if s["type"] != "object" {
		t.Errorf("type = %v", s["type"])
	}
	req, _ := s["required"].([]any)
	if len(req) != 1 || req[0] != "text" {
		t.Errorf("required = %v", req)
	}
	props, _ := s["properties"].(map[string]any)
	for _, k := range []string{"text", "voice", "format", "path"} {
		if _, ok := props[k]; !ok {
			t.Errorf("missing property %s", k)
		}
	}
	if tool.Name() != "tts" {
		t.Errorf("name = %s", tool.Name())
	}
}

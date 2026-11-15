// Task image upload: data URLs decode into multimodal turns (FR-10).
package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"chyan.tech/chyan/internal/api"
	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// imageRecorder records whether any turn carried image bytes.
type imageRecorder struct {
	mu   sync.Mutex
	seen bool
}

func (b *imageRecorder) Chat(_ context.Context, req core.Request) (core.Response, error) {
	b.mu.Lock()
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == "image" && p.Image != nil && len(p.Image.Data) > 0 {
				b.seen = true
			}
		}
	}
	b.mu.Unlock()
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: "seen"}}}}, nil
}

func (b *imageRecorder) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: "seen"})
	}
	return r, err
}

func (b *imageRecorder) sawImage() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen
}

func newImageServer(t *testing.T) (*Server, *imageRecorder) {
	t.Helper()
	home := t.TempDir()
	st := store.NewMemory()
	rec := &imageRecorder{}
	a, err := app.New(app.Options{
		SkipGate: true, SkipExtensions: true,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: rec, Store: st,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	s, err := New(Options{Home: home, App: a, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	return s, rec
}

// PNGDataURL returns a data URL for a minimal PNG.
func PNGDataURL(t *testing.T) string {
	t.Helper()
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

func TestCreateTaskWithImage(t *testing.T) {
	s, rec := newImageServer(t)
	h := s.Handler()

	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{
		Prompt: "what is this?", Images: []string{PNGDataURL(t)},
	}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create = %d body=%s", w.Code, w.Body.String())
	}
	var task api.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	pollTask(t, h, task.ID, "done")
	if !rec.sawImage() {
		t.Fatal("task turn did not receive the image")
	}
}

func TestCreateTaskRejectsBadImages(t *testing.T) {
	s, _ := newImageServer(t)
	h := s.Handler()

	cases := []struct {
		name  string
		image string
		want  string
	}{
		{"not a data url", "http://example.com/a.png", "data URL"},
		{"not base64", "data:image/png,abc", "base64"},
		{"garbage bytes", "data:image/png;base64,bm90IGFuIGltYWdl", "unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{
				Prompt: "x", Images: []string{tc.image},
			}, "test-token")
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body = %s, want %q", w.Body.String(), tc.want)
			}
		})
	}
}

func TestCreateTaskTooManyImages(t *testing.T) {
	s, _ := newImageServer(t)
	h := s.Handler()
	images := make([]string, core.MaxImagesPerTurn+1)
	for i := range images {
		images[i] = PNGDataURL(t)
	}
	w, req := doJSON(t, http.MethodPost, "/api/v1/tasks", api.CreateTaskRequest{
		Prompt: "x", Images: images,
	}, "test-token")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "at most") {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

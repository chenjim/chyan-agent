// Multimodal turn tests: image parts reach the LLM, persist as pointers, and
// replay hydrates bytes from the artifact (FR-10).
package app

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

// captureBackend records every request (the title call included) and replies
// "ok".
type captureBackend struct {
	mu   sync.Mutex
	reqs []core.Request
}

func (b *captureBackend) Chat(_ context.Context, req core.Request) (core.Response, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	b.mu.Unlock()
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: "ok"}}}}, nil
}

func (b *captureBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: "ok"})
	}
	return r, err
}

func (b *captureBackend) sawImage() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, req := range b.reqs {
		for _, m := range req.Messages {
			for _, p := range m.Content {
				if p.Type == "image" && p.Image != nil && len(p.Image.Data) > 0 {
					return true
				}
			}
		}
	}
	return false
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRunOptsImageRoundTrip(t *testing.T) {
	backend := &captureBackend{}
	a, st := newInterruptApp(t, backend)
	png := tinyPNG(t)

	_, sid, err := a.RunOpts(context.Background(), "", "what is this?", RunOptions{
		Images: []ImageInput{{MIME: "image/png", Data: png}},
	})
	if err != nil {
		t.Fatalf("RunOpts: %v", err)
	}
	if !backend.sawImage() {
		t.Fatal("LLM request did not carry the image part")
	}

	// Persisted form keeps the pointer, not the bytes.
	rows, err := st.ListMessages(context.Background(), sid, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := unmarshalMessage(rows[0].Role, rows[0].Content, rows[0].CreatedAt)
	var pointer *core.Image
	for _, p := range first.Content {
		if p.Type == "image" {
			pointer = p.Image
		}
	}
	if pointer == nil || pointer.Path == "" || pointer.SHA256 == "" {
		t.Fatalf("image pointer = %+v", pointer)
	}
	if len(pointer.Data) != 0 {
		t.Fatal("raw image bytes must not be persisted")
	}
	if _, err := os.Stat(pointer.Path); err != nil {
		t.Fatalf("artifact missing: %v", err)
	}

	// Replay hydrates the bytes back from the artifact.
	history, err := a.loadHistory(context.Background(), sid)
	if err != nil {
		t.Fatalf("loadHistory: %v", err)
	}
	found := false
	for _, m := range history {
		for _, p := range m.Content {
			if p.Type == "image" && p.Image != nil && len(p.Image.Data) == len(png) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("replayed history did not hydrate the image bytes")
	}
}

func TestRunOptsImageLimits(t *testing.T) {
	backend := &captureBackend{}
	a, _ := newInterruptApp(t, backend)
	png := tinyPNG(t)

	tooMany := make([]ImageInput, core.MaxImagesPerTurn+1)
	for i := range tooMany {
		tooMany[i] = ImageInput{Data: png}
	}
	if _, _, err := a.RunOpts(context.Background(), "", "x", RunOptions{Images: tooMany}); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Fatalf("too many images: %v", err)
	}
	if _, _, err := a.RunOpts(context.Background(), "", "x", RunOptions{
		Images: []ImageInput{{Data: []byte("not an image")}},
	}); err == nil {
		t.Fatal("non-image bytes must be rejected")
	}
}

// TestRunOptsVisionOff: [llm] vision="off" rejects images before any LLM call.
func TestRunOptsVisionOff(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfg, []byte("[llm]\nvision = \"off\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	backend := &captureBackend{}
	a, err := New(Options{
		SkipExtensions: true, SkipGate: true, ConfigPath: cfg,
		Home: home, MemoryRoot: t.TempDir(), WorkDir: t.TempDir(),
		Backend: backend, Store: st,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	_, _, err = a.RunOpts(context.Background(), "", "x", RunOptions{
		Images: []ImageInput{{Data: tinyPNG(t)}},
	})
	if err == nil || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("vision off error = %v", err)
	}
	if backend.sawImage() {
		t.Fatal("vision off must not call the LLM with images")
	}
}

// toolImageBackend asks the agent to read a png, then checks the follow-up
// request carried the image bytes (tool -> model feedback path).
type toolImageBackend struct {
	mu       sync.Mutex
	calls    int
	sawImage bool
}

func (b *toolImageBackend) Chat(_ context.Context, req core.Request) (core.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == "image" && p.Image != nil && len(p.Image.Data) > 0 {
				b.sawImage = true
			}
		}
	}
	if b.calls == 1 {
		return core.Response{Message: core.Message{Role: core.RoleAssistant,
			ToolCalls: []core.ToolCall{{ID: "c1", Name: "read",
				Input: map[string]any{"path": "shot.png"}}}}}, nil
	}
	return core.Response{Message: core.Message{Role: core.RoleAssistant,
		Content: []core.Part{{Type: "text", Text: "got it"}}}}, nil
}

func (b *toolImageBackend) Stream(ctx context.Context, req core.Request, onDelta func(core.Delta)) (core.Response, error) {
	r, err := b.Chat(ctx, req)
	if err == nil && onDelta != nil {
		onDelta(core.Delta{Text: "got it"})
	}
	return r, err
}

func TestReadToolImageReachesModel(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "shot.png"), tinyPNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &toolImageBackend{}
	a, err := New(Options{
		SkipExtensions: true, SkipGate: true,
		Home: t.TempDir(), MemoryRoot: t.TempDir(), WorkDir: work,
		Backend: backend, Store: store.NewMemory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	if _, _, err := a.RunOpts(context.Background(), "", "read shot.png", RunOptions{}); err != nil {
		t.Fatalf("RunOpts: %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.sawImage {
		t.Fatalf("model never saw the read image (calls=%d)", backend.calls)
	}
}

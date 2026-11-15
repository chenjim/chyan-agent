package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

// writeArtifact creates a file under the app's artifact root.
func writeArtifact(t *testing.T, a *App, rel, body string) string {
	t.Helper()
	path := filepath.Join(a.artifactRoot(), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExpandRefArtifact(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	ctx := context.Background()
	body := "line one\n[artifact: artifact:other.txt]\nline three"
	path := writeArtifact(t, a, "ses1/bash/out.txt", body)

	res, err := a.ExpandRef(ctx, "artifact:ses1/bash/out.txt", 0, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if res.Text != body || res.Total != len(body) {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Next) != 1 || res.Next[0] != "artifact:artifact:other.txt" {
		t.Fatalf("next refs = %v", res.Next)
	}

	// Absolute pointers resolve too.
	if res, err = a.ExpandRef(ctx, path, 0, 0); err != nil || res.Text != body {
		t.Fatalf("absolute ref = %+v, %v", res, err)
	}
}

func TestExpandRefWindow(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	body := strings.Repeat("abcdef", 10)
	writeArtifact(t, a, "ses2/read/out.txt", body)

	res, err := a.ExpandRef(context.Background(), "artifact:ses2/read/out.txt", 10, 7)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if res.Text != body[10:17] || res.Total != len(body) {
		t.Fatalf("window = %q, total %d", res.Text, res.Total)
	}
	// Offset past the end yields an empty window, not an error.
	res, err = a.ExpandRef(context.Background(), "artifact:ses2/read/out.txt", len(body)+5, 5)
	if err != nil || res.Text != "" || res.Total != len(body) {
		t.Fatalf("past-end window = %+v, %v", res, err)
	}
}

func TestExpandRefRejectsEscapesAndMissing(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	ctx := context.Background()

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExpandRef(ctx, outside, 0, 0); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("outside path error = %v", err)
	}
	if _, err := a.ExpandRef(ctx, "../secret.txt", 0, 0); err == nil {
		t.Fatal("relative traversal must be rejected as unsupported")
	}
	if _, err := a.ExpandRef(ctx, "artifact:../secret.txt", 0, 0); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("traversal error = %v", err)
	}
	if _, err := a.ExpandRef(ctx, "artifact:missing.txt", 0, 0); err == nil {
		t.Fatal("missing artifact must error")
	}
}

func TestExpandRefHandoff(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	ctx := context.Background()

	if _, err := a.ExpandRef(ctx, "handoff:ses9", 0, 0); err == nil {
		t.Fatal("missing handoff must error")
	}
	note := core.HandoffNote{Summary: "did the thing"}
	raw, err := json.Marshal(note)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetKV(ctx, "handoff:ses9", raw); err != nil {
		t.Fatal(err)
	}
	res, err := a.ExpandRef(ctx, "handoff:ses9", 0, 0)
	if err != nil {
		t.Fatalf("expand handoff: %v", err)
	}
	if res.Text != string(raw) || res.Total != len(raw) {
		t.Fatalf("handoff res = %+v", res)
	}
	if _, err := a.ExpandRef(ctx, "handoff:", 0, 0); err == nil {
		t.Fatal("empty handoff id must error")
	}
}

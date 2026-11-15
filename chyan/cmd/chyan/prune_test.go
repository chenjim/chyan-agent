// prune_test.go — retention helpers (artifacts, sizes, formatting).
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chyan.tech/chyan/internal/store"
	"chyan.tech/chyan/internal/trace"
)

func TestPruneTraceSpans(t *testing.T) {
	ctx := context.Background()
	s, err := store.NewSQLite(filepath.Join(t.TempDir(), "chyan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	old := trace.Span{ID: "old", TraceID: "tr", Kind: trace.KindTurn,
		Start: now.Add(-48 * time.Hour), End: now.Add(-47 * time.Hour), Status: trace.StatusOK}
	fresh := trace.Span{ID: "new", TraceID: "tr", Kind: trace.KindTurn,
		Start: now.Add(-time.Hour), End: now.Add(-30 * time.Minute), Status: trace.StatusOK}
	for _, sp := range []trace.Span{old, fresh} {
		if err := s.AppendSpan(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := now.Add(-24 * time.Hour)
	n, err := pruneTraceSpans(ctx, s, cutoff, true)
	if err != nil || n != 1 {
		t.Fatalf("dry-run = %d, %v", n, err)
	}
	n, err = pruneTraceSpans(ctx, s, cutoff, false)
	if err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}
}

func TestPruneArtifacts(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "ses1", "bash")
	newDir := filepath.Join(root, "ses2", "read")
	for _, dir := range []string{oldDir, newDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldFile := filepath.Join(oldDir, "old.txt")
	newFile := filepath.Join(newDir, "new.txt")
	for _, f := range []string{oldFile, newFile} {
		if err := os.WriteFile(f, []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)

	// Dry run: counted but untouched.
	files, bytes, err := pruneArtifacts(root, cutoff, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if files != 1 || bytes != 5 {
		t.Fatalf("dry-run files=%d bytes=%d, want 1/5", files, bytes)
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("dry-run removed the file: %v", err)
	}

	// Real run: old file and its now-empty tool dir go away, new data stays.
	files, bytes, err = pruneArtifacts(root, cutoff, false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if files != 1 || bytes != 5 {
		t.Fatalf("prune files=%d bytes=%d, want 1/5", files, bytes)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("old artifact still present: %v", err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("empty tool dir should be removed, err=%v", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("new artifact removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ses1")); !os.IsNotExist(err) {
		t.Errorf("empty session dir should be removed, err=%v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("artifacts root must survive: %v", err)
	}

	// Missing root is a no-op, not an error.
	if files, _, err := pruneArtifacts(filepath.Join(root, "nope"), cutoff, false); err != nil || files != 0 {
		t.Fatalf("missing root = %d, %v", files, err)
	}
}

func TestSizeHelpers(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "one"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "two"), []byte("56"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := dirSize(dir); got != 6 {
		t.Errorf("dirSize = %d, want 6", got)
	}
	if got := dirSize(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("missing dirSize = %d, want 0", got)
	}
	if got := fileSize(filepath.Join(dir, "two")); got != 2 {
		t.Errorf("fileSize = %d, want 2", got)
	}
	if got := fileSize(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("missing fileSize = %d, want 0", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		1023:    "1023 B",
		1024:    "1.0 KB",
		1536:    "1.5 KB",
		5 << 20: "5.0 MB",
		3 << 30: "3.0 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

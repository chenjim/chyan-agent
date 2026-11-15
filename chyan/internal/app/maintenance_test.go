package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chyan.tech/chyan/internal/core"
)

// TestPurgeCacheRemovesStaleFiles: files older than the TTL go, fresh files
// stay, empty dirs are pruned (architecture 4.6).
func TestPurgeCacheRemovesStaleFiles(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "shard", "old.bin")
	fresh := filepath.Join(root, "shard", "fresh.bin")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	if err := cleanCache(root, time.Now(), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale file must be purged")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file must survive")
	}
	// root itself survives even though its subdir may now be pruned.
	if _, err := os.Stat(root); err != nil {
		t.Error("cache root must survive")
	}
}

// TestPurgeCacheMissingRootIsNoop.
func TestPurgeCacheMissingRootIsNoop(t *testing.T) {
	if err := cleanCache(filepath.Join(t.TempDir(), "none"), time.Now(), time.Hour); err != nil {
		t.Errorf("missing root must be a noop: %v", err)
	}
}

// TestAppPurgeAndDream: the App-level maintenance hooks work against a real
// memory root and cache dir.
func TestAppPurgeAndDream(t *testing.T) {
	a := newTestApp(t, &stubBackend{responses: []core.Response{textResp("x")}})
	// Cache purge on an empty/missing cache dir is fine.
	if err := a.PurgeCache(); err != nil {
		t.Fatalf("PurgeCache: %v", err)
	}
	// Dream over an empty memory root reports zero without error.
	rep, err := a.RunDream(context.Background())
	if err != nil {
		t.Fatalf("RunDream: %v", err)
	}
	if rep.Examined != 0 {
		t.Errorf("examined = %d, want 0", rep.Examined)
	}
	// DreamOnce logs and swallows errors (empty root is not an error).
	a.DreamOnce(context.Background())
}

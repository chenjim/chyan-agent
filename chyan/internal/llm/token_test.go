package llm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHeuristicCounter(t *testing.T) {
	c := HeuristicCounter{}
	if got := c.Count(""); got != 0 {
		t.Errorf("empty text = %d, want 0", got)
	}
	// ASCII: ~4 bytes per token.
	if got := c.Count("abcd"); got != 1 {
		t.Errorf("4 ascii bytes = %d, want 1", got)
	}
	if got := c.Count("abcde"); got != 2 {
		t.Errorf("5 ascii bytes = %d, want 2 (ceil)", got)
	}
	// CJK: one token per rune.
	if got := c.Count("上下文预算"); got != 5 {
		t.Errorf("5 cjk runes = %d, want 5", got)
	}
	// Mixed text sums both regimes.
	if got := c.Count("hi你好"); got != 1+2 {
		t.Errorf("mixed = %d, want %d", got, 1+2)
	}
	// Heuristic must over-count short ascii rather than under-count: a budget
	// that compresses early is safe; one that overflows is not.
	if got := c.Count("ab"); got < 1 {
		t.Errorf("2 ascii bytes = %d, want >= 1", got)
	}
}

func TestCountMessages(t *testing.T) {
	got := CountMessages(HeuristicCounter{}, []string{"abcd", "abcd"})
	want := 2 * (1 + messageOverhead)
	if got != want {
		t.Errorf("CountMessages = %d, want %d", got, want)
	}
	if got := CountMessages(HeuristicCounter{}, nil); got != 0 {
		t.Errorf("nil messages = %d, want 0", got)
	}
}

func TestParseBpeBytes(t *testing.T) {
	// base64("a") == "YQ==", base64("bc") == "YmM=".
	data := []byte("YQ== 0\nYmM= 1\n")
	ranks, err := parseBpeBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if ranks["a"] != 0 || ranks["bc"] != 1 {
		t.Errorf("parsed ranks = %v", ranks)
	}
	// Malformed rank line must error.
	if _, err := parseBpeBytes([]byte("YQ== notanumber\n")); err == nil {
		t.Error("malformed rank must error")
	}
	// Malformed base64 must error.
	if _, err := parseBpeBytes([]byte("!!! 0\n")); err == nil {
		t.Error("malformed base64 must error")
	}
	// Wrong field count must error.
	if _, err := parseBpeBytes([]byte("onlyonefield\n")); err == nil {
		t.Error("wrong field count must error")
	}
}

func TestCachedBpeLoaderUsesCache(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed the cache file named after the URL basename so the loader
	// finds it without any network access.
	if err := os.WriteFile(filepath.Join(dir, "o200k_base.tiktoken"), []byte("YQ== 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &cachedBpeLoader{dir: dir}
	ranks, err := l.LoadTiktokenBpe("https://example.invalid/encodings/o200k_base.tiktoken")
	if err != nil {
		t.Fatal(err)
	}
	if ranks["a"] != 0 {
		t.Errorf("cache-loaded ranks = %v", ranks)
	}
}

func TestCachedBpeLoaderUnreachable(t *testing.T) {
	l := &cachedBpeLoader{dir: t.TempDir()}
	// Nothing listens on port 1: must return an error, not panic or hang.
	if _, err := l.LoadTiktokenBpe("http://127.0.0.1:1/o200k_base.tiktoken"); err == nil {
		t.Error("unreachable BPE source must error")
	}
}

func TestNewCounterFallsBackOffline(t *testing.T) {
	// Point the cache at an empty dir and force a miss: without network the
	// precise counter cannot be built, so NewCounter must return a working
	// heuristic counter rather than fail. Either counter type is acceptable
	// (precise when a cached BPE table exists on the machine), but Count must
	// always return a sane positive number.
	ResetCounterForTest()
	t.Cleanup(ResetCounterForTest)
	c := NewCounter(filepath.Join(t.TempDir(), "tiktoken"))
	if c == nil {
		t.Fatal("NewCounter returned nil")
	}
	if got := c.Count("hello world"); got <= 0 {
		t.Errorf("Count = %d, want > 0", got)
	}
	// Process-wide sharing: second call returns the same instance.
	if NewCounter("") != c {
		t.Error("NewCounter must return the process-wide shared counter")
	}
}

func TestFetchBytesRejectsUnreachable(t *testing.T) {
	if _, err := fetchBytes("http://127.0.0.1:1/x"); err == nil {
		t.Error("unreachable fetch must error")
	}
	if err := fetchJSON("http://127.0.0.1:1/x", &struct{}{}); err == nil {
		t.Error("unreachable fetchJSON must error")
	}
}

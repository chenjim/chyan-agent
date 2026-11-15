package llm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMatchBuiltin(t *testing.T) {
	table := map[string]int{
		"gpt-4o-mini":       128000,
		"gpt-4o":            128000,
		"claude-3-5-sonnet": 200000,
		"deepseek-v4-flash": 1000000,
		"o1-preview":        200000,
		"claude-sonnet-4-5": 1000000,
		"mimo-v2.5-pro":     1048576,
	}
	cases := []struct {
		model string
		want  int
		found bool
	}{
		{"gpt-4o-mini", 128000, true},
		{"openai/gpt-4o", 128000, true},               // provider prefix stripped
		{"GPT-4O-MINI", 128000, true},                 // case-insensitive
		{"claude-sonnet-4-5-20250929", 1000000, true}, // longest prefix wins
		{"mimo-v2.5-pro-0731", 1048576, true},         // version suffix tolerated
		{"o1-preview", 200000, true},                  // segment boundary exact
		{"foo1", 0, false},                            // no false boundary hit
		{"totally-unknown-model", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := matchBuiltin(tc.model, table)
		if ok != tc.found || (ok && got != tc.want) {
			t.Errorf("matchBuiltin(%q) = (%d, %v), want (%d, %v)", tc.model, got, ok, tc.want, tc.found)
		}
	}
}

func TestMatchBuiltinLongestPrefix(t *testing.T) {
	// "claude-sonnet-4-5" must beat "claude-sonnet-4" for dated model names.
	table := map[string]int{"claude-sonnet-4": 200000, "claude-sonnet-4-5": 1000000}
	got, ok := matchBuiltin("claude-sonnet-4-5-20250929", table)
	if !ok || got != 1000000 {
		t.Fatalf("longest prefix: got (%d,%v), want 1000000,true", got, ok)
	}
}

func TestExplicitLimit(t *testing.T) {
	cases := []struct {
		raw   any
		want  int
		found bool
	}{
		{9999, 9999, true},
		{"12345", 12345, true},
		{" 42 ", 42, true},
		{"auto", 0, false},
		{"", 0, false},
		{0, 0, false},
		{-5, 0, false},
		{true, 0, false},
		{nil, 0, false},
	}
	for _, tc := range cases {
		got, ok := explicitLimit(tc.raw)
		if ok != tc.found || (ok && got != tc.want) {
			t.Errorf("explicitLimit(%v) = (%d,%v), want (%d,%v)", tc.raw, got, ok, tc.want, tc.found)
		}
	}
}

func TestResolveLayers(t *testing.T) {
	// Builtin table resolution (layer 3).
	cfg := &WindowConfig{}
	if n, src := cfg.ResolveContextLimit("gpt-4o-mini"); n != 128000 || src != SourceBuiltin {
		t.Errorf("builtin layer: got (%d,%s), want (128000,builtin)", n, src)
	}
	// Fallback layer (layer 4).
	if n, src := cfg.ResolveContextLimit("unknown-model-xyz"); n != DefaultFallbackLimit || src != SourceFallback {
		t.Errorf("fallback layer: got (%d,%s), want (%d,fallback)", n, src, DefaultFallbackLimit)
	}
	// Config layer wins over builtin (layer 1).
	pinned := &WindowConfig{ContextLimit: 9999}
	if n, src := pinned.ResolveContextLimit("gpt-4o-mini"); n != 9999 || src != SourceConfig {
		t.Errorf("config layer: got (%d,%s), want (9999,config)", n, src)
	}
	// Custom DefaultLimit in fallback.
	dflt := &WindowConfig{DefaultLimit: 65536}
	if n, src := dflt.ResolveContextLimit("zzz-unknown"); n != 65536 || src != SourceFallback {
		t.Errorf("default limit: got (%d,%s), want (65536,fallback)", n, src)
	}
}

func TestResolveOnlineLayer(t *testing.T) {
	// Offline by default: a pointed-at unreachable catalog must degrade to
	// builtin/fallback without panicking or hanging.
	cfg := &WindowConfig{
		OnlineCatalog: true,
		CatalogURL:    "http://127.0.0.1:1/api.json", // nothing listens here
		CatalogCache:  filepath.Join(t.TempDir(), "catalog.json"),
		CatalogTTL:    time.Hour,
	}
	if n, src := cfg.ResolveContextLimit("gpt-4o-mini"); n != 128000 || src != SourceBuiltin {
		t.Errorf("online degrade: got (%d,%s), want (128000,builtin)", n, src)
	}
}

func TestCatalogCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	c := &catalog{
		Models:   map[string]int{"gpt-4o": 128000},
		Families: map[string]int{"o1": 200000},
		Fetched:  float64(time.Now().Unix()),
	}
	writeCatalogCache(path, c)
	got := readCatalogCache(path)
	if got == nil || got.Models["gpt-4o"] != 128000 {
		t.Fatalf("catalog cache round trip failed: %+v", got)
	}
	// A stale cache (expired TTL) is still usable as a fetch-failure fallback.
	writeCatalogCache(path, &catalog{Models: map[string]int{"x": 1}, Fetched: 1})
	if readCatalogCache(path) == nil {
		t.Fatal("stale cache should still load for fallback")
	}
	// Missing path yields nil, never an error path.
	if readCatalogCache(filepath.Join(dir, "nope.json")) != nil {
		t.Fatal("missing cache should be nil")
	}
}

func TestMatchCatalog(t *testing.T) {
	c := &catalog{
		Models:   map[string]int{"gpt-4o": 128000, "o1-preview": 200000},
		Families: map[string]int{"kimi": 262144},
	}
	if n, ok := matchCatalog("gpt-4o", c); !ok || n != 128000 {
		t.Errorf("exact: got (%d,%v)", n, ok)
	}
	if n, ok := matchCatalog("openai/gpt-4o", c); !ok || n != 128000 {
		t.Errorf("short name: got (%d,%v)", n, ok)
	}
	if n, ok := matchCatalog("kimi-k2.7-code", c); !ok || n != 262144 {
		t.Errorf("family match: got (%d,%v)", n, ok)
	}
	if _, ok := matchCatalog("unknown-model", c); ok {
		t.Error("unknown model must not match")
	}
	if _, ok := matchCatalog("", c); ok {
		t.Error("empty model must not match")
	}
	if _, ok := matchCatalog("gpt-4o", nil); ok {
		t.Error("nil catalog must not match")
	}
}

func TestParseWindowJSONFormats(t *testing.T) {
	// Wrapped format.
	m, err := parseWindowJSON([]byte(`{"windows": {"GPT-4O": 128000, "bad": -1, "zero": 0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m["gpt-4o"] != 128000 {
		t.Errorf("wrapped parse: %v", m)
	}
	if _, ok := m["bad"]; ok {
		t.Error("non-positive window must be dropped")
	}
	// Flat legacy format.
	m, err = parseWindowJSON([]byte(`{"a-model": 999}`))
	if err != nil || m["a-model"] != 999 {
		t.Errorf("flat parse: %v %v", m, err)
	}
	// Broken JSON.
	if _, err := parseWindowJSON([]byte(`{not json`)); err == nil {
		t.Error("broken JSON must error")
	}
}

func TestCustomWindowTable(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "windows.json")
	if err := os.WriteFile(custom, []byte(`{"windows": {"my-model": 777}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &WindowConfig{WindowTablePath: custom}
	if n, src := cfg.ResolveContextLimit("my-model"); n != 777 || src != SourceBuiltin {
		t.Errorf("custom table: got (%d,%s), want (777,builtin)", n, src)
	}
	// A broken custom table must fall back to the builtin table, not fail.
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = &WindowConfig{WindowTablePath: broken}
	if n, src := cfg.ResolveContextLimit("gpt-4o-mini"); n != 128000 || src != SourceBuiltin {
		t.Errorf("broken custom table fallback: got (%d,%s), want (128000,builtin)", n, src)
	}
}

func TestBuiltinTableEmbedded(t *testing.T) {
	// The embedded table must parse and carry known entries from the legacy map.
	w := builtinWindows()
	if len(w) == 0 {
		t.Fatal("embedded builtin window table is empty")
	}
	if w["gpt-4o-mini"] != 128000 || w["deepseek-v4-flash"] != 1000000 {
		t.Errorf("embedded table missing expected entries: gpt-4o-mini=%d deepseek-v4-flash=%d",
			w["gpt-4o-mini"], w["deepseek-v4-flash"])
	}
}

func TestSegmentMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"o1", "o1-preview", true},
		{"o1", "foo1", false},
		{"kimi", "kimi-k2", true},
		{"", "anything", true}, // empty pattern trivially matches at 0
		{"abc", "xabc", false}, // mid-string preceded by alnum
	}
	for _, tc := range cases {
		if got := segmentMatch(tc.pat, tc.s); got != tc.want {
			t.Errorf("segmentMatch(%q,%q) = %v, want %v", tc.pat, tc.s, got, tc.want)
		}
	}
}

package llm

import (
	_ "embed"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Builtin window table shipped inside the binary (data ported from the earlier
// Python implementation's model_windows.json).
//
//go:embed model_windows.json
var builtinWindowJSON []byte

const (
	// DefaultFallbackLimit is used when no layer resolves a window.
	DefaultFallbackLimit = 131072
	// DefaultCatalogURL is the online model catalog (models.dev).
	DefaultCatalogURL = "https://models.dev/api.json"
	// DefaultCatalogTTLHours controls how long a fetched catalog stays fresh.
	DefaultCatalogTTLHours = 168
)

// WindowConfig configures layered window resolution.
type WindowConfig struct {
	// ContextLimit, when > 0 or a numeric string, pins the window (layer 1).
	// "" or "auto" defers to lower layers.
	ContextLimit any
	// DefaultLimit is the fallback window (layer 4). Zero means DefaultFallbackLimit.
	DefaultLimit int
	// OnlineCatalog enables the online catalog layer (layer 2).
	OnlineCatalog bool
	CatalogURL    string
	CatalogCache  string // local JSON cache path; empty disables caching
	CatalogTTL    time.Duration
	// WindowTablePath points at a custom {"windows": {...}} JSON file
	// overriding the builtin table (layer 3).
	WindowTablePath string
}

// WindowSource identifies which resolution layer produced a window value.
type WindowSource string

const (
	SourceConfig   WindowSource = "config"
	SourceOnline   WindowSource = "online"
	SourceBuiltin  WindowSource = "builtin"
	SourceFallback WindowSource = "fallback"
)

// ResolveContextLimit resolves the context window for model using the layer
// order config > online catalog > builtin table > fallback, mirroring the
// legacy chyan-agent policy. Returns the window in tokens and its source.
func (c *WindowConfig) ResolveContextLimit(model string) (int, WindowSource) {
	if n, ok := explicitLimit(c.ContextLimit); ok {
		return n, SourceConfig
	}
	if c.OnlineCatalog {
		if cat := loadCatalog(c); cat != nil {
			if n, ok := matchCatalog(model, cat); ok {
				return n, SourceOnline
			}
		}
	}
	table := loadTable(c)
	if n, ok := matchBuiltin(model, table); ok {
		return n, SourceBuiltin
	}
	if c.DefaultLimit > 0 {
		return c.DefaultLimit, SourceFallback
	}
	return DefaultFallbackLimit, SourceFallback
}

// explicitLimit normalizes WindowConfig.ContextLimit to a positive window.
// int64 covers TOML's integer decoding into `any`.
func explicitLimit(raw any) (int, bool) {
	switch v := raw.(type) {
	case int:
		if v > 0 {
			return v, true
		}
	case int64:
		if v > 0 {
			return int(v), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// loadTable returns the effective builtin window map, honouring a custom
// table path. A broken or missing custom table falls back to the embedded one.
func loadTable(c *WindowConfig) map[string]int {
	if c != nil && c.WindowTablePath != "" {
		if t, err := readWindowFile(c.WindowTablePath); err == nil {
			return t
		}
		log.Printf("llm: custom window table unusable (%s), falling back to builtin", c.WindowTablePath)
	}
	return builtinWindows()
}

var (
	builtinOnce sync.Once
	builtin     map[string]int
)

func builtinWindows() map[string]int {
	builtinOnce.Do(func() {
		t, err := parseWindowJSON(builtinWindowJSON)
		if err != nil {
			log.Printf("llm: builtin window table unreadable: %v", err)
			t = map[string]int{}
		}
		builtin = t
	})
	return builtin
}

// readWindowFile reads and parses a window table file. Accepts both
// {"windows": {...}} and a flat {"model": n} map (legacy formats).
func readWindowFile(path string) (map[string]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseWindowJSON(data)
}

// parseWindowJSON parses a window table body into a lowercase name->tokens map.
func parseWindowJSON(data []byte) (map[string]int, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	windows, _ := raw["windows"].(map[string]any)
	if windows == nil {
		windows = raw
	}
	out := make(map[string]int, len(windows))
	for k, v := range windows {
		if n, ok := v.(float64); ok && n > 0 {
			out[strings.ToLower(strings.TrimSpace(k))] = int(n)
		}
	}
	return out, nil
}

// matchBuiltin resolves model against a window table with legacy matching:
// full name exact, then provider-prefix-stripped exact, then longest prefix
// (tolerates date/version suffixes).
func matchBuiltin(model string, table map[string]int) (int, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" || len(table) == 0 {
		return 0, false
	}
	if n, ok := table[name]; ok {
		return n, true
	}
	short := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		short = name[i+1:]
	}
	if n, ok := table[short]; ok {
		return n, true
	}
	bestKey := ""
	for key := range table {
		if strings.HasPrefix(short, key) && len(key) > len(bestKey) {
			bestKey = key
		}
	}
	if bestKey != "" {
		return table[bestKey], true
	}
	return 0, false
}

// catalog is a distilled online model window directory.
type catalog struct {
	Models   map[string]int `json:"models"`
	Families map[string]int `json:"families"`
	Fetched  float64        `json:"fetched_at"`
}

// loadCatalog returns the online window catalog, using the on-disk cache
// when fresh. Fetch failures degrade to a stale cache, then to nil (no
// catalog), never to an error: resolution must keep working offline.
func loadCatalog(c *WindowConfig) *catalog {
	url := c.CatalogURL
	if url == "" {
		url = DefaultCatalogURL
	}
	ttl := c.CatalogTTL
	if ttl <= 0 {
		ttl = DefaultCatalogTTLHours * time.Hour
	}
	cached := readCatalogCache(c.CatalogCache)
	if cached != nil && time.Since(time.Unix(int64(cached.Fetched), 0)) < ttl {
		return cached
	}
	raw, err := fetchModelsDev(url)
	if err != nil {
		log.Printf("llm: model catalog fetch failed (%v), degrading to cache", err)
		return cached
	}
	writeCatalogCache(c.CatalogCache, raw)
	return raw
}

// modelsDevResp is the raw models.dev /api.json shape:
// {"<provider>": {"models": {"<provider>/<model>": {"limit": {"context": n}, ...}}}}.
type modelsDevResp map[string]struct {
	Models map[string]struct {
		Limit  map[string]int `json:"limit"`
		Family string         `json:"family"`
	} `json:"models"`
}

// fetchModelsDev downloads and distills the catalog. Same-model entries
// across providers take the smaller window (compress early, never overflow).
func fetchModelsDev(url string) (*catalog, error) {
	var raw modelsDevResp
	if err := fetchJSON(url, &raw); err != nil {
		return nil, err
	}
	out := &catalog{Models: map[string]int{}, Families: map[string]int{}, Fetched: float64(time.Now().Unix())}
	keep := func(m map[string]int, key string, n int) {
		if key == "" || n <= 0 {
			return
		}
		if old, ok := m[key]; !ok || n < old {
			m[key] = n
		}
	}
	for _, prov := range raw {
		for id, info := range prov.Models {
			short := id
			if i := strings.LastIndex(id, "/"); i >= 0 {
				short = id[i+1:]
			}
			keep(out.Models, strings.ToLower(short), info.Limit["context"])
			keep(out.Families, strings.ToLower(info.Family), info.Limit["context"])
		}
	}
	return out, nil
}

func readCatalogCache(path string) *catalog {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return &c
}

func writeCatalogCache(path string, c *catalog) {
	if path == "" || c == nil {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	// Best effort: a failed cache write only costs a re-fetch next time.
	_ = os.WriteFile(path, data, 0o600)
}

// matchCatalog resolves model against a distilled catalog: exact full name,
// then short name, then longest shared substring, then family at a segment
// boundary (legacy matching order).
func matchCatalog(model string, c *catalog) (int, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" || c == nil {
		return 0, false
	}
	short := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		short = name[i+1:]
	}
	if n, ok := c.Models[name]; ok {
		return n, true
	}
	if n, ok := c.Models[short]; ok {
		return n, true
	}
	bestKey := ""
	for k := range c.Models {
		if k != "" && (strings.Contains(name, k) || strings.Contains(k, name)) && len(k) > len(bestKey) {
			bestKey = k
		}
	}
	if bestKey != "" {
		return c.Models[bestKey], true
	}
	for fam, n := range c.Families {
		if fam != "" && segmentMatch(fam, name) {
			return n, true
		}
	}
	return 0, false
}

// segmentMatch reports whether pattern starts at a non-alphanumeric boundary
// inside s (e.g. "o1" matches "o1-preview" but not "foo1").
func segmentMatch(pattern, s string) bool {
	for i := 0; ; {
		idx := strings.Index(s[i:], pattern)
		if idx < 0 {
			return false
		}
		pos := i + idx
		if pos == 0 || !isAlnum(s[pos-1]) {
			return true
		}
		i = pos + 1
	}
}

func isAlnum(b byte) bool {
	return ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// Package llm: token counting. Precise counting uses tiktoken (o200k_base,
// aligned with the legacy chyan-agent); a heuristic counter is the offline
// fallback so context budgeting never blocks on network availability.
package llm

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"github.com/pkoukk/tiktoken-go"
)

// Counter estimates token usage for context budgeting.
type Counter interface {
	Count(text string) int
}

// HeuristicCounter estimates tokens without any external data: one token per
// non-ASCII rune (CJK is dense in o200k) and roughly four ASCII bytes per
// token. It deliberately over-counts a little: a budget that triggers
// compression early is safe, one that under-counts overflows the window.
type HeuristicCounter struct{}

// Count implements Counter.
func (HeuristicCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	ascii := 0
	other := 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// messageOverhead approximates the per-message framing tokens chat templates
// add around content (role markers, separators).
const messageOverhead = 4

// CountMessages sums message content plus per-message overhead.
func CountMessages(c Counter, texts []string) int {
	total := 0
	for _, t := range texts {
		total += c.Count(t) + messageOverhead
	}
	return total
}

// preciseCounter wraps a tiktoken encoding. Construction downloads the BPE
// table once (cached under dataDir); see bpeLoader.
type preciseCounter struct {
	enc *tiktoken.Tiktoken
}

func (p preciseCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	return len(p.enc.Encode(text, nil, nil))
}

// cachedBpeLoader reads the BPE table from a local cache under dataDir and
// downloads it once on miss, so counting works offline after first use.
type cachedBpeLoader struct {
	dir string
	mu  sync.Mutex
}

func (l *cachedBpeLoader) LoadTiktokenBpe(url string) (map[string]int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	path := filepath.Join(l.dir, filepath.Base(url))
	if _, err := os.Stat(path); err == nil {
		return parseBpeFile(path)
	}
	data, err := fetchBytes(url)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(l.dir, 0o700); err == nil {
		// Best effort: a write failure only costs one re-download later.
		_ = os.WriteFile(path, data, 0o600)
	}
	return parseBpeBytes(data)
}

var (
	counterOnce sync.Once
	shared      Counter
)

// NewCounter returns a precise tiktoken-based counter when the BPE table is
// available (downloaded once, then cached under dataDir/tiktoken), falling
// back to the heuristic counter when offline or on any failure. The result is
// process-wide (the encoding is immutable and heavy to build).
func NewCounter(dataDir string) Counter {
	counterOnce.Do(func() {
		shared = buildCounter(dataDir)
	})
	return shared
}

// ResetCounterForTest clears the process-wide counter (tests only).
func ResetCounterForTest() {
	counterOnce = sync.Once{}
	shared = nil
}

// ForceHeuristicForTest primes the process-wide counter with the heuristic
// (no tiktoken download) so tests stay hermetic and offline. Call it from
// test setup before any agent run. Tests only.
func ForceHeuristicForTest() {
	counterOnce.Do(func() { shared = HeuristicCounter{} })
}

func buildCounter(dataDir string) Counter {
	// Route tiktoken's loader through our cache-aware one before the first
	// encoding is built; the loader is package-global in tiktoken-go.
	tiktoken.SetBpeLoader(&cachedBpeLoader{dir: filepath.Join(dataDir, "tiktoken")})
	enc, err := tiktoken.GetEncoding("o200k_base")
	if err != nil {
		log.Printf("llm: tiktoken unavailable (%v), using heuristic token counter", err)
		return HeuristicCounter{}
	}
	return preciseCounter{enc: enc}
}

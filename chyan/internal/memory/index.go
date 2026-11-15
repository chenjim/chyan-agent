// Memory full-text index: a derived, rebuildable search cache over the
// markdown truth source (architecture 4.3). The cache lives in its own SQLite
// file (FTS5 virtual table when the driver supports it, otherwise a pure-Go
// inverted index persisted as JSON) and never touches the internal/store
// schema. Markdown files remain the truth source; the index can be deleted
// and rebuilt at any time via Rebuild.
package memory

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"chyan.tech/chyan/internal/store"
)

// Ranking weights of the architecture 4.3 formula
// score = relevance × recency × pinned × domain.
const (
	// recencyWindowDays is the age at which the recency factor halves.
	recencyWindowDays  = 30.0
	pinnedWeight       = 2.0
	projectWeight      = 1.5
	projectLocalWeight = 1.25
)

// summaryRunes caps the body excerpt kept in the index (just-in-time design:
// the full body is re-read from the markdown file only when needed).
const summaryRunes = 400

// Hit is one search result with just-in-time fields: the summary (not the
// full body) plus the metadata needed for ranking and follow-up file reads.
type Hit struct {
	ID        string
	Path      string // truth-source file for full-body follow-up reads
	Domain    string
	Kind      string
	Name      string
	Keywords  []string
	Triggers  []string
	Summary   string
	Status    string
	Pinned    bool
	CreatedAt time.Time
	Score     float64 // relevance × recency × pinned × domain
}

// indexDoc is one indexed memory document (one cache row).
type indexDoc struct {
	ID        string
	Path      string
	Domain    string
	Kind      string
	Name      string
	Keywords  []string
	Triggers  []string
	Summary   string
	Status    string
	Pinned    bool
	CreatedAt time.Time
}

// backend stores index docs and answers token queries with a relevance
// score per matching doc (higher is better).
type backend interface {
	upsert(docs []indexDoc) error
	remove(ids []string) error
	// replaceUnder drops all cached rows whose file lives under root and
	// inserts docs, keeping docs outside root untouched.
	replaceUnder(root string, docs []indexDoc) error
	match(queryTokens []string) ([]matchResult, error)
	close() error
}

// matchResult is one backend hit before ranking factors are applied.
type matchResult struct {
	doc indexDoc
	rel float64
}

// Index is the memory full-text index cache. All methods are safe for
// concurrent use; the markdown files stay the single writer truth source.
type Index struct {
	mu sync.Mutex
	be backend
}

// Open opens (creating when absent) the index cache at path, e.g.
// ~/.chyan/memory-index.db. FTS5 is used when available; otherwise the same
// file stores a pure-Go inverted index as JSON. A cache file in the wrong
// format is discarded and recreated: it is always derivable via Rebuild.
func Open(path string) (*Index, error) {
	return openIndex(path, false)
}

// FTS5Available reports whether the linked SQLite build supports FTS5
// virtual tables (verified at runtime, not by build flags).
func FTS5Available() bool {
	return fts5Available()
}

// openIndex opens the cache; forcePostings pins the pure-Go fallback
// backend (used to exercise the fallback path in tests).
func openIndex(path string, forcePostings bool) (*Index, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("memory: index path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if !forcePostings && fts5Available() {
		be, err := newFTSBackend(path)
		if err != nil {
			return nil, err
		}
		return &Index{be: be}, nil
	}
	be, err := newPostingsBackend(path)
	if err != nil {
		return nil, err
	}
	return &Index{be: be}, nil
}

// Rebuild scans the markdown truth source under one memory root
// (<root>/<kind>/<id>-<slug>.md) and refreshes its cached rows in place:
// rows of files under root are replaced, rows of other roots survive. Files
// with broken frontmatter are skipped with a warning (never fatal), so
// running Rebuild twice yields identical results. Superseded memories are
// indexed too and filtered at Search time, mirroring the truth source.
func (x *Index) Rebuild(root string) error {
	docs, err := scanRoot(root)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.be.replaceUnder(filepath.Clean(root), docs)
}

// Search ranks memories matching every token of query (CJK bigram + English
// word tokens, same tokenizer as the file store). Final score is
// relevance × recency × pinned × domain (architecture 4.3). Superseded
// memories are hidden; limit <= 0 returns all hits. An empty query yields
// no hits and no error.
func (x *Index) Search(query string, limit int) ([]Hit, error) {
	qtoks := tokens(query)
	if len(qtoks) == 0 {
		return nil, nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	res, err := x.be.match(dedup(qtoks))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	hits := make([]Hit, 0, len(res))
	for _, r := range res {
		if r.doc.Status == StatusSuperseded {
			continue
		}
		h := hitOf(r.doc)
		h.Score = finalScore(r.rel, r.doc, now)
		hits = append(hits, h)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if !hits[i].CreatedAt.Equal(hits[j].CreatedAt) {
			return hits[i].CreatedAt.After(hits[j].CreatedAt)
		}
		return hits[i].ID > hits[j].ID
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// Add inserts or replaces the index row of one memory (incremental
// maintenance; the md file write is the caller's job). A zero CreatedAt
// becomes now, an empty Domain becomes the global domain.
func (x *Index) Add(m store.Memory) error {
	if m.ID == "" {
		return errors.New("memory: index add requires ID")
	}
	d := docFromMemory(m)
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.Domain == "" {
		d.Domain = DomainGlobal
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.be.upsert([]indexDoc{d})
}

// Remove drops the cached row of one memory id (no error when absent: the
// cache mirrors deletions of the truth source).
func (x *Index) Remove(id string) error {
	if id == "" {
		return errors.New("memory: index remove requires ID")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.be.remove([]string{id})
}

// Close releases the backend (the SQLite handle for the FTS5 backend).
func (x *Index) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.be.close()
}

// scanRoot reads all memory files under one root (like scanDomain but keeps
// every status and a per-root domain).
func scanRoot(root string) ([]indexDoc, error) {
	domain := domainOfRoot(root)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var docs []indexDoc
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			path := filepath.Join(root, e.Name(), f.Name())
			m, ok := readMemory(path, f.Name(), e.Name(), domain, "")
			if !ok {
				continue
			}
			docs = append(docs, docFromMemory(m))
		}
	}
	return docs, nil
}

// docFromMemory derives the cache row of one truth-source memory.
func docFromMemory(m store.Memory) indexDoc {
	return indexDoc{
		ID:        m.ID,
		Path:      m.Path,
		Domain:    m.Domain,
		Kind:      m.Kind,
		Name:      m.Name,
		Keywords:  m.Keywords,
		Triggers:  m.Triggers,
		Summary:   summaryOf(m.Content),
		Status:    m.Status,
		Pinned:    m.Pinned,
		CreatedAt: m.CreatedAt,
	}
}

func hitOf(d indexDoc) Hit {
	return Hit{
		ID:        d.ID,
		Path:      d.Path,
		Domain:    d.Domain,
		Kind:      d.Kind,
		Name:      d.Name,
		Keywords:  d.Keywords,
		Triggers:  d.Triggers,
		Summary:   d.Summary,
		Status:    d.Status,
		Pinned:    d.Pinned,
		CreatedAt: d.CreatedAt,
	}
}

// docText is the indexed vocabulary source of one document: name +
// keywords + triggers + body summary.
func docText(d indexDoc) string {
	return strings.Join([]string{
		d.Name,
		strings.Join(d.Keywords, " "),
		strings.Join(d.Triggers, " "),
		d.Summary,
	}, "\n")
}

// summaryOf returns the body excerpt kept in the index.
func summaryOf(content string) string {
	s := strings.TrimSpace(content)
	r := []rune(s)
	if len(r) > summaryRunes {
		return string(r[:summaryRunes])
	}
	return s
}

// finalScore combines backend relevance with the ranking factors.
func finalScore(rel float64, d indexDoc, now time.Time) float64 {
	s := rel * recencyWeight(d.CreatedAt, now) * domainWeight(d.Domain)
	if d.Pinned {
		s *= pinnedWeight
	}
	return s
}

// recencyWeight decays with age: 1.0 fresh, 0.5 after recencyWindowDays.
func recencyWeight(created, now time.Time) float64 {
	if created.IsZero() {
		return 1
	}
	age := now.Sub(created).Hours() / 24
	if age < 0 {
		age = 0
	}
	return 1 / (1 + age/recencyWindowDays)
}

// domainWeight implements the domain priority of architecture 4.3
// (project > project-local > global).
func domainWeight(domain string) float64 {
	switch domain {
	case DomainProject:
		return projectWeight
	case DomainProjectLocal:
		return projectLocalWeight
	default:
		return 1
	}
}

// domainOfRoot infers the domain of a memory root from its conventional
// layout: <repo>/.agents/local/memory is project-local, <repo>/.agents/memory
// is project (global when it sits directly under the user's home, covering
// the ~/.agents/memory layout), anything else is global.
func domainOfRoot(root string) string {
	clean := filepath.Clean(root)
	base := filepath.Base(clean)
	parent := filepath.Dir(clean)
	if base == "memory" && filepath.Base(parent) == "local" && filepath.Base(filepath.Dir(parent)) == ".agents" {
		return DomainProjectLocal
	}
	// Global: ~/.chyan/memory (current) or ~/.agents/memory (legacy layout).
	if base == "memory" && (filepath.Base(parent) == ".chyan" || filepath.Base(parent) == ".agents") {
		if home, err := userHomeDir(); err == nil && filepath.Dir(parent) == filepath.Clean(home) {
			return DomainGlobal
		}
	}
	if base == "memory" && filepath.Base(parent) == ".agents" {
		return DomainProject
	}
	return DomainGlobal
}

// userHomeDir is os.UserHomeDir as an injectable seam (tests cover the
// global-domain detection without touching the real home).
var userHomeDir = os.UserHomeDir

// underRoot reports whether a file path lives under a memory root.
func underRoot(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// indexTokens is the indexed vocabulary of one text: tokens() output plus
// CJK unigrams, so single-rune CJK queries hit docs whose runs were split
// into bigrams (the FTS5 backend cannot substring-match).
func indexTokens(text string) []string {
	toks := tokens(text)
	var cjk []rune
	flush := func() {
		for _, r := range cjk {
			toks = append(toks, string(r))
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		if isCJK(r) {
			cjk = append(cjk, r)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

func dedup(toks []string) []string {
	seen := make(map[string]bool, len(toks))
	out := toks[:0]
	for _, t := range toks {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

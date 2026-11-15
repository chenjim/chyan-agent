package memory

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"chyan.tech/chyan/internal/frontmatter"
	"chyan.tech/chyan/internal/store"
	"gopkg.in/yaml.v3"
)

// Domains of the memory layout (architecture 4.3). P2 supports global and
// project; project-local lands in P4-3.
const (
	DomainGlobal       = "global"
	DomainProject      = "project"
	DomainProjectLocal = "project-local"
)

// Status values of a memory (architecture 4.3). Superseded memories are
// hidden from search/recall.
const (
	StatusActive     = "active"
	StatusSuperseded = "superseded"
)

// Query describes one memory lookup.
type Query struct {
	Text         string   // keywords, multi-word AND (tokenized)
	Kinds        []string // fact|kb|model|directive; empty = all kinds
	ProjectPath  string   // non-empty: search this project domain plus global
	TopK         int      // <= 0: no limit
	ContentMatch string   // extra raw text matched against the body only
}

// errIndexDegraded reports that a cache root failed to rebuild; callers fall
// back to scanning the markdown truth source.
var errIndexDegraded = errors.New("memory: index degraded")

// FileStore persists memories as markdown files:
// <root>/<kind>/<id>-<slug>.md. The global root is fixed at construction
// (production: ~/.chyan/memory); project roots are <repo>/.agents/memory.
// A mutex serializes writes (single-writer requirement); reads are lock-free
// file scans.
type FileStore struct {
	mu       sync.Mutex
	global   string            // memory root of the global domain
	projects map[string]string // repo path -> memory root, learned on use
	// index is the optional derived full-text cache (architecture 4.3). When
	// attached, writes keep it in sync and lookups prefer it; nil means every
	// lookup scans the markdown truth source.
	index *Index
	// indexed marks roots already rebuilt in this process, so a lookup does
	// not rescan the whole root on every call.
	indexed map[string]bool
	// broken marks roots whose rebuild failed: lookups must not trust (or
	// retry) the cache for those roots and fall back to scanning instead.
	broken map[string]bool
}

// New returns a FileStore rooted at globalRoot (e.g. ~/.agents/memory).
func New(globalRoot string) *FileStore {
	return &FileStore{
		global: globalRoot, projects: map[string]string{}, indexed: map[string]bool{},
		broken: map[string]bool{},
	}
}

// SetIndex attaches (or detaches with nil) the full-text cache. Call it
// during wiring, before concurrent lookups start.
func (s *FileStore) SetIndex(x *Index) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index = x
	s.indexed = map[string]bool{}
	s.broken = map[string]bool{}
}

// getIndex returns the attached cache (nil when scan-only).
func (s *FileStore) getIndex() *Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index
}

// ensureIndexed rebuilds root in the cache at most once per process and
// reports the rebuild outcome. The markdown files stay the truth source, so
// callers fall back to scanning when the cache cannot be primed.
func (s *FileStore) ensureIndexed(x *Index, root string) error {
	if root == "" {
		return nil
	}
	s.mu.Lock()
	if s.broken[root] {
		s.mu.Unlock()
		return errIndexDegraded
	}
	done := s.indexed[root]
	s.indexed[root] = true
	s.mu.Unlock()
	if done {
		return nil
	}
	if err := x.Rebuild(root); err != nil {
		// Never serve hits from a root we failed to prime: mark it degraded so
		// later lookups (and other goroutines) go straight to the file scan.
		s.mu.Lock()
		s.broken[root] = true
		s.mu.Unlock()
		log.Printf("memory: index rebuild %s degraded: %v", root, err)
		return err
	}
	return nil
}

// NewDefault roots the store at ~/.chyan/memory (chyan's own data root).
func NewDefault() (*FileStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return New(filepath.Join(home, ".chyan", "memory")), nil
}

// AddMemory writes one memory to its domain file and returns the stored copy
// (generated id, resolved domain/status, file path). Unset Domain is inferred
// from ProjectPath; unset Status becomes "active"; an explicit ID must not
// already exist in the domain.
func (s *FileStore) AddMemory(ctx context.Context, m store.Memory) (store.Memory, error) {
	if err := ctx.Err(); err != nil {
		return store.Memory{}, err
	}
	domain := m.Domain
	if domain == "" {
		if m.ProjectPath == "" {
			domain = DomainGlobal
		} else {
			domain = DomainProject
		}
	}
	if err := validate(m, domain); err != nil {
		return store.Memory{}, err
	}
	status := m.Status
	if status == "" {
		status = StatusActive
	}
	slug := slugify(m.Name)
	if slug == "" {
		slug = "mem"
	}
	if m.ID != "" {
		if err := validID(m.ID); err != nil {
			return store.Memory{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	root := s.rootLocked(domain, m.ProjectPath)
	id := m.ID
	var rel string
	if id != "" {
		// Uniqueness is per id, not per file name: the same id must not
		// resolve to an existing file whatever slug it was saved under.
		found, err := findByID(root, id)
		if err != nil {
			return store.Memory{}, err
		}
		if len(found) > 0 {
			return store.Memory{}, fmt.Errorf("memory: id %q already exists in %s", id, domain)
		}
		rel = filepath.Join(m.Kind, id+"-"+slug+".md")
	} else {
		// Re-roll generated ids on the unlikely same-millisecond collision.
		for i := 0; ; i++ {
			id = newID(time.Now().UTC())
			rel = filepath.Join(m.Kind, id+"-"+slug+".md")
			found, err := findByID(root, id)
			if err != nil {
				return store.Memory{}, err
			}
			if len(found) == 0 {
				break
			}
			if i == 2 {
				return store.Memory{}, errors.New("memory: cannot allocate unique id")
			}
		}
	}
	mm := &meta{
		Name:       m.Name,
		Keywords:   m.Keywords,
		Triggers:   m.Triggers,
		Type:       m.Type,
		Status:     status,
		Supersedes: m.Supersedes,
		Source:     m.Source,
		Pinned:     m.Pinned,
		ID:         id,
	}
	body := renderFrontmatter(mm)
	if strings.TrimSpace(m.Content) != "" {
		body += strings.TrimSpace(m.Content) + "\n"
	}
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return store.Memory{}, err
	}
	if err := writeFileAtomic(full, []byte(body), 0o644); err != nil {
		return store.Memory{}, err
	}
	m.ID = id
	m.Path = full
	m.Domain = domain
	m.Status = status
	m.CreatedAt = time.Now().UTC()
	// Keep the derived cache fresh; the markdown write above is the truth.
	if s.index != nil {
		if err := s.index.Add(m); err != nil {
			log.Printf("memory: index add degraded: %v", err)
		}
	}
	return m, nil
}

// writeFileAtomic replaces path with data through a same-directory temp file
// plus rename. A crash mid-write can never leave a truncated memory file:
// readers see either the old content or the complete new one. The temp file
// is removed on every error path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := f.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// SearchMemories runs a keyword lookup: every token of q.Text must occur in
// name+keywords+triggers+body, and q.ContentMatch (if set) must occur raw in
// the body. Ranking: project domain before global, then token hit count, then
// newer id first. Superseded memories are excluded.
func (s *FileStore) SearchMemories(ctx context.Context, q Query) ([]store.Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The cache keeps the same contract (all tokens, project > global) but
	// needs the whole scan only when it is absent or a body-only match is
	// requested (the index keeps a summary, not the full body).
	if x := s.getIndex(); x != nil && q.ContentMatch == "" {
		if hits, err := s.searchIndex(x, q); err == nil {
			return hits, nil
		} else {
			log.Printf("memory: index search degraded: %v", err)
		}
	}
	terms := tokens(q.Text)
	hits, err := s.collect(q, func(m store.Memory, h hay) bool {
		return h.match(terms) && contentMatch(m, q.ContentMatch)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		di, dj := hits[i].Domain == DomainProject, hits[j].Domain == DomainProject
		if di != dj {
			return di
		}
		si, sj := hayOf(hits[i]).score(terms), hayOf(hits[j]).score(terms)
		if si != sj {
			return si > sj
		}
		return hits[i].ID > hits[j].ID
	})
	return truncate(hits, q.TopK), nil
}

// TriggerMatches returns active memories whose trigger cue occurs in text
// (case-insensitive). Triggers are scenario phrases, not search keywords, so
// the check is a substring match: a lesson resurfaces before the same mistake
// is repeated. Project memories rank before global ones.
func (s *FileStore) TriggerMatches(ctx context.Context, text, projectPath string, topK int) ([]store.Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lower := strings.ToLower(text)
	if strings.TrimSpace(lower) == "" {
		return nil, nil
	}
	hits, err := s.collect(Query{ProjectPath: projectPath}, func(m store.Memory, _ hay) bool {
		for _, tr := range m.Triggers {
			tr = strings.TrimSpace(strings.ToLower(tr))
			if tr != "" && strings.Contains(lower, tr) {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		di, dj := hits[i].Domain == DomainProject, hits[j].Domain == DomainProject
		if di != dj {
			return di
		}
		return hits[i].ID > hits[j].ID
	})
	return truncate(hits, topK), nil
}

// searchIndex runs one lookup against the attached cache. Roots are rebuilt
// lazily so files written before this process started become visible; hits are
// filtered to the requested domains/kinds and mapped back to truth-source
// memories (the index keeps only a summary, so the body is re-read on demand).
func (s *FileStore) searchIndex(x *Index, q Query) ([]store.Memory, error) {
	projectRoot := ""
	if err := s.ensureIndexed(x, s.global); err != nil {
		return nil, err
	}
	if q.ProjectPath != "" {
		projectRoot = filepath.Join(q.ProjectPath, ".agents", "memory")
		s.Track(q.ProjectPath)
		if err := s.ensureIndexed(x, projectRoot); err != nil {
			return nil, err
		}
	}
	hits, err := x.Search(q.Text, 0)
	if err != nil {
		return nil, err
	}
	out := make([]store.Memory, 0, len(hits))
	for _, h := range hits {
		if !indexHitVisible(h, projectRoot, q.Kinds) {
			continue
		}
		out = append(out, memoryOfHit(h))
	}
	return truncate(out, q.TopK), nil
}

// indexHitVisible applies the same domain/kind visibility as the file scan:
// with no project path only the global domain is searched, and project rows
// from other repositories never leak.
func indexHitVisible(h Hit, projectRoot string, kinds []string) bool {
	if len(kinds) > 0 {
		ok := false
		for _, k := range kinds {
			if k == h.Kind {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if projectRoot == "" {
		return h.Domain == DomainGlobal
	}
	if h.Domain == DomainGlobal {
		return true
	}
	return h.Domain == DomainProject && underRoot(h.Path, projectRoot)
}

// memoryOfHit maps a cached hit back to a truth-source memory. The file body
// is re-read (just-in-time loading); if the file is gone the summary stands in.
func memoryOfHit(h Hit) store.Memory {
	if m, ok := readMemory(h.Path, filepath.Base(h.Path), h.Kind, h.Domain, ""); ok {
		return m
	}
	return store.Memory{
		ID: h.ID, Path: h.Path, Domain: h.Domain, Kind: h.Kind, Name: h.Name,
		Keywords: h.Keywords, Triggers: h.Triggers, Content: h.Summary,
		Status: h.Status, Pinned: h.Pinned, CreatedAt: h.CreatedAt,
	}
}

// Recall treats a conversation excerpt as a query: it ranks active memories
// by token overlap with name+keywords+triggers+body (CJK bigram + English
// word tokens), project domain before global, and returns the topK most
// relevant. It searches the global domain only plus, when projectPath is set,
// that project domain.
func (s *FileStore) Recall(ctx context.Context, contextText, projectPath string, topK int) ([]store.Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	qTokens := tokens(contextText)
	if len(qTokens) == 0 {
		return nil, nil
	}
	// Prefer the cache when it yields matches; an all-token miss (typical for
	// long conversation excerpts) falls back to the overlap scan below.
	if x := s.getIndex(); x != nil {
		if hits, err := s.searchIndex(x, Query{Text: contextText, ProjectPath: projectPath}); err == nil && len(hits) > 0 {
			return truncate(hits, topK), nil
		}
	}
	hits, err := s.collect(Query{ProjectPath: projectPath}, nil)
	if err != nil {
		return nil, err
	}
	type scored struct {
		m store.Memory
		n int
	}
	var list []scored
	for _, m := range hits {
		n := hayOf(m).overlap(qTokens)
		if n > 0 {
			list = append(list, scored{m, n})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		di, dj := list[i].m.Domain == DomainProject, list[j].m.Domain == DomainProject
		if di != dj {
			return di
		}
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].m.ID > list[j].m.ID
	})
	out := make([]store.Memory, 0, len(list))
	for _, sc := range list {
		out = append(out, sc.m)
	}
	return truncate(out, topK), nil
}

// ListMemories returns the active memories of the requested domains (global
// plus projectPath when set), scanned from the markdown truth source. Unlike
// SearchMemories it does not need a query, so the Web browser can show every
// active memory without going through the index (which ignores empty queries).
func (s *FileStore) ListMemories(ctx context.Context, projectPath string, kinds []string) ([]store.Memory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.collect(Query{ProjectPath: projectPath, Kinds: kinds}, nil)
}

// GetMemory returns one active memory by id, looked up in the global domain
// and the given project domain.
func (s *FileStore) GetMemory(ctx context.Context, id, projectPath string) (store.Memory, error) {
	if err := ctx.Err(); err != nil {
		return store.Memory{}, err
	}
	if err := validID(id); err != nil {
		return store.Memory{}, err
	}
	hits, err := s.collect(Query{ProjectPath: projectPath}, nil)
	if err != nil {
		return store.Memory{}, err
	}
	for _, m := range hits {
		if m.ID == id {
			return m, nil
		}
	}
	return store.Memory{}, fmt.Errorf("memory: %w: %s", os.ErrNotExist, id)
}

// Supersede marks one memory invalid by flipping its frontmatter status to
// "superseded" (the file stays as history; recall/search ignore it). It is
// the Web "标记失效" action: safer than deleting, since the evidence survives.
func (s *FileStore) Supersede(ctx context.Context, id, projectPath string) (store.Memory, error) {
	if err := ctx.Err(); err != nil {
		return store.Memory{}, err
	}
	if err := validID(id); err != nil {
		return store.Memory{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	roots := make([]string, 0, len(s.projects)+2)
	seen := map[string]bool{}
	addRoot := func(root string) {
		if root != "" && !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	addRoot(s.global)
	for _, r := range s.projectRootsLocked() {
		addRoot(r)
	}
	if projectPath != "" {
		addRoot(s.rootLocked(DomainProject, projectPath))
	}
	var paths []string
	for _, root := range roots {
		found, err := findByID(root, id)
		if err != nil {
			return store.Memory{}, err
		}
		paths = append(paths, found...)
	}
	if len(paths) == 0 {
		return store.Memory{}, fmt.Errorf("memory: %w: %s", os.ErrNotExist, id)
	}
	if len(paths) > 1 {
		return store.Memory{}, fmt.Errorf("memory: id %q is ambiguous (%d files)", id, len(paths))
	}
	path := paths[0]
	data, err := os.ReadFile(path)
	if err != nil {
		return store.Memory{}, err
	}
	fm, body, err := frontmatter.Split(data)
	if err != nil {
		return store.Memory{}, fmt.Errorf("memory: %s: %w", path, err)
	}
	var mm meta
	if err := yaml.Unmarshal(fm, &mm); err != nil {
		return store.Memory{}, fmt.Errorf("memory: %s: %w", path, err)
	}
	mm.Status = StatusSuperseded
	updated := renderFrontmatter(&mm) + body
	if body != "" {
		updated += "\n"
	}
	if err := writeFileAtomic(path, []byte(updated), 0o644); err != nil {
		return store.Memory{}, err
	}
	if s.index != nil {
		if err := s.index.Remove(id); err != nil {
			log.Printf("memory: index remove degraded: %v", err)
		}
	}
	return readMemoryValue(path, id, mm, body), nil
}

// readMemoryValue builds the returned copy after a supersede write.
func readMemoryValue(path, id string, mm meta, body string) store.Memory {
	created := time.Time{}
	if fi, err := os.Stat(path); err == nil {
		created = fi.ModTime()
	}
	return store.Memory{
		ID: id, Path: path, Kind: memoryKind(path), Name: mm.Name,
		Type: mm.Type, Keywords: mm.Keywords, Triggers: mm.Triggers,
		Content: body, Source: mm.Source, Status: mm.Status,
		Supersedes: mm.Supersedes, Pinned: mm.Pinned, CreatedAt: created,
	}
}

// memoryKind derives the kind directory of a memory file path.
func memoryKind(path string) string {
	return filepath.Base(filepath.Dir(path))
}

// DeleteMemory physically deletes the memory file (architecture 4.3: delete
// = physical removal). The id is looked up in the global domain plus every
// project domain this store has seen (via AddMemory/Search/Recall/Track).
func (s *FileStore) DeleteMemory(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	roots := append([]string{s.global}, s.projectRootsLocked()...)
	var paths []string
	for _, root := range roots {
		found, err := findByID(root, id)
		if err != nil {
			return err
		}
		paths = append(paths, found...)
	}
	switch len(paths) {
	case 0:
		return fmt.Errorf("memory: %w: %s", os.ErrNotExist, id)
	case 1:
		if err := os.Remove(paths[0]); err != nil {
			return err
		}
		if s.index != nil {
			_ = s.index.Remove(id)
		}
		return nil
	default:
		return fmt.Errorf("memory: id %q is ambiguous (%d files)", id, len(paths))
	}
}

// Track registers a project root so DeleteMemory can reach its domain files.
func (s *FileStore) Track(projectPath string) {
	s.track(projectPath)
}

// collect scans both domains (project first) and filters with keep, which
// may be nil (keep all).
func (s *FileStore) collect(q Query, keep func(store.Memory, hay) bool) ([]store.Memory, error) {
	var hits []store.Memory
	if q.ProjectPath != "" {
		root := s.track(q.ProjectPath)
		found, err := scanDomain(root, DomainProject, q.ProjectPath, q.Kinds)
		if err != nil {
			return nil, err
		}
		hits = append(hits, found...)
	}
	found, err := scanDomain(s.global, DomainGlobal, "", q.Kinds)
	if err != nil {
		return nil, err
	}
	hits = append(hits, found...)
	if keep == nil {
		return hits, nil
	}
	out := hits[:0]
	for _, m := range hits {
		if keep(m, hayOf(m)) {
			out = append(out, m)
		}
	}
	return out, nil
}

func truncate(hits []store.Memory, topK int) []store.Memory {
	if topK > 0 && len(hits) > topK {
		return hits[:topK]
	}
	return hits
}

func contentMatch(m store.Memory, raw string) bool {
	if raw == "" {
		return true
	}
	return strings.Contains(strings.ToLower(m.Content), strings.ToLower(raw))
}

func (s *FileStore) track(projectPath string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rootLocked(DomainProject, projectPath)
}

// rootLocked resolves the memory root of a domain and records project roots.
// Callers hold s.mu.
func (s *FileStore) rootLocked(domain, projectPath string) string {
	if domain == DomainGlobal {
		return s.global
	}
	root := filepath.Join(projectPath, ".agents", "memory")
	s.projects[projectPath] = root
	return root
}

func (s *FileStore) projectRootsLocked() []string {
	roots := make([]string, 0, len(s.projects))
	for _, r := range s.projects {
		roots = append(roots, r)
	}
	sort.Strings(roots) // deterministic traversal
	return roots
}

// scanDomain reads one domain's <kind> files. Broken files (bad YAML, invalid
// id) are skipped with a warning; superseded memories are filtered out.
func scanDomain(root, domain, projectPath string, kinds []string) ([]store.Memory, error) {
	ks := kinds
	if len(ks) == 0 {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		ks = ks[:0]
		for _, e := range entries {
			if e.IsDir() {
				ks = append(ks, e.Name())
			}
		}
	}
	var out []store.Memory
	for _, k := range ks {
		dir := filepath.Join(root, k)
		files, err := os.ReadDir(dir)
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
			path := filepath.Join(dir, f.Name())
			m, ok := readMemory(path, f.Name(), k, domain, projectPath)
			if !ok || m.Status == StatusSuperseded {
				continue
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// readMemory parses one memory file. ok is false on malformed files.
func readMemory(path, fileName, kind, domain, projectPath string) (store.Memory, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("memory: skip %s: %v", path, err)
		return store.Memory{}, false
	}
	fm, body, err := frontmatter.Split(data)
	if err != nil {
		log.Printf("memory: skip %s: %v", path, err)
		return store.Memory{}, false
	}
	if fm == nil {
		log.Printf("memory: skip %s: no frontmatter", path)
		return store.Memory{}, false
	}
	var mm meta
	if err := yaml.Unmarshal(fm, &mm); err != nil {
		log.Printf("memory: skip %s: %v", path, err)
		return store.Memory{}, false
	}
	id := mm.ID
	if id == "" {
		id = strings.TrimSuffix(fileName, ".md")
		if i := strings.IndexByte(id, '-'); i > 0 {
			id = id[:i]
		}
	}
	if err := validID(id); err != nil {
		log.Printf("memory: skip %s: %v", path, err)
		return store.Memory{}, false
	}
	created := time.Time{}
	if fi, err := os.Stat(path); err == nil {
		created = fi.ModTime()
	}
	return store.Memory{
		ID:          id,
		Path:        path,
		Domain:      domain,
		ProjectPath: projectPath,
		Kind:        kind,
		Type:        mm.Type,
		Name:        mm.Name,
		Keywords:    mm.Keywords,
		Triggers:    mm.Triggers,
		Content:     body,
		Source:      mm.Source,
		Status:      mm.Status,
		Supersedes:  mm.Supersedes,
		Pinned:      mm.Pinned,
		CreatedAt:   created,
	}, true
}

// findByID locates memory files of one domain by filename or frontmatter id.
func findByID(root, id string) ([]string, error) {
	kinds, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, k := range kinds {
		if !k.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, k.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".md") {
				continue
			}
			path := filepath.Join(root, k.Name(), name)
			base := strings.TrimSuffix(name, ".md")
			if i := strings.IndexByte(base, '-'); i > 0 {
				base = base[:i]
			}
			if base == id {
				paths = append(paths, path)
				continue
			}
			// Frontmatter id fallback for renamed files.
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			fm, _, err := frontmatter.Split(data)
			if err != nil {
				continue
			}
			var mm meta
			if yaml.Unmarshal(fm, &mm) == nil && mm.ID == id {
				paths = append(paths, path)
			}
		}
	}
	return paths, nil
}

// hay is the tokenized searchable text of one memory.
type hay struct {
	set   map[string]int
	lower string
}

func hayOf(m store.Memory) hay {
	h := strings.ToLower(strings.Join([]string{
		m.Name,
		strings.Join(m.Keywords, " "),
		strings.Join(m.Triggers, " "),
		m.Content,
	}, "\n"))
	return hay{set: tokenCounts(tokens(h)), lower: h}
}

// match reports whether every token of terms is present.
func (h hay) match(terms []string) bool {
	for _, t := range terms {
		if !hasTok(h.set, h.lower, t) {
			return false
		}
	}
	return true
}

// score ranks matched memories by token hit count.
func (h hay) score(terms []string) int {
	n := 0
	for _, t := range terms {
		n += h.set[t]
	}
	return n
}

// overlap counts distinct query tokens present in the memory.
func (h hay) overlap(qTokens []string) int {
	return overlap(h.set, h.lower, qTokens)
}

// validate rejects memories that would escape the kind directory or carry
// unusable fields.
func validate(m store.Memory, domain string) error {
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("memory: Name is required")
	}
	if m.Kind == "" {
		return errors.New("memory: Kind is required")
	}
	for _, r := range m.Kind {
		if !isSlugRune(r) {
			return fmt.Errorf("memory: invalid Kind %q", m.Kind)
		}
	}
	switch domain {
	case DomainGlobal, DomainProject:
	default:
		return fmt.Errorf("memory: unsupported Domain %q", domain)
	}
	if domain == DomainGlobal && m.ProjectPath != "" {
		return errors.New("memory: global domain must not set ProjectPath")
	}
	if domain == DomainProject && m.ProjectPath == "" {
		return errors.New("memory: project domain requires ProjectPath")
	}
	switch m.Status {
	case "", StatusActive, StatusSuperseded:
	default:
		return fmt.Errorf("memory: unknown Status %q", m.Status)
	}
	return nil
}

func validID(id string) error {
	if id == "" || len(id) > 64 {
		return fmt.Errorf("memory: invalid id %q", id)
	}
	for _, r := range id {
		if !isSlugRune(r) {
			return fmt.Errorf("memory: invalid id %q", id)
		}
	}
	return nil
}

func isSlugRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

// Night dream pass: collapse near-duplicate memories into supersede chains and
// report project-attribution and stale-orphan candidates in a rollbackable report.
package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"chyan.tech/chyan/internal/frontmatter"
	"chyan.tech/chyan/internal/store"
	"gopkg.in/yaml.v3"
)

// Default thresholds of the dream pass (architecture 4.3).
const (
	// DreamNearDupRatio is the minimum distinct-token overlap ratio for two
	// active memories to count as near-duplicates (the pair is examined from
	// both sides, so the smaller token set is the effective denominator).
	DreamNearDupRatio = 0.8
	// DreamOrphanAge is the age past which a superseded memory is reported
	// as an orphan candidate (counted only; deletion is a user decision).
	DreamOrphanAge = 30 * 24 * time.Hour
)

// DreamChange kinds recorded in a DreamReport (each is one rollbackable
// field change).
const (
	DreamChangeSupersede = "supersede" // status flip of the superseded entry
	DreamChangeLink      = "link"      // supersedes pointer of the survivor
	DreamChangeNote      = "note"      // attribution marker appended to the body
)

// pathPattern matches explicit paths in memory bodies: "/x/y/..." (3+ path
// segments) or "~/x/..." (2+). The segment class excludes whitespace, quotes
// and CJK punctuation so a path ends where prose resumes.
const pathPattern = "~(?:/[^/\\s\"'`，。、；：！？]+){2,}|(?:/[^/\\s\"'`，。、；：！？]+){3,}"

var (
	pathRe       = regexp.MustCompile(pathPattern)
	volPathRe    = regexp.MustCompile(`^/vol[0-9]+/`)
	projectSegRe = regexp.MustCompile(`(?i)/project/`)
)

// DreamConfig configures one Dreamer. Now is the clock seam for tests; nil
// means time.Now.
type DreamConfig struct {
	Root  string // memory root whose <kind> dirs are dreamt (e.g. ~/.agents/memory)
	Index *Index // optional cache kept in sync via Add/Remove
	Now   func() time.Time
}

// DreamChange is one rollbackable modification of a dream cycle: the memory
// id touched plus the changed field's value before and after.
type DreamChange struct {
	ID     string
	Kind   string // DreamChangeSupersede | DreamChangeLink | DreamChangeNote
	Before string
	After  string
}

// DreamReport is one dream cycle report (architecture 4.3: pushed to the
// user; Rollback undoes exactly its Changes and nothing else).
type DreamReport struct {
	Examined   int // active memories scanned
	Merged     int // near-duplicate pairs collapsed into the newer entry
	Superseded int // entries flipped to status=superseded
	Orphaned   int // superseded entries stale past DreamOrphanAge (candidates)
	Notes      []string
	Changes    []DreamChange
}

// Dreamer runs the night dream pass over one memory root: the markdown truth
// files are rewritten in place and the Index, when set, follows each change.
type Dreamer struct {
	root  string
	index *Index
	now   func() time.Time
}

// NewDreamer returns a Dreamer for cfg.Root.
func NewDreamer(cfg DreamConfig) *Dreamer {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Dreamer{root: cfg.Root, index: cfg.Index, now: now}
}

// dreamEntry is one memory file on disk with the fields the pass needs.
type dreamEntry struct {
	mem  store.Memory
	path string
	fm   meta
	body string
}

// Run executes one dream cycle: near-duplicate pairs are merged into the
// supersede chain of the newer entry, global memories naming explicit
// project paths are flagged for migration (report only: the pass never moves
// files), and stale superseded entries are counted as orphan candidates.
// Every change is recorded in the report so Rollback can undo this cycle.
func (d *Dreamer) Run(ctx context.Context) (DreamReport, error) {
	var rep DreamReport
	entries, err := d.scan()
	if err != nil {
		return rep, err
	}
	active := make([]*dreamEntry, 0, len(entries))
	for _, e := range entries {
		if e.mem.Status != StatusSuperseded {
			active = append(active, e)
		}
	}
	rep.Examined = len(active)

	// 1) Near-duplicates: the newer entry survives, the older is superseded.
	// Each entry takes part in at most one merge per cycle (chains build over
	// successive cycles), so pairing order cannot cascade.
	used := make(map[string]bool)
	for i := 0; i < len(active); i++ {
		for j := i + 1; j < len(active); j++ {
			a, b := active[i], active[j]
			if used[a.mem.ID] || used[b.mem.ID] || !nearDuplicate(a, b) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			older, newer := a, b
			if newerOf(a, b) == a {
				older, newer = b, a
			}
			if err := d.mergePair(older, newer, &rep); err != nil {
				return rep, err
			}
			used[older.mem.ID], used[newer.mem.ID] = true, true
			rep.Merged++
			rep.Superseded++
			rep.Notes = append(rep.Notes,
				fmt.Sprintf("合并：%s 被 %s 替代（近重复）", older.mem.ID, newer.mem.ID))
		}
	}

	// 2) Project attribution: global memories naming explicit project paths
	// are flagged for migration only (safety boundary: never moved).
	for _, e := range active {
		if e.mem.Domain != DomainGlobal {
			continue
		}
		p := firstProjectPath(e.body)
		if p == "" {
			continue
		}
		recorded, err := d.note(e, "建议迁移到项目域: "+p, &rep)
		if err != nil {
			return rep, err
		}
		if recorded {
			rep.Notes = append(rep.Notes,
				fmt.Sprintf("建议迁移到项目域：%s -> %s", e.mem.ID, p))
		}
	}

	// 3) Orphan candidates: superseded for over DreamOrphanAge (age judged
	// against the file mtime, the supersede write time), counted only.
	for _, e := range entries {
		if e.mem.Status != StatusSuperseded {
			continue
		}
		var mod time.Time
		if fi, err := os.Stat(e.path); err == nil {
			mod = fi.ModTime()
		}
		if d.now().Sub(mod) > DreamOrphanAge {
			rep.Orphaned++
			rep.Notes = append(rep.Notes,
				fmt.Sprintf("孤儿候选：%s 已 superseded 超过 30 天（删除由用户决定）", e.mem.ID))
		}
	}
	return rep, nil
}

// Rollback undoes exactly the changes of one report, in reverse order:
// superseded statuses and supersedes pointers are restored and note markers
// removed. Files deleted since the run are skipped.
func (d *Dreamer) Rollback(ctx context.Context, rep DreamReport) error {
	for i := len(rep.Changes) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := rep.Changes[i]
		e, ok, err := d.loadID(c.ID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		switch c.Kind {
		case DreamChangeSupersede:
			e.fm.Status = c.Before
		case DreamChangeLink:
			e.fm.Supersedes = c.Before
		case DreamChangeNote:
			marker := noteMarker(c.After)
			if marker == "" || !strings.Contains(e.body, marker) {
				continue
			}
			e.body = strings.TrimSpace(strings.ReplaceAll(e.body, marker, ""))
		default:
			continue
		}
		if err := d.save(e); err != nil {
			return err
		}
	}
	return nil
}

// mergePair supersedes older in favor of newer: older flips to
// status=superseded and newer.supersedes points at older. Older keeps its
// own supersedes link, so over successive cycles the chain stays complete
// (A<-B<-C: C.supersedes=B, B.supersedes=A). The supersede stamps older's
// mtime (the orphan age clock); both files are rewritten and the index
// synced.
func (d *Dreamer) mergePair(older, newer *dreamEntry, rep *DreamReport) error {
	prevStatus, prevLink := older.fm.Status, newer.fm.Supersedes
	older.fm.Status = StatusSuperseded
	if err := d.save(older); err != nil {
		return err
	}
	if err := os.Chtimes(older.path, d.now(), d.now()); err != nil {
		// Undo the half merge: leaving older superseded without its link
		// would drop a live memory for no reason and Rollback could not see it.
		older.fm.Status = prevStatus
		_ = d.save(older)
		return err
	}
	root := older.mem.ID
	newer.fm.Supersedes = root
	if err := d.save(newer); err != nil {
		older.fm.Status = prevStatus
		_ = d.save(older)
		return err
	}
	// Both files are on disk: only now is a reversible change recorded.
	rep.Changes = append(rep.Changes, DreamChange{
		ID: older.mem.ID, Kind: DreamChangeSupersede,
		Before: prevStatus, After: StatusSuperseded,
	})
	rep.Changes = append(rep.Changes, DreamChange{
		ID: newer.mem.ID, Kind: DreamChangeLink,
		Before: prevLink, After: root,
	})
	return nil
}

// note appends an attribution marker to the body (visible in the truth file,
// removed by Rollback) and records the change. It reports whether a change
// was recorded: an already-marked body is left untouched (idempotence).
func (d *Dreamer) note(e *dreamEntry, after string, rep *DreamReport) (bool, error) {
	marker := noteMarker(after)
	if marker == "" || strings.Contains(e.body, marker) {
		return false, nil
	}
	e.body = strings.TrimSpace(e.body + "\n" + marker)
	if err := d.save(e); err != nil {
		return false, err
	}
	rep.Changes = append(rep.Changes, DreamChange{
		ID: e.mem.ID, Kind: DreamChangeNote, Before: "", After: after,
	})
	return true, nil
}

// scan reads every memory file under root (all statuses, like scanRoot).
// Broken files are skipped with a warning, never fatal.
func (d *Dreamer) scan() ([]*dreamEntry, error) {
	domain := domainOfRoot(d.root)
	kinds, err := os.ReadDir(d.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*dreamEntry
	for _, k := range kinds {
		if !k.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(d.root, k.Name()))
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
			path := filepath.Join(d.root, k.Name(), f.Name())
			e, ok := d.readEntry(path, f.Name(), k.Name(), domain)
			if ok {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// readEntry parses one memory file; ok is false on malformed files.
func (d *Dreamer) readEntry(path, fileName, kind, domain string) (*dreamEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	fm, body, err := frontmatter.Split(data)
	if err != nil || fm == nil {
		return nil, false
	}
	var mm meta
	if err := yaml.Unmarshal(fm, &mm); err != nil {
		return nil, false
	}
	id := mm.ID
	if id == "" {
		id = strings.TrimSuffix(fileName, ".md")
		if i := strings.IndexByte(id, '-'); i > 0 {
			id = id[:i]
		}
	}
	if err := validID(id); err != nil {
		return nil, false
	}
	created := time.Time{}
	if fi, err := os.Stat(path); err == nil {
		created = fi.ModTime()
	}
	return &dreamEntry{
		path: path,
		fm:   mm,
		body: body,
		mem: store.Memory{
			ID:         id,
			Path:       path,
			Domain:     domain,
			Kind:       kind,
			Name:       mm.Name,
			Keywords:   mm.Keywords,
			Triggers:   mm.Triggers,
			Content:    body,
			Source:     mm.Source,
			Status:     mm.Status,
			Supersedes: mm.Supersedes,
			Pinned:     mm.Pinned,
			CreatedAt:  created,
		},
	}, true
}

// loadID locates one memory file by id.
func (d *Dreamer) loadID(id string) (*dreamEntry, bool, error) {
	entries, err := d.scan()
	if err != nil {
		return nil, false, err
	}
	for _, e := range entries {
		if e.mem.ID == id {
			return e, true, nil
		}
	}
	return nil, false, nil
}

// save rewrites one memory file (frontmatter + body, the AddMemory write
// path) and syncs the index. The original mtime is preserved: the mtime is
// the CreatedAt proxy for files without timestamps, so rewrites must not
// make an entry look newer (mergePair stamps the supersede time explicitly).
func (d *Dreamer) save(e *dreamEntry) error {
	var mod time.Time
	if fi, err := os.Stat(e.path); err == nil {
		mod = fi.ModTime()
	}
	body := renderFrontmatter(&e.fm)
	if strings.TrimSpace(e.body) != "" {
		body += strings.TrimSpace(e.body) + "\n"
	}
	if err := writeFileAtomic(e.path, []byte(body), 0o644); err != nil {
		return err
	}
	if !mod.IsZero() {
		if err := os.Chtimes(e.path, mod, mod); err != nil {
			return err
		}
	}
	e.mem.Status = e.fm.Status
	e.mem.Supersedes = e.fm.Supersedes
	e.mem.Content = e.body
	if d.index != nil {
		return d.index.Add(e.mem)
	}
	return nil
}

// nearDuplicate reports whether two memories share at least
// DreamNearDupRatio of their distinct name+keywords+triggers+content tokens.
func nearDuplicate(a, b *dreamEntry) bool {
	sa := tokenSet(a.mem)
	sb := tokenSet(b.mem)
	if len(sa) == 0 || len(sb) == 0 {
		return false
	}
	n := 0
	for t := range sa {
		if sb[t] {
			n++
		}
	}
	return float64(n)/float64(min(len(sa), len(sb))) >= DreamNearDupRatio
}

// tokenSet builds the distinct searchable tokens of one memory.
func tokenSet(m store.Memory) map[string]bool {
	set := make(map[string]bool)
	for _, t := range tokens(strings.ToLower(strings.Join([]string{
		m.Name,
		strings.Join(m.Keywords, " "),
		strings.Join(m.Triggers, " "),
		m.Content,
	}, "\n"))) {
		set[t] = true
	}
	return set
}

// newerOf returns the entry to keep on a merge: the later CreatedAt (the
// file mtime), with the lexicographically greater (time-ordered) id breaking
// ties.
func newerOf(a, b *dreamEntry) *dreamEntry {
	if a.mem.CreatedAt.After(b.mem.CreatedAt) {
		return a
	}
	if b.mem.CreatedAt.After(a.mem.CreatedAt) {
		return b
	}
	if a.mem.ID > b.mem.ID {
		return a
	}
	return b
}

// firstProjectPath returns the first explicit project path in body (the
// /volN/... and home Project/... layouts of architecture 4.3), or "" when
// the body names none. Plain config paths such as /etc/app/x.yaml are not
// project paths and are ignored.
func firstProjectPath(body string) string {
	for _, m := range pathRe.FindAllString(body, -1) {
		p := strings.TrimRight(m, ".,;:!?\"'`")
		if isProjectPath(p) {
			return p
		}
	}
	return ""
}

// isProjectPath reports whether an extracted path is a project location:
// under a /vol<n>/ volume, under a Project directory, or under ~/Project.
func isProjectPath(p string) bool {
	if volPathRe.MatchString(p) || projectSegRe.MatchString(p) {
		return true
	}
	return strings.HasPrefix(p, "~/") && strings.Contains(strings.ToLower(p), "/project/")
}

// noteMarker is the body marker a note change installs ("" clears none).
func noteMarker(after string) string {
	if after == "" {
		return ""
	}
	return "[dream: " + after + "]"
}

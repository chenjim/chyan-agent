package memory

import (
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/store"
)

// TestFTS5Probe documents the FTS5 measurement: modernc.org/sqlite ships FTS5
// (virtual tables + bm25), but its unicode61 tokenizer does not
// segment CJK runs, so the index must tokenize at the application layer.
func TestFTS5Probe(t *testing.T) {
	if !FTS5Available() {
		t.Skip("modernc.org/sqlite built without FTS5; fallback backend covered elsewhere")
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(body)`); err != nil {
		t.Fatalf("fts5 virtual table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t(body) VALUES (?)`, "数据库检索系统 design"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t WHERE t MATCH ?`, "检索").Scan(&n); err != nil {
		t.Fatalf("bm25/match: %v", err)
	}
	if n != 0 {
		t.Fatalf("unicode61 unexpectedly segmented CJK, n=%d", n)
	}
	var rank float64
	if err := db.QueryRow(`SELECT bm25(t) FROM t WHERE t MATCH ?`, "design").Scan(&rank); err != nil {
		t.Fatalf("bm25: %v", err)
	}
	t.Logf("T2: FTS5 available, raw CJK match=%d (needs app-layer tokenization), bm25(design)=%g", n, rank)
}

// runBoth executes fn once per backend (FTS5 when available, pure-Go
// fallback always), so both code paths are exercised on every run.
func runBoth(t *testing.T, fn func(t *testing.T, x *Index)) {
	t.Helper()
	t.Run("fallback", func(t *testing.T) {
		x := mustOpen(t, t.TempDir()+"/idx.db", true)
		fn(t, x)
	})
	if FTS5Available() {
		t.Run("fts5", func(t *testing.T) {
			x := mustOpen(t, t.TempDir()+"/idx.db", false)
			fn(t, x)
		})
	}
}

func mustOpen(t *testing.T, path string, forcePostings bool) *Index {
	t.Helper()
	x, err := openIndex(path, forcePostings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.Close() })
	return x
}

// writeMemoryFile writes one markdown truth-source file under root.
func writeMemoryFile(t *testing.T, root, kind, file, content string) string {
	t.Helper()
	dir := filepath.Join(root, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedDoc upserts raw docs with caller-controlled CreatedAt (ranking tests
// need precise timestamps; Add uses now).
func seedDoc(t *testing.T, x *Index, docs ...indexDoc) {
	t.Helper()
	if err := x.be.upsert(docs); err != nil {
		t.Fatal(err)
	}
}

func hitIDs(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func TestIndexSearchCJKBigram(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		root := t.TempDir()
		writeMemoryFile(t, root, "fact", "m1cjk00001-db.md", `---
name: 数据库检索系统笔记
keywords: [检索, database]
status: active
id: m1cjk00001
---
数据库检索系统的实现笔记。
`)
		writeMemoryFile(t, root, "fact", "m2cjk00002-weather.md", `---
name: 天气记录
keywords: [weather]
status: active
id: m2cjk00002
---
今天天气不错。
`)
		if err := x.Rebuild(root); err != nil {
			t.Fatal(err)
		}
		// Query "数据库检索" must hit the doc containing
		// "数据库检索系统" through overlapping CJK bigrams.
		hits, err := x.Search("数据库检索", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ID != "m1cjk00001" {
			t.Fatalf("hits = %+v", hits)
		}
		if !strings.Contains(hits[0].Summary, "数据库检索系统") {
			t.Fatalf("summary = %q", hits[0].Summary)
		}
		// A lone CJK rune matches via the unigram fallback.
		hits, err = x.Search("数", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ID != "m1cjk00001" {
			t.Fatalf("unigram hits = %+v", hits)
		}
		// English words are AND-ed and case-insensitive.
		if hits, err = x.Search("DATABASE missing", 10); err != nil || len(hits) != 0 {
			t.Fatalf("hits=%+v err=%v", hits, err)
		}
	})
}

// TestIndexSearchRequiresAllTerms: multi-word queries are AND on both
// backends, so a doc sharing one common word must not be recalled.
func TestIndexSearchRequiresAllTerms(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		root := t.TempDir()
		writeMemoryFile(t, root, "fact", "m1and00001-a.md", `---
name: alpha
keywords: [one]
id: m1and00001
---
alpha beta both words
`)
		writeMemoryFile(t, root, "fact", "m2and00002-b.md", `---
name: beta
keywords: [two]
id: m2and00002
---
beta only here
`)
		if err := x.Rebuild(root); err != nil {
			t.Fatal(err)
		}
		hits, err := x.Search("alpha beta", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ID != "m1and00001" {
			t.Fatalf("AND hits = %v", hitIDs(hits))
		}
	})
}

func TestIndexRebuildIdempotent(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		root := t.TempDir()
		writeMemoryFile(t, root, "fact", "m1rep00001-a.md", `---
name: alpha
keywords: [one]
id: m1rep00001
---
first body
`)
		writeMemoryFile(t, root, "kb", "m2rep00002-b.md", `---
name: beta
keywords: [two]
id: m2rep00002
---
second body
`)
		for i := 0; i < 2; i++ {
			if err := x.Rebuild(root); err != nil {
				t.Fatalf("rebuild %d: %v", i, err)
			}
			hits, err := x.Search("alpha", 10)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(hitIDs(hits), ","); got != "m1rep00001" {
				t.Fatalf("rebuild %d: hits = %s", i, got)
			}
			all, err := x.Search("body", 10)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(hitIDs(all), ","); !strings.Contains(got, "m1rep00001") || !strings.Contains(got, "m2rep00002") {
				t.Fatalf("rebuild %d: all = %s", i, got)
			}
		}
		// Rebuilding an empty root clears this root only.
		empty := t.TempDir()
		if err := x.Rebuild(empty); err != nil {
			t.Fatal(err)
		}
		if hits, err := x.Search("body", 10); err != nil || len(hits) != 2 {
			t.Fatalf("hits=%+v err=%v", hits, err)
		}
		// A root that does not exist is a no-op.
		if err := x.Rebuild(filepath.Join(empty, "nope")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIndexRebuildSkipsBrokenFiles(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		root := t.TempDir()
		writeMemoryFile(t, root, "fact", "m1good00001-good.md", `---
name: good
keywords: [topic]
id: m1good00001
---
topic body
`)
		dir := filepath.Join(root, "fact")
		for name, content := range map[string]string{
			"bad-yaml.md":       "---\n: : :\n---\ntopic body",
			"unterminated.md":   "---\nname: x\nno closing",
			"no-frontmatter.md": "topic without frontmatter",
			"invalid-id.md":     "---\nid: ..\n---\ntopic body",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := x.Rebuild(root); err != nil {
			t.Fatalf("broken files must not fail the rebuild: %v", err)
		}
		hits, err := x.Search("topic", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ID != "m1good00001" {
			t.Fatalf("hits = %+v", hits)
		}
	})
}

func TestIndexSearchRanking(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		now := time.Now().UTC()
		day := 24 * time.Hour
		mk := func(id, domain string, pinned bool, age time.Duration) indexDoc {
			return indexDoc{
				ID: id, Path: "/mem/fact/" + id + "-alpha.md", Domain: domain,
				Kind: "fact", Name: "alpha", Summary: "alpha",
				Status: StatusActive, Pinned: pinned,
				CreatedAt: now.Add(-age),
			}
		}
		seedDoc(t, x,
			mk("mfresh", DomainGlobal, false, time.Hour),
			mk("mpinned", DomainGlobal, true, 300*day),
			mk("mproject", DomainProject, false, 300*day),
			mk("mold", DomainGlobal, false, 300*day),
		)
		hits, err := x.Search("alpha", 10)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(hitIDs(hits), ",")
		// fresh > pinned(old) > project(old) > plain(old):
		// relevance equal, so recency/pinned/domain factors decide.
		if got != "mfresh,mpinned,mproject,mold" {
			t.Fatalf("order = %s", got)
		}
		// pinned factor is exactly 2x, all else equal.
		byID := map[string]Hit{}
		for _, h := range hits {
			byID[h.ID] = h
		}
		if r := byID["mpinned"].Score / byID["mold"].Score; math.Abs(r-pinnedWeight) > 0.01 {
			t.Fatalf("pinned ratio = %v, want %v", r, pinnedWeight)
		}
		if r := byID["mproject"].Score / byID["mold"].Score; math.Abs(r-projectWeight) > 0.01 {
			t.Fatalf("project ratio = %v, want %v", r, projectWeight)
		}
		// Limit truncates after ranking.
		if hits, err := x.Search("alpha", 2); err != nil || hitIDs(hits)[0] != "mfresh" || len(hits) != 2 {
			t.Fatalf("hits=%+v err=%v", hits, err)
		}
	})
}

func TestIndexRankingFactors(t *testing.T) {
	now := time.Now().UTC()
	day := 24 * time.Hour
	// Recency halves every recencyWindowDays.
	if r := recencyWeight(now, now); math.Abs(r-1) > 1e-9 {
		t.Fatalf("fresh recency = %v", r)
	}
	if r := recencyWeight(now.Add(-recencyWindowDays*24*time.Hour), now); math.Abs(r-0.5) > 1e-9 {
		t.Fatalf("half-life recency = %v", r)
	}
	if r := recencyWeight(time.Time{}, now); r != 1 {
		t.Fatalf("zero time recency = %v", r)
	}
	if r := recencyWeight(now.Add(time.Hour), now); r != 1 {
		t.Fatalf("future time must clamp, got %v", r)
	}
	// Domain priority: project > project-local > global.
	if !(domainWeight(DomainProject) > domainWeight(DomainProjectLocal) && domainWeight(DomainProjectLocal) > domainWeight(DomainGlobal)) {
		t.Fatalf("domain weights wrong: %v %v %v", domainWeight(DomainProject), domainWeight(DomainProjectLocal), domainWeight(DomainGlobal))
	}
	// Pinned multiplies the score.
	d := indexDoc{Domain: DomainGlobal, CreatedAt: now.Add(-day)}
	base := finalScore(1, d, now)
	d.Pinned = true
	if r := finalScore(1, d, now) / base; math.Abs(r-pinnedWeight) > 1e-9 {
		t.Fatalf("pinned factor = %v", r)
	}
}

func TestIndexAddRemove(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		m := store.Memory{
			ID: "madd000001", Path: "/mem/fact/madd000001-inc.md",
			Domain: DomainGlobal, Kind: "fact", Name: "incremental",
			Keywords: []string{"incword"}, Content: "first wording",
		}
		if err := x.Add(m); err != nil {
			t.Fatal(err)
		}
		hits, err := x.Search("incword", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ID != "madd000001" || hits[0].CreatedAt.IsZero() {
			t.Fatalf("hits = %+v", hits)
		}
		if hits[0].Domain != DomainGlobal {
			t.Fatalf("default domain = %q", hits[0].Domain)
		}
		// Re-Add replaces the row: old vocabulary disappears.
		m.Keywords = []string{"newword"}
		if err := x.Add(m); err != nil {
			t.Fatal(err)
		}
		if hits, err := x.Search("incword", 10); err != nil || len(hits) != 0 {
			t.Fatalf("stale hits=%+v err=%v", hits, err)
		}
		if hits, err := x.Search("newword", 10); err != nil || len(hits) != 1 {
			t.Fatalf("fresh hits=%+v err=%v", hits, err)
		}
		// Remove drops the row; removing again is a no-op.
		if err := x.Remove("madd000001"); err != nil {
			t.Fatal(err)
		}
		if hits, err := x.Search("newword", 10); err != nil || len(hits) != 0 {
			t.Fatalf("hits=%+v err=%v", hits, err)
		}
		if err := x.Remove("madd000001"); err != nil {
			t.Fatal(err)
		}
		// Missing ids are fine.
		if err := x.Add(store.Memory{}); err == nil {
			t.Fatal("Add without ID must fail")
		}
		if err := x.Remove(""); err == nil {
			t.Fatal("Remove without ID must fail")
		}
		// Empty query: no hits, no error.
		if hits, err := x.Search("   ", 10); err != nil || len(hits) != 0 {
			t.Fatalf("hits=%+v err=%v", hits, err)
		}
	})
}

func TestIndexRebuildMergesRootsSameID(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		rootA, rootB := t.TempDir(), t.TempDir()
		for _, r := range []string{rootA, rootB} {
			writeMemoryFile(t, r, "fact", "mshared00001-x.md", `---
name: shared entry
keywords: [shared]
id: mshared00001
---
body
`)
		}
		if err := x.Rebuild(rootA); err != nil {
			t.Fatal(err)
		}
		if err := x.Rebuild(rootB); err != nil {
			t.Fatal(err)
		}
		// Same id in two domains: both rows survive (ids are only
		// unique per domain).
		hits, err := x.Search("shared", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 {
			t.Fatalf("hits = %+v", hits)
		}
		// Remove drops every row with that id across domains.
		if err := x.Remove("mshared00001"); err != nil {
			t.Fatal(err)
		}
		if hits, err := x.Search("shared", 10); err != nil || len(hits) != 0 {
			t.Fatalf("hits=%+v err=%v", hits, err)
		}
	})
}

func TestIndexSupersededHidden(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		root := t.TempDir()
		writeMemoryFile(t, root, "fact", "mold0000001-old.md", `---
name: old entry
keywords: [topic]
status: superseded
id: mold0000001
---
old body
`)
		writeMemoryFile(t, root, "fact", "mnew0000001-new.md", `---
name: new entry
keywords: [topic]
status: active
supersedes: mold0000001
id: mnew0000001
---
new body
`)
		if err := x.Rebuild(root); err != nil {
			t.Fatal(err)
		}
		hits, err := x.Search("topic", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].ID != "mnew0000001" {
			t.Fatalf("hits = %+v", hits)
		}
	})
}

func TestIndexSummaryTruncated(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		body := strings.Repeat("长文字段落 ", 200) // way past summaryRunes
		writeMemoryFile(t, t.TempDir(), "fact", "msum0000001-big.md", `---
name: big body
keywords: [big]
id: msum0000001
---
`+body)
		root := t.TempDir()
		writeMemoryFile(t, root, "fact", "msum0000001-big.md", `---
name: big body
keywords: [big]
id: msum0000001
---
`+body)
		if err := x.Rebuild(root); err != nil {
			t.Fatal(err)
		}
		hits, err := x.Search("big", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 {
			t.Fatalf("hits = %+v", hits)
		}
		if r := []rune(hits[0].Summary); len(r) != summaryRunes {
			t.Fatalf("summary runes = %d, want %d", len(r), summaryRunes)
		}
		if hits[0].Path == "" {
			t.Fatal("hit must carry the truth-source path")
		}
	})
}

func TestIndexCacheReopen(t *testing.T) {
	runBoth(t, func(t *testing.T, x *Index) {
		root := t.TempDir()
		writeMemoryFile(t, root, "fact", "mpers000001-a.md", `---
name: persistent
keywords: [persist]
id: mpers000001
---
body
`)
		if err := x.Rebuild(root); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "reopen.db")
		// Reopen the same cache path without Rebuild: the state must
		// survive on disk.
		y := mustOpen(t, path, sameBackend(x))
		if err := y.Rebuild(root); err != nil {
			t.Fatal(err)
		}
		if err := y.Close(); err != nil {
			t.Fatal(err)
		}
		z := mustOpen(t, path, sameBackend(x))
		hits, err := z.Search("persist", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 {
			t.Fatalf("hits = %+v", hits)
		}
	})
}

// sameBackend reports the forcePostings flag reproducing x's backend kind.
func sameBackend(x *Index) bool {
	_, ok := x.be.(*postingsBackend)
	return ok
}

func TestIndexForeignCacheAndCorruption(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	writeMemoryFile(t, root, "fact", "mswap0000001-a.md", `---
name: swap
keywords: [swap]
id: mswap0000001
---
body
`)
	// Fallback cache on disk, then opened as FTS5: file is discarded and
	// rebuilt (never a SQLite-over-JSON crash).
	path := filepath.Join(dir, "cache.db")
	x := mustOpen(t, path, true)
	if err := x.Rebuild(root); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	y := mustOpen(t, path, false)
	if err := y.Rebuild(root); err != nil {
		t.Fatal(err)
	}
	if hits, err := y.Search("swap", 10); err != nil || len(hits) != 1 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if err := y.Close(); err != nil {
		t.Fatal(err)
	}
	// And the other direction: SQLite cache opened by the fallback.
	z := mustOpen(t, path, true)
	if err := z.Rebuild(root); err != nil {
		t.Fatal(err)
	}
	if hits, err := z.Search("swap", 10); err != nil || len(hits) != 1 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	// Corrupted JSON cache: silently recreated empty.
	if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := mustOpen(t, path, true)
	if hits, err := w.Search("swap", 10); err != nil || len(hits) != 0 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if err := w.Rebuild(root); err != nil {
		t.Fatal(err)
	}
	if hits, err := w.Search("swap", 10); err != nil || len(hits) != 1 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
}

func TestIndexStaleSchemaReset(t *testing.T) {
	if !FTS5Available() {
		t.Skip("schema reset path is FTS5-specific")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	root := t.TempDir()
	writeMemoryFile(t, root, "fact", "mstale000001-a.md", `---
name: stale
keywords: [stale]
id: mstale000001
---
body
`)
	x := mustOpen(t, path, false)
	if err := x.Rebuild(root); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	// Tamper with the schema version: Open must reset the cache, not err.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE idx_meta SET v = '0' WHERE k = 'schema'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	y := mustOpen(t, path, false)
	if hits, err := y.Search("stale", 10); err != nil || len(hits) != 0 {
		t.Fatalf("cache must be reset, hits=%+v err=%v", hits, err)
	}
	if err := y.Rebuild(root); err != nil {
		t.Fatal(err)
	}
	if hits, err := y.Search("stale", 10); err != nil || len(hits) != 1 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
}

func TestIndexOpenErrors(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("empty path must fail")
	}
	// Closing the FTS5 backend makes later calls fail loudly.
	if FTS5Available() {
		x := mustOpen(t, t.TempDir()+"/idx.db", false)
		if err := x.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := x.Search("x", 1); err == nil {
			t.Fatal("search on closed index must fail")
		}
	}
}

func TestDomainOfRoot(t *testing.T) {
	home := t.TempDir()
	old := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	defer func() { userHomeDir = old }()

	cases := []struct {
		root string
		want string
	}{
		{filepath.Join(home, ".chyan", "memory"), DomainGlobal},
		{filepath.Join(home, ".agents", "memory"), DomainGlobal}, // legacy layout
		{filepath.Join("/repo", ".agents", "memory"), DomainProject},
		{filepath.Join("/repo", ".agents", "local", "memory"), DomainProjectLocal},
		{filepath.Join("/elsewhere", "memroot"), DomainGlobal},
	}
	for _, c := range cases {
		if got := domainOfRoot(c.root); got != c.want {
			t.Fatalf("domainOfRoot(%q) = %q, want %q", c.root, got, c.want)
		}
	}
}

func TestIndexTokensIncludeCJKUnigram(t *testing.T) {
	toks := indexTokens("数据库 ab")
	joined := strings.Join(toks, ",")
	for _, want := range []string{"数据", "据库", "数", "据", "库", "ab"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("indexTokens = %v, missing %q", toks, want)
		}
	}
}

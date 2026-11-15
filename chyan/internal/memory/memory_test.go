package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"chyan.tech/chyan/internal/store"
)

// newTestStore returns a FileStore rooted in a temp dir, with the project
// domain under <tmp>/repo/.agents/memory.
func newTestStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	root := t.TempDir()
	return New(filepath.Join(root, "global")), filepath.Join(root, "repo")
}

func mustAdd(t *testing.T, s *FileStore, m store.Memory) store.Memory {
	t.Helper()
	got, err := s.AddMemory(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAddMemoryGeneratesIDAndFile(t *testing.T) {
	s, _ := newTestStore(t)
	got := mustAdd(t, s, store.Memory{
		Name:     "用户偏好Python",
		Kind:     "fact",
		Type:     "semantic",
		Keywords: []string{"python", "偏好"},
		Content:  "偏好 Python 技术栈",
		Source:   "explicit",
	})
	if !idPattern.MatchString(got.ID) {
		t.Fatalf("id %q malformed", got.ID)
	}
	if got.Domain != DomainGlobal || got.Status != StatusActive {
		t.Fatalf("domain/status = %q/%q", got.Domain, got.Status)
	}
	want := filepath.Join(s.global, "fact", got.ID+"-mem.md")
	if got.Path != want {
		t.Fatalf("path = %q, want %q", got.Path, want)
	}
	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, frag := range []string{"---\n", "name: 用户偏好Python", "id: " + got.ID, "status: active", "偏好 Python 技术栈"} {
		if !strings.Contains(text, frag) {
			t.Fatalf("file missing %q:\n%s", frag, text)
		}
	}
}

func TestAddMemorySlugAndExplicitID(t *testing.T) {
	s, repo := newTestStore(t)
	got := mustAdd(t, s, store.Memory{
		Name: "Store large tool outputs in DB",
		Kind: "fact",
		ID:   "m00000000000ab",
	})
	if base := filepath.Base(got.Path); base != "m00000000000ab-store-large-tool-outputs.md" {
		t.Fatalf("file name = %q", base)
	}
	// Same explicit id in the same domain is rejected.
	_, err := s.AddMemory(context.Background(), store.Memory{Name: "x", Kind: "fact", ID: "m00000000000ab"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want already exists", err)
	}
	// Same id in another domain is fine.
	mustAdd(t, s, store.Memory{Name: "x", Kind: "fact", ID: "m00000000000ab", ProjectPath: repo})
}

func TestAddMemoryProjectDomain(t *testing.T) {
	s, repo := newTestStore(t)
	got := mustAdd(t, s, store.Memory{
		Name:        "chyan 契约只读",
		Kind:        "directive",
		ProjectPath: repo,
		Content:     "store.go 只读",
	})
	if got.Domain != DomainProject {
		t.Fatalf("domain = %q", got.Domain)
	}
	want := filepath.Join(repo, ".agents", "memory", "directive", got.ID+"-chyan.md")
	if got.Path != want {
		t.Fatalf("path = %q, want %q", got.Path, want)
	}
}

func TestAddMemoryValidation(t *testing.T) {
	s, repo := newTestStore(t)
	tests := []struct {
		name string
		m    store.Memory
	}{
		{"missing name", store.Memory{Kind: "fact"}},
		{"missing kind", store.Memory{Name: "x"}},
		{"kind traversal", store.Memory{Name: "x", Kind: "../evil"}},
		{"bad domain", store.Memory{Name: "x", Kind: "fact", Domain: "weird"}},
		{"global with project", store.Memory{Name: "x", Kind: "fact", Domain: DomainGlobal, ProjectPath: repo}},
		{"project without path", store.Memory{Name: "x", Kind: "fact", Domain: DomainProject}},
		{"bad status", store.Memory{Name: "x", Kind: "fact", Status: "stale"}},
		{"bad id", store.Memory{Name: "x", Kind: "fact", ID: "bad id!"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.AddMemory(context.Background(), tt.m); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestAddMemoryCanceledContext(t *testing.T) {
	s, _ := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.AddMemory(ctx, store.Memory{Name: "x", Kind: "fact"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSearchMemoryRanksProjectFirst(t *testing.T) {
	s, repo := newTestStore(t)
	mustAdd(t, s, store.Memory{
		Name:        "项目约定",
		Kind:        "directive",
		Keywords:    []string{"约定", "memory"},
		ProjectPath: repo,
	})
	mustAdd(t, s, store.Memory{
		Name:     "全局偏好",
		Kind:     "fact",
		Keywords: []string{"偏好", "memory"},
	})
	hits, err := s.SearchMemories(context.Background(), Query{Text: "memory", ProjectPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("len = %d, want 2", len(hits))
	}
	if hits[0].Domain != DomainProject || hits[1].Domain != DomainGlobal {
		t.Fatalf("order = %q, %q", hits[0].Domain, hits[1].Domain)
	}
}

func TestSearchGlobalOnlyWhenNoProject(t *testing.T) {
	s, repo := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "项目知识", Kind: "kb", Keywords: []string{"seed"}, ProjectPath: repo})
	mustAdd(t, s, store.Memory{Name: "全局知识", Kind: "kb", Keywords: []string{"seed"}})
	hits, err := s.SearchMemories(context.Background(), Query{Text: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Domain != DomainGlobal {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchMultiWordANDAndCaseInsensitive(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "Alpha Beta", Kind: "fact", Content: "both alpha and beta here"})
	mustAdd(t, s, store.Memory{Name: "Alpha only", Kind: "fact", Content: "alpha but not the second word"})
	hits, err := s.SearchMemories(context.Background(), Query{Text: "ALPHA beta"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "Alpha Beta" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchCJKBigram(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "上下文预算", Kind: "model", Content: "压缩阈值按比例触发"})
	mustAdd(t, s, store.Memory{Name: "无关条目", Kind: "model", Content: "别的内容"})
	hits, err := s.SearchMemories(context.Background(), Query{Text: "压缩阈值"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "上下文预算" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchKindFilterAndTopK(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "a", Kind: "fact", Keywords: []string{"topic"}})
	mustAdd(t, s, store.Memory{Name: "b", Kind: "kb", Keywords: []string{"topic"}})
	mustAdd(t, s, store.Memory{Name: "c", Kind: "kb", Keywords: []string{"topic"}})
	hits, err := s.SearchMemories(context.Background(), Query{Text: "topic", Kinds: []string{"kb"}, TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Kind != "kb" {
		t.Fatalf("hits = %+v", hits)
	}
	// Empty kinds searches all.
	all, err := s.SearchMemories(context.Background(), Query{Text: "topic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
}

func TestSearchContentMatch(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "m1", Kind: "fact", Keywords: []string{"k"}, Content: "Truncate tool output at 20KB"})
	mustAdd(t, s, store.Memory{Name: "m2", Kind: "fact", Keywords: []string{"k"}, Content: "nothing related"})
	hits, err := s.SearchMemories(context.Background(), Query{Text: "k", ContentMatch: "tool output"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "m1" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchExcludesSuperseded(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "old", Kind: "fact", Keywords: []string{"topic"}, Status: StatusSuperseded})
	mustAdd(t, s, store.Memory{Name: "new", Kind: "fact", Keywords: []string{"topic"}, Status: StatusActive, Supersedes: "mold"})
	hits, err := s.SearchMemories(context.Background(), Query{Text: "topic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "new" {
		t.Fatalf("hits = %+v", hits)
	}
}

// TestTriggerMatchesSkipsSupersededFact is the FR-13 regression for a user
// updating an old fact: both entries still carry the trigger, but only the
// active replacement may be injected into a turn.
func TestTriggerMatchesSkipsSupersededFact(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, store.Memory{
		ID: "mold", Name: "budget", Kind: "fact", Keywords: []string{"budget"},
		Triggers: []string{"预算"}, Content: "预算 50 万", Status: StatusSuperseded,
	})
	mustAdd(t, s, store.Memory{
		ID: "mnew", Name: "budget", Kind: "fact", Keywords: []string{"budget"},
		Triggers: []string{"预算"}, Content: "预算 30 万", Status: StatusActive, Supersedes: "mold",
	})
	hits, err := s.TriggerMatches(context.Background(), "确认一下预算", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "mnew" || hits[0].Content != "预算 30 万" {
		t.Fatalf("injected hits = %+v", hits)
	}
}

func TestSearchSkipsBrokenFiles(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "good", Kind: "fact", Keywords: []string{"topic"}})
	dir := filepath.Join(s.global, "fact")
	for name, content := range map[string]string{
		"bad-yaml.md":       "---\n: : :\n---\nbody topic",
		"unterminated.md":   "---\nname: x\nno closing",
		"no-frontmatter.md": "topic without frontmatter",
		"invalid-id.md":     "---\nid: ..\n---\ntopic",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.SearchMemories(context.Background(), Query{Text: "topic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "good" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestRecallRanksByOverlap(t *testing.T) {
	s, repo := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "输出截断", Kind: "fact", Keywords: []string{"truncation"}, Triggers: []string{"bash 落库"}, Content: "tool output 按 20KB 截断"})
	mustAdd(t, s, store.Memory{Name: "无关", Kind: "fact", Content: "天气不错"})
	mustAdd(t, s, store.Memory{Name: "项目项", Kind: "kb", Keywords: []string{"truncation", "output"}, ProjectPath: repo})
	ctxText := "we need truncation of tool output before saving to the DB"
	hits, err := s.Recall(context.Background(), ctxText, repo, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(hits), hits)
	}
	if hits[0].Domain != DomainProject {
		t.Fatalf("project memory should rank first, got %q (%+v)", hits[0].Domain, hits)
	}
	if hits[1].Name != "输出截断" {
		t.Fatalf("second = %+v", hits[1])
	}
}

func TestTriggerMatches(t *testing.T) {
	s, repo := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "bash 落库", Kind: "directive",
		Triggers: []string{"bash spill"}, Content: "先落库再截断"})
	mustAdd(t, s, store.Memory{Name: "项目部署", Kind: "directive", ProjectPath: repo,
		Triggers: []string{"deploy 服务"}, Content: "部署前跑 go test"})
	mustAdd(t, s, store.Memory{Name: "无触发", Kind: "fact", Content: "天气不错"})

	ctx := context.Background()
	hits, err := s.TriggerMatches(ctx, "we hit a BASH SPILL during deploy 服务", repo, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
	// Project memories rank before global ones.
	if hits[0].Domain != DomainProject {
		t.Fatalf("project memory should rank first: %+v", hits)
	}
	if len(hits[0].Triggers) == 0 || hits[0].Triggers[0] != "deploy 服务" {
		t.Fatalf("wrong trigger match: %+v", hits[0])
	}

	// No trigger in the text (or empty text) yields nothing.
	if hits, err := s.TriggerMatches(ctx, "hello world", "", 3); err != nil || len(hits) != 0 {
		t.Fatalf("unexpected hits = %+v, %v", hits, err)
	}
	if hits, err := s.TriggerMatches(ctx, "   ", "", 3); err != nil || len(hits) != 0 {
		t.Fatalf("empty text hits = %+v, %v", hits, err)
	}
}

func TestRecallEdgeCases(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "a", Kind: "fact", Content: "alpha"})
	// Empty context: no hits, no error.
	if hits, err := s.Recall(context.Background(), "   ", "", 5); err != nil || len(hits) != 0 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	// topK <= 0 returns everything matched.
	if hits, err := s.Recall(context.Background(), "alpha", "", 0); err != nil || len(hits) != 1 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	// No overlap: empty.
	if hits, err := s.Recall(context.Background(), "zzz qqq", "", 5); err != nil || len(hits) != 0 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	// Canceled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Recall(ctx, "alpha", "", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeleteMemory(t *testing.T) {
	s, repo := newTestStore(t)
	got := mustAdd(t, s, store.Memory{Name: "temp", Kind: "fact", ProjectPath: repo})
	if err := s.DeleteMemory(context.Background(), got.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(got.Path); !os.IsNotExist(err) {
		t.Fatal("file still exists")
	}
	if err := s.DeleteMemory(context.Background(), got.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
	if err := s.DeleteMemory(context.Background(), "bad id!"); err == nil {
		t.Fatal("want invalid id error")
	}
}

func TestDeleteMemoryAmbiguous(t *testing.T) {
	s, repo := newTestStore(t)
	g1 := mustAdd(t, s, store.Memory{Name: "x", Kind: "fact", ID: "mshared12345"})
	g2 := mustAdd(t, s, store.Memory{Name: "x", Kind: "fact", ID: "mshared12345", ProjectPath: repo})
	s.Track(repo)
	err := s.DeleteMemory(context.Background(), "mshared12345")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want ambiguous", err)
	}
	// Both files survive the failed delete.
	for _, p := range []string{g1.Path, g2.Path} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeleteMemoryByFrontmatterID(t *testing.T) {
	s, _ := newTestStore(t)
	got := mustAdd(t, s, store.Memory{Name: "renamed", Kind: "fact", ID: "mrename12345"})
	// Rename the file so only the frontmatter id identifies it.
	renamed := filepath.Join(filepath.Dir(got.Path), "something-else.md")
	if err := os.Rename(got.Path, renamed); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMemory(context.Background(), "mrename12345"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(renamed); !os.IsNotExist(err) {
		t.Fatal("file still exists")
	}
}

func TestAddMemoryConcurrentWrites(t *testing.T) {
	s, _ := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = s.AddMemory(context.Background(), store.Memory{
				Name:     "concurrent",
				Kind:     "fact",
				Keywords: []string{"conc"},
			})
		}(i)
	}
	wg.Wait()
	hits, err := s.SearchMemories(context.Background(), Query{Text: "conc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 20 {
		t.Fatalf("len = %d, want 20", len(hits))
	}
}

// TestWriteFileAtomic: replacement is complete in one rename and no temp
// file survives; a failed target directory reports an error instead of
// leaving a partial file behind.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.md")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("content = %q, %v", data, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, %v", fi, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	if err := writeFileAtomic(filepath.Join(dir, "missing", "x.md"), []byte("x"), 0o644); err == nil {
		t.Fatal("missing directory must fail")
	}
}

// TestSearchMemoriesUsesIndex: once a cache is attached, writes keep it in
// sync and lookups honor the same contract (all tokens, project > global,
// kind filter, body re-read from the truth source).
func TestSearchMemoriesUsesIndex(t *testing.T) {
	s, repo := newTestStore(t)
	x := mustOpen(t, filepath.Join(t.TempDir(), "idx.db"), true)
	defer x.Close()
	s.SetIndex(x)

	global := mustAdd(t, s, store.Memory{
		Name: "全局偏好", Kind: "fact", Keywords: []string{"memory"},
		Content: "全局正文内容",
	})
	project := mustAdd(t, s, store.Memory{
		Name: "项目约定", Kind: "directive", Keywords: []string{"memory"}, ProjectPath: repo,
	})
	hits, err := s.SearchMemories(context.Background(), Query{Text: "memory", ProjectPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Domain != DomainProject {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[1].Content != "全局正文内容" {
		t.Fatalf("body not re-read from file: %q", hits[1].Content)
	}
	// Kind filter applies on the index path too.
	only := Query{Text: "memory", Kinds: []string{"directive"}, ProjectPath: repo}
	hits, err = s.SearchMemories(context.Background(), only)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != project.ID {
		t.Fatalf("kind-filtered hits = %+v", hits)
	}
	// Delete keeps the cache in sync.
	if err := s.DeleteMemory(context.Background(), global.ID); err != nil {
		t.Fatal(err)
	}
	hits, err = s.SearchMemories(context.Background(), Query{Text: "memory", ProjectPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != project.ID {
		t.Fatalf("after delete hits = %+v", hits)
	}
}

// TestIndexPathServesAfterFileLoss proves lookups actually read the cache:
// the markdown file is removed behind the store's back, yet the index still
// answers (falling back to the summarized body).
func TestIndexPathServesAfterFileLoss(t *testing.T) {
	s, _ := newTestStore(t)
	x := mustOpen(t, filepath.Join(t.TempDir(), "idx.db"), true)
	defer x.Close()
	s.SetIndex(x)
	m := mustAdd(t, s, store.Memory{
		Name: "索引命中", Kind: "fact", Keywords: []string{"cacheonly"}, Content: "短正文",
	})
	// First lookup rebuilds the root from disk, priming the cache.
	if hits, err := s.SearchMemories(context.Background(), Query{Text: "cacheonly"}); err != nil || len(hits) != 1 {
		t.Fatalf("prime hits=%+v err=%v", hits, err)
	}
	if err := os.Remove(m.Path); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchMemories(context.Background(), Query{Text: "cacheonly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != m.ID {
		t.Fatalf("index hit lost: %+v", hits)
	}
	if hits[0].Content != "短正文" {
		t.Fatalf("summary fallback = %q", hits[0].Content)
	}
	// A body-only match cannot be answered by the summary, so the scan path
	// runs; with the file gone it finds nothing.
	hits, err = s.SearchMemories(context.Background(), Query{Text: "cacheonly", ContentMatch: "短正文"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("content-match must scan files: %+v", hits)
	}
}

// TestIndexRebuildsExistingFiles: memories written before the cache was
// attached become visible on the first lookup (lazy one-time rebuild).
func TestIndexRebuildsExistingFiles(t *testing.T) {
	s, repo := newTestStore(t)
	mustAdd(t, s, store.Memory{Name: "旧全局", Kind: "fact", Keywords: []string{"legacy"}})
	mustAdd(t, s, store.Memory{Name: "旧项目", Kind: "fact", Keywords: []string{"legacy"}, ProjectPath: repo})
	x := mustOpen(t, filepath.Join(t.TempDir(), "idx.db"), true)
	defer x.Close()
	s.SetIndex(x)
	hits, err := s.Recall(context.Background(), "legacy", repo, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Domain != DomainProject {
		t.Fatalf("hits = %+v", hits)
	}
}

// TestIndexRecallFallsBackToOverlap: a long conversation excerpt misses the
// all-token cache and falls back to the overlap scan.
func TestIndexRecallFallsBackToOverlap(t *testing.T) {
	s, _ := newTestStore(t)
	x := mustOpen(t, filepath.Join(t.TempDir(), "idx.db"), true)
	defer x.Close()
	s.SetIndex(x)
	mustAdd(t, s, store.Memory{Name: "输出截断", Kind: "fact", Keywords: []string{"truncation"}})
	ctxText := "we need truncation of tool output before saving to the db"
	hits, err := s.Recall(context.Background(), ctxText, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "输出截断" {
		t.Fatalf("fallback hits = %+v", hits)
	}
}

// failBackend is an index backend whose every operation fails, used to
// exercise the degraded-cache path.
type failBackend struct {
	mu      sync.Mutex
	rebuild int
}

func (f *failBackend) upsert([]indexDoc) error { return errors.New("boom") }
func (f *failBackend) remove([]string) error   { return errors.New("boom") }
func (f *failBackend) match([]string) ([]matchResult, error) {
	return nil, errors.New("boom")
}
func (f *failBackend) close() error { return nil }
func (f *failBackend) replaceUnder(string, []indexDoc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuild++
	return errors.New("boom")
}

func (f *failBackend) rebuilds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rebuild
}

// TestSearchMemoriesFallsBackOnDegradedIndex: when the cache cannot be primed,
// lookups must scan the truth source on every call (never serve the partial
// cache, never retry the failed rebuild).
func TestSearchMemoriesFallsBackOnDegradedIndex(t *testing.T) {
	s, _ := newTestStore(t)
	fb := &failBackend{}
	s.SetIndex(&Index{be: fb})
	mustAdd(t, s, store.Memory{Name: "降级命中", Kind: "fact", Keywords: []string{"degraded"}})
	for i := 0; i < 2; i++ {
		hits, err := s.SearchMemories(context.Background(), Query{Text: "degraded"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Name != "降级命中" {
			t.Fatalf("call %d hits = %+v", i, hits)
		}
	}
	if got := fb.rebuilds(); got != 1 {
		t.Fatalf("rebuild attempts = %d, want 1", got)
	}
	hits, err := s.Recall(context.Background(), "degraded", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "降级命中" {
		t.Fatalf("recall hits = %+v", hits)
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Store large tool outputs in DB", "store-large-tool-outputs"},
		{"chyan 契约只读", "chyan"},
		{"用户偏好Python", ""}, // mixed-script word: no ASCII abbreviation, AddMemory substitutes "mem"
		{"A  B__C", "a-b-c"},
		{"中文", ""},
		{"abcdefghijklmnopqrstuvwxyz0123456789", "abcdefghijklmnopqrstuvwx"},
	}
	for _, tt := range tests {
		if got := slugify(tt.in); got != tt.want {
			t.Fatalf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTokenHelpers(t *testing.T) {
	toks := tokens("Hello 世界World 42")
	want := []string{"hello", "世界", "world", "42"}
	if strings.Join(toks, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens = %v, want %v", toks, want)
	}
	// Sliding bigrams per CJK run (architecture 4.3): 2 runes -> 1 token,
	// 3 runes -> 2 overlapping bigrams, a lone rune stays a unigram.
	if got := strings.Join(tokens("世界"), ","); got != "世界" {
		t.Fatalf("tokens(世界) = %v", tokens("世界"))
	}
	if got := strings.Join(tokens("上下文"), ","); got != "上下,下文" {
		t.Fatalf("tokens(上下文) = %v", tokens("上下文"))
	}
	if got := strings.Join(tokens("世"), ","); got != "世" {
		t.Fatalf("tokens(世) = %v", tokens("世"))
	}
	h := hayOf(store.Memory{Name: "x", Content: "hello world"})
	if !h.match(tokens("HELLO")) || h.match(tokens("hello missing")) {
		t.Fatal("match logic wrong")
	}
	if h.score(tokens("hello world hello")) != 3 {
		t.Fatalf("score = %d", h.score(tokens("hello world hello")))
	}
}

func TestRenderFrontmatterRoundTrip(t *testing.T) {
	m := &meta{
		Name:       "n",
		Keywords:   []string{"k1", "k2"},
		Triggers:   []string{"t1"},
		Type:       "procedural",
		Status:     "active",
		Supersedes: "mold123",
		Source:     "dream",
		Pinned:     true,
		ID:         "mabc123",
	}
	out := renderFrontmatter(m)
	if !strings.HasPrefix(out, "---\n") || !strings.HasSuffix(out, "---\n") {
		t.Fatalf("bad delimiters: %q", out)
	}
	if !strings.Contains(out, "pinned: true") || !strings.Contains(out, "- k1") {
		t.Fatalf("missing fields: %q", out)
	}
}

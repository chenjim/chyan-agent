package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/frontmatter"
	"chyan.tech/chyan/internal/store"
	"gopkg.in/yaml.v3"
)

// file memory fixtures are written directly under root/<kind> so tests can
// pin ids and mtimes (AddMemory generates both).

// writeMem writes one memory file and stamps its mtime.
func writeMem(t *testing.T, root string, m store.Memory, content string, mtime time.Time) string {
	t.Helper()
	status := m.Status
	if status == "" {
		status = StatusActive // AddMemory default
	}
	fm := renderFrontmatter(&meta{
		Name: m.Name, Keywords: m.Keywords, Type: m.Type, Status: status,
		Supersedes: m.Supersedes, ID: m.ID,
	})
	path := filepath.Join(root, m.Kind, m.ID+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fm+content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadMem re-parses one memory file (id derived from the file name).
func loadMem(t *testing.T, path string) store.Memory {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fm, body, err := frontmatter.Split(data)
	if err != nil {
		t.Fatal(err)
	}
	var mm meta
	if err := yaml.Unmarshal(fm, &mm); err != nil {
		t.Fatal(err)
	}
	return store.Memory{
		ID: mm.ID, Name: mm.Name, Content: body,
		Status: mm.Status, Supersedes: mm.Supersedes,
	}
}

// dreamer returns a Dreamer over a fresh root with a fixed clock.
func dreamer(t *testing.T, now time.Time) (*Dreamer, string) {
	t.Helper()
	root := t.TempDir()
	return NewDreamer(DreamConfig{Root: root, Now: func() time.Time { return now }}), root
}

func TestDreamNearDuplicatePair(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	d, root := dreamer(t, now)
	// Chinese near-duplicates: a small suffix differs only.
	writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局。", now.Add(-2*time.Hour))
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局！", now.Add(-1*time.Hour))

	rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Examined != 2 || rep.Merged != 1 || rep.Superseded != 1 || rep.Orphaned != 0 {
		t.Fatalf("report counters = %+v", rep)
	}
	old, newer := loadMem(t, filepath.Join(root, "fact", "ma.md")), loadMem(t, filepath.Join(root, "fact", "mb.md"))
	if old.Status != StatusSuperseded {
		t.Fatalf("older status = %q", old.Status)
	}
	if newer.Status != StatusActive || newer.Supersedes != "ma" {
		t.Fatalf("newer = %+v", newer)
	}
}

func TestDreamKeepsNewerAndPassesChain(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	d, root := dreamer(t, now)
	body := "工具大输出必须截断后落库。"
	writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "大输出落库"}, body, now.Add(-4*time.Hour))
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "大输出落库"}, body, now.Add(-3*time.Hour))
	writeMem(t, root, store.Memory{ID: "mc", Kind: "fact", Name: "大输出落库"}, body, now.Add(-1*time.Hour))

	// Cycle 1 collapses one pair (each entry joins at most one merge per
	// cycle); cycle 2 extends the chain: A<-B<-C.
	rep1, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep1.Merged != 1 || rep1.Superseded != 1 {
		t.Fatalf("cycle 1 report = %+v", rep1)
	}
	rep2, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Merged != 1 || rep2.Superseded != 1 {
		t.Fatalf("cycle 2 report = %+v", rep2)
	}
	a := loadMem(t, filepath.Join(root, "fact", "ma.md"))
	b := loadMem(t, filepath.Join(root, "fact", "mb.md"))
	c := loadMem(t, filepath.Join(root, "fact", "mc.md"))
	if a.Status != StatusSuperseded || a.Supersedes != "" {
		t.Fatalf("A = %+v, want superseded root", a)
	}
	if b.Status != StatusSuperseded || b.Supersedes != "ma" {
		t.Fatalf("B = %+v, want superseded, supersedes ma", b)
	}
	if c.Status != StatusActive || c.Supersedes != "mb" {
		t.Fatalf("C = %+v, want active, supersedes mb (chain A<-B<-C)", c)
	}
}

func TestDreamOlderSupersededByNewer(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	d, root := dreamer(t, now)
	body := "配置文件统一放 config.toml。"
	// Write the newer entry first: scan order must not decide the winner.
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "配置文件位置"}, body, now.Add(-30*time.Minute))
	writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "配置文件位置"}, body, now.Add(-2*time.Hour))

	rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Superseded != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if m := loadMem(t, filepath.Join(root, "fact", "ma.md")); m.Status != StatusSuperseded {
		t.Fatalf("older ma status = %q", m.Status)
	}
	if m := loadMem(t, filepath.Join(root, "fact", "mb.md")); m.Status != StatusActive || m.Supersedes != "ma" {
		t.Fatalf("newer mb = %+v", m)
	}
}

func TestDreamProjectAttributionNote(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	d, root := dreamer(t, now)
	pa := writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "仓库路径"}, "工作目录 /srv/project/demo 需要单独配置。", now.Add(-time.Hour))
	pb := writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "Home项目"}, "参考 ~/Project/demo 的构建脚本。", now.Add(-time.Hour))
	pc := writeMem(t, root, store.Memory{ID: "mc", Kind: "fact", Name: "配置路径"}, "配置在 /etc/app/config.yaml，与项目无关。", now.Add(-time.Hour))

	rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Notes) != 2 || len(rep.Changes) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "建议迁移到项目域") ||
		!strings.Contains(strings.Join(rep.Notes, "\n"), "/srv/project/demo") ||
		!strings.Contains(strings.Join(rep.Notes, "\n"), "~/Project/demo") {
		t.Fatalf("notes = %v", rep.Notes)
	}
	// Safety boundary: files stay in place, only body markers are added.
	for _, p := range []string{pa, pb} {
		if !strings.Contains(loadMem(t, p).Content, "[dream: 建议迁移到项目域:") {
			t.Fatalf("marker missing in %s", p)
		}
		if !strings.HasPrefix(p, root) {
			t.Fatalf("file moved: %s", p)
		}
	}
	if strings.Contains(loadMem(t, pc).Content, "[dream:") {
		t.Fatalf("non-project path flagged: %+v", loadMem(t, pc))
	}
}

func TestDreamOrphanCount(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	d, root := dreamer(t, now)
	writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "旧事实", Status: StatusSuperseded}, "内容甲。", now.Add(-31*24*time.Hour))
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "新事实", Status: StatusSuperseded}, "内容乙。", now.Add(-24*time.Hour))

	rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphaned != 1 || rep.Examined != 0 || rep.Superseded != 0 {
		t.Fatalf("report = %+v", rep)
	}
	// Counted only: the stale entry is not deleted.
	if _, err := os.Stat(filepath.Join(root, "fact", "ma.md")); err != nil {
		t.Fatalf("orphan deleted: %v", err)
	}
}

func TestDreamRollbackRestores(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	d, root := dreamer(t, now)
	writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局。", now.Add(-2*time.Hour))
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局！", now.Add(-time.Hour))
	pnote := writeMem(t, root, store.Memory{ID: "mp", Kind: "fact", Name: "仓库路径"}, "工作目录 /srv/project/demo 需要单独配置。", now.Add(-time.Hour))

	rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Changes) != 3 {
		t.Fatalf("changes = %+v", rep.Changes)
	}
	if err := d.Rollback(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	if m := loadMem(t, filepath.Join(root, "fact", "ma.md")); m.Status != StatusActive {
		t.Fatalf("ma status = %q, want active", m.Status)
	}
	if m := loadMem(t, filepath.Join(root, "fact", "mb.md")); m.Supersedes != "" {
		t.Fatalf("mb supersedes = %q, want empty", m.Supersedes)
	}
	if strings.Contains(loadMem(t, pnote).Content, "[dream:") {
		t.Fatalf("note marker not rolled back: %+v", loadMem(t, pnote))
	}
}

func TestDreamSecondRunNoChanges(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	d, root := dreamer(t, now)
	writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局。", now.Add(-2*time.Hour))
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局！", now.Add(-time.Hour))
	writeMem(t, root, store.Memory{ID: "mp", Kind: "fact", Name: "仓库路径"}, "工作目录 /srv/project/demo 需要单独配置。", now.Add(-time.Hour))

	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep2, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Merged != 0 || rep2.Superseded != 0 || len(rep2.Changes) != 0 {
		t.Fatalf("second run changed things: %+v", rep2)
	}
	if len(rep2.Notes) != 0 {
		t.Fatalf("second run notes = %v, want none", rep2.Notes)
	}
}

func TestDreamBadFrontmatterSkipped(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	d, root := dreamer(t, now)
	bad := filepath.Join(root, "fact")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"bad1.md":         "no frontmatter at all\n",
		"bad2.md":         "---\nname: [broken\n---\nbody\n",
		"bad3.md":         "---\nname: unterminated\n",
		"bad4.md":         "---\nname: bad id\nid: ../escape\n---\nbody\n",
		"mz-notmd.md.txt": "not markdown suffix\n",
	} {
		if err := os.WriteFile(filepath.Join(bad, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局。", now.Add(-2*time.Hour))
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局！", now.Add(-time.Hour))

	rep, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("bad frontmatter must not crash the run: %v", err)
	}
	if rep.Examined != 2 || rep.Superseded != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestDreamIndexSync(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	root := t.TempDir()
	x := mustOpen(t, filepath.Join(t.TempDir(), "idx.db"), true)
	defer x.Close()
	d := NewDreamer(DreamConfig{Root: root, Index: x, Now: func() time.Time { return now }})
	writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局。", now.Add(-2*time.Hour))
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局！", now.Add(-time.Hour))
	if err := x.Rebuild(root); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The superseded entry must be hidden from index search after the pass.
	hits, err := x.Search("构建缓存目录", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == "ma" {
			t.Fatalf("superseded entry still visible: %+v", hits)
		}
	}
	if len(hits) != 1 || hits[0].ID != "mb" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestDreamEdgeCases(t *testing.T) {
	now := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	// Missing root: an empty cycle, no error (NewDreamer nil Now uses time.Now).
	d := NewDreamer(DreamConfig{Root: filepath.Join(t.TempDir(), "absent")})
	rep, err := d.Run(context.Background())
	if err != nil || rep.Examined != 0 || len(rep.Changes) != 0 {
		t.Fatalf("empty root: rep=%+v err=%v", rep, err)
	}

	// Equal CreatedAt falls back to the greater (time-ordered) id.
	a := &dreamEntry{mem: store.Memory{ID: "ma", CreatedAt: now}}
	b := &dreamEntry{mem: store.Memory{ID: "mb", CreatedAt: now}}
	if newerOf(a, b) != b || newerOf(b, a) != b {
		t.Fatal("tie-break must pick the greater id")
	}

	// Rollback skips files deleted since the run.
	d2, root := dreamer(t, now)
	p := writeMem(t, root, store.Memory{ID: "ma", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局。", now.Add(-2*time.Hour))
	writeMem(t, root, store.Memory{ID: "mb", Kind: "fact", Name: "构建缓存目录"}, "构建缓存目录使用内容寻址分桶布局！", now.Add(-time.Hour))
	rep2, err := d2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := d2.Rollback(context.Background(), rep2); err != nil {
		t.Fatalf("rollback with deleted file: %v", err)
	}
	if m := loadMem(t, filepath.Join(root, "fact", "mb.md")); m.Supersedes != "" {
		t.Fatalf("mb supersedes = %q, want rolled back to empty", m.Supersedes)
	}
}

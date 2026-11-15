package memory

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/store"
)

func TestListGetAndSupersedeMemory(t *testing.T) {
	root := t.TempDir()
	fs := New(root)
	ctx := context.Background()
	m, err := fs.AddMemory(ctx, store.Memory{
		Kind: "fact", Name: "CPU 型号", Keywords: []string{"cpu"},
		Content: "server CPU is Xeon",
	})
	if err != nil {
		t.Fatal(err)
	}

	list, err := fs.ListMemories(ctx, "", nil)
	if err != nil || len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	got, err := fs.GetMemory(ctx, m.ID, "")
	if err != nil || got.Content != "server CPU is Xeon" {
		t.Fatalf("get = %+v, %v", got, err)
	}

	sup, err := fs.Supersede(ctx, m.ID, "")
	if err != nil || sup.Status != StatusSuperseded {
		t.Fatalf("supersede = %+v, %v", sup, err)
	}
	if list, err := fs.ListMemories(ctx, "", nil); err != nil || len(list) != 0 {
		t.Fatalf("superseded memory still listed: %+v, %v", list, err)
	}
	if _, err := fs.GetMemory(ctx, m.ID, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("get after supersede = %v", err)
	}
	data, err := os.ReadFile(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "status: superseded") {
		t.Errorf("frontmatter not rewritten:\n%s", data)
	}

	if _, err := fs.Supersede(ctx, "missing", ""); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing supersede = %v", err)
	}
	if _, err := fs.Supersede(ctx, "bad/id", ""); err == nil {
		t.Error("invalid id accepted")
	}
}

func TestProjectMemoryListAndSupersede(t *testing.T) {
	root := t.TempDir()
	project := t.TempDir()
	fs := New(root)
	ctx := context.Background()
	global, err := fs.AddMemory(ctx, store.Memory{Kind: "fact", Name: "global fact", Content: "g"})
	if err != nil {
		t.Fatal(err)
	}
	pm, err := fs.AddMemory(ctx, store.Memory{
		Kind: "fact", Name: "project fact", Content: "p", ProjectPath: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pm.Domain != DomainProject || !strings.HasPrefix(pm.Path, project) {
		t.Fatalf("project memory = %+v", pm)
	}

	list, err := fs.ListMemories(ctx, project, nil)
	if err != nil || len(list) != 2 {
		t.Fatalf("project list = %+v, %v", list, err)
	}
	if _, err := fs.Supersede(ctx, pm.ID, project); err != nil {
		t.Fatal(err)
	}
	list, err = fs.ListMemories(ctx, project, nil)
	if err != nil || len(list) != 1 || list[0].ID != global.ID {
		t.Fatalf("after project supersede = %+v, %v", list, err)
	}
}

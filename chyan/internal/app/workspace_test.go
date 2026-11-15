package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

func TestResolveWorkDir(t *testing.T) {
	base := t.TempDir()
	if got, err := resolveWorkDir(base, ""); err != nil || got != base {
		t.Fatalf("default = %q, %v", got, err)
	}
	alt := t.TempDir()
	if got, err := resolveWorkDir(base, alt); err != nil || got != alt {
		t.Fatalf("explicit = %q, %v", got, err)
	}
	file := filepath.Join(alt, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWorkDir(base, file); err == nil {
		t.Fatal("a file must not be accepted as a workspace")
	}
	if _, err := resolveWorkDir(base, filepath.Join(alt, "missing")); err == nil {
		t.Fatal("a missing directory must not be accepted")
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(alt, link); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveWorkDir(base, link); err != nil || got != alt {
		t.Fatalf("symlink = %q, %v", got, err)
	}
}

func TestRegistryBindsSelectedWorkspace(t *testing.T) {
	a := newTestApp(t, &stubBackend{})
	alt := t.TempDir()
	reg := a.registryFor("ses", nil, alt)
	tool, ok := reg.Get("write")
	if !ok {
		t.Fatal("write tool missing")
	}
	res := tool.Execute(context.Background(), core.ToolCall{
		Name: "write", Input: map[string]any{"path": "x.txt", "content": "hi"},
	})
	if res.Err != nil {
		t.Fatalf("write: %v", res.Err)
	}
	if _, err := os.Stat(filepath.Join(alt, "x.txt")); err != nil {
		t.Fatalf("file not written to selected workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.WorkDir(), "x.txt")); !os.IsNotExist(err) {
		t.Fatal("write escaped the startup workspace")
	}
}

func TestRunOptsUsesSelectedWorkspaceAndModel(t *testing.T) {
	backend := &stubBackend{}
	a := newTestApp(t, backend)
	alt := t.TempDir()
	if err := os.WriteFile(filepath.Join(alt, "AGENTS.md"), []byte("ALT-RULE-MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.RunOpts(context.Background(), "", "hi", RunOptions{
		WorkDir: alt, Model: "custom/model",
	}); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) == 0 {
		t.Fatal("no LLM requests recorded")
	}
	first := backend.requests[0] // the turn; later calls are title generation
	if first.Model != "custom/model" {
		t.Errorf("model = %q", first.Model)
	}
	if len(first.Messages) == 0 {
		t.Fatal("no messages sent")
	}
	sys := core.FlattenText(first.Messages[0])
	if sys == "" || !strings.Contains(sys, "ALT-RULE-MARKER") {
		t.Errorf("system prompt does not use the selected workspace directives: %q", sys)
	}
}

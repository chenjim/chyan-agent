package commands

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCommand plants one command file under root.
func writeCommand(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadGlobalAndProjectOverride(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	writeCommand(t, global, "foo.md", "---\ndescription: g\n---\nrun foo\n")
	writeCommand(t, global, "shared.md", "---\ndescription: from-global\n---\ntemplate g\n")
	writeCommand(t, project, "shared.md", "---\ndescription: from-project\n---\ntemplate p\n")
	writeCommand(t, project, "bar.md", "---\ndescription: p\n---\nrun bar\n")

	got, err := Load(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	names := []string{got[0].Name, got[1].Name, got[2].Name}
	if names[0] != "bar" || names[1] != "foo" || names[2] != "shared" {
		t.Fatalf("order = %v", names)
	}
	shared := got[2]
	if shared.Description != "from-project" || shared.Template != "template p" {
		t.Fatalf("override lost: %+v", shared)
	}
	if shared.Path != filepath.Join(project, "shared.md") {
		t.Fatalf("path = %q", shared.Path)
	}
}

func TestLoadProjectDirEmptyAndMissingRoots(t *testing.T) {
	global := t.TempDir()
	writeCommand(t, global, "only.md", "---\ndescription: d\n---\nb\n")
	got, err := Load(global, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "only" {
		t.Fatalf("got = %+v", got)
	}
	got, err = Load(filepath.Join(global, "nope"), filepath.Join(global, "gone"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got = %+v, err = %v", got, err)
	}
	if got, err = Load("", ""); err != nil || len(got) != 0 {
		t.Fatalf("got = %+v, err = %v", got, err)
	}
}

func TestLoadSkipsBroken(t *testing.T) {
	global := t.TempDir()
	writeCommand(t, global, "good.md", "---\ndescription: d\n---\ntemplate body\n")
	writeCommand(t, global, "no-frontmatter.md", "just a body\n")
	writeCommand(t, global, "bad-yaml.md", "---\n: : :\n---\nbody\n")
	writeCommand(t, global, "unterminated.md", "---\ndescription: d\nbody")
	writeCommand(t, global, "empty-template.md", "---\ndescription: d\n---\n   \n")
	writeCommand(t, global, "not-markdown.txt", "x")
	if err := os.MkdirAll(filepath.Join(global, "sub.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Load(global, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("got = %+v", got)
	}
}

func TestLoadReadDirError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file, ""); err == nil {
		t.Fatal("want error when root is a file")
	}
}

func TestRender(t *testing.T) {
	cmd := Command{Name: "x", Template: "fix $ARGUMENTS and $ARGUMENTS again"}
	out, err := Render(cmd, "the bug")
	if err != nil {
		t.Fatal(err)
	}
	if out != "fix the bug and the bug again" {
		t.Fatalf("out = %q", out)
	}
	// A template without the placeholder passes through unchanged.
	cmd.Template = "no placeholder"
	if out, err = Render(cmd, "args"); err != nil || out != "no placeholder" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if _, err := Render(Command{}, "args"); err == nil {
		t.Fatal("want error on empty template")
	}
}

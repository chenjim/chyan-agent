package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSkill plants one skill dir with its SKILL.md content under root.
func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadGlobalAndProjectOverride(t *testing.T) {
	global := t.TempDir()
	project := t.TempDir()
	writeSkill(t, global, "alpha", "---\nname: alpha\ndescription: g\n---\nbody a\n")
	writeSkill(t, global, "shared", "---\nname: shared\ndescription: from-global\n---\nbody g\n")
	writeSkill(t, project, "shared", "---\nname: shared\ndescription: from-project\n---\nbody p\n")
	writeSkill(t, project, "beta", "---\nname: beta\ndescription: p\n---\nbody b\n")

	got, err := Load(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	names := []string{got[0].Name, got[1].Name, got[2].Name}
	if names[0] != "alpha" || names[1] != "beta" || names[2] != "shared" {
		t.Fatalf("order = %v", names)
	}
	shared := got[2]
	if shared.Description != "from-project" || shared.Body != "body p" {
		t.Fatalf("override lost: %+v", shared)
	}
	if shared.Path != filepath.Join(project, "shared", "SKILL.md") {
		t.Fatalf("path = %q", shared.Path)
	}
}

func TestLoadProjectDirEmptyAndMissingRoots(t *testing.T) {
	global := t.TempDir()
	writeSkill(t, global, "only", "---\nname: only\n---\nb\n")
	got, err := Load(global, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "only" {
		t.Fatalf("got = %+v", got)
	}
	// Missing roots (and empty root args) yield nothing, not an error.
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
	writeSkill(t, global, "good", "---\nname: good\ndescription: d\n---\nbody\n")
	writeSkill(t, global, "no-frontmatter", "just a body\n")
	writeSkill(t, global, "bad-yaml", "---\n: : :\n---\nbody\n")
	writeSkill(t, global, "unterminated", "---\nname: unterminated\nbody")
	writeSkill(t, global, "no-name", "---\ndescription: d\n---\nbody\n")
	writeSkill(t, global, "empty-body", "---\nname: empty-body\n---\n   \n")
	// A dir without SKILL.md and a plain file are both skipped silently.
	if err := os.MkdirAll(filepath.Join(global, "no-file"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "stray.md"), []byte("x"), 0o644); err != nil {
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

// TestLoadGroupDirsAndHidden: a directory without SKILL.md is a group and its
// children load one level deeper; hidden dirs are never scanned (no .git noise).
func TestLoadGroupDirsAndHidden(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "flat", "---\nname: flat\n---\nb\n")
	writeSkill(t, filepath.Join(root, "devops"), "ops-a", "---\nname: ops-a\n---\nb\n")
	writeSkill(t, filepath.Join(root, "devops"), "ops-b", "---\nname: ops-b\n---\nb\n")
	writeSkill(t, filepath.Join(root, ".git"), "junk", "---\nname: junk\n---\nb\n")

	got, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, sk := range got {
		names = append(names, sk.Name)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (flat + 2 group children): %v", len(got), names)
	}
	if names[0] != "flat" || names[1] != "ops-a" || names[2] != "ops-b" {
		t.Fatalf("names = %v", names)
	}
	for _, sk := range got {
		if sk.Name == "junk" {
			t.Fatal("hidden .git dir must not load skills")
		}
	}
}

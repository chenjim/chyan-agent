package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errInvalid = errors.New("cross-device")

func TestHome(t *testing.T) {
	t.Setenv("CHYAN_HOME", filepath.Join(t.TempDir(), "custom"))
	h, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(h, "custom") {
		t.Errorf("CHYAN_HOME ignored: %s", h)
	}
}

func TestInitDirsCreatesLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home")
	t.Setenv("CHYAN_HOME", root)
	d, info, err := InitDirs()
	if err != nil {
		t.Fatal(err)
	}
	if info != "" {
		t.Errorf("no migration expected, got %q", info)
	}
	for _, dir := range []string{d.Root, d.Secret, d.Cache} {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if !st.IsDir() {
			t.Errorf("%s is not a dir", dir)
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s perm = %o, want 700", dir, perm)
		}
	}
}

func TestInitDirsMigratesLegacy(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "newhome")
	cfgDir := filepath.Join(tmp, "cfg", "chyan")
	cacheDir := filepath.Join(tmp, "cache", "chyan")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("CHYAN_HOME", root)

	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cfgDir, "config.toml"), "x = 1")
	write(filepath.Join(cfgDir, "chyan.db"), "db")
	write(filepath.Join(cfgDir, "secret", "tok"), "TOK")
	write(filepath.Join(cfgDir, "unknown.txt"), "keep")
	write(filepath.Join(cacheDir, "shot.png"), "img")

	d, info, err := InitDirs()
	if err != nil {
		t.Fatal(err)
	}
	if info == "" {
		t.Fatal("want migration info string")
	}
	for path, want := range map[string]string{
		filepath.Join(d.Root, "config.toml"): "x = 1",
		filepath.Join(d.Root, "chyan.db"):    "db",
		filepath.Join(d.Secret, "tok"):       "TOK",
		filepath.Join(d.Cache, "shot.png"):   "img",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("migrated file missing %s: %v", path, err)
			continue
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", path, data, want)
		}
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "unknown.txt")); err != nil {
		t.Errorf("unknown legacy entry must stay in place: %v", err)
	}
	// second start: nothing left to migrate
	_, info2, err := InitDirs()
	if err != nil {
		t.Fatal(err)
	}
	if info2 != "" {
		t.Errorf("second init should not migrate, got %q", info2)
	}
}

func TestInitDirsNoOverwrite(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "newhome")
	cfgDir := filepath.Join(tmp, "cfg", "chyan")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("CHYAN_HOME", root)
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InitDirs(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Errorf("existing file must not be overwritten, got %q", data)
	}
	// leftover source stays because the destination existed
	if _, err := os.Stat(filepath.Join(cfgDir, "config.toml")); err != nil {
		t.Errorf("source should remain: %v", err)
	}
}

func TestInitDirsMergesSecretDir(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg", "chyan")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("CHYAN_HOME", filepath.Join(tmp, "newhome"))
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cfgDir, "secret", "a"), "A")
	write(filepath.Join(cfgDir, "secret", "b"), "B")
	d, _, err := InitDirs()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a": "A", "b": "B"} {
		data, err := os.ReadFile(filepath.Join(d.Secret, name))
		if err != nil {
			t.Errorf("secret %s: %v", name, err)
			continue
		}
		if string(data) != want {
			t.Errorf("secret %s = %q", name, data)
		}
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "secret")); !os.IsNotExist(err) {
		t.Errorf("legacy secret dir should be removed: %v", err)
	}
}

// TestInitDirsMergeConflictSurvives: a legacy secret entry whose destination
// already exists is skipped, and the non-empty source dir must not abort
// startup with ENOTEMPTY.
func TestInitDirsMergeConflictSurvives(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg", "chyan")
	root := filepath.Join(tmp, "newhome")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("CHYAN_HOME", root)
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cfgDir, "secret", "a"), "legacy")
	write(filepath.Join(cfgDir, "secret", "b"), "B")
	write(filepath.Join(root, "secret", "a"), "current")

	if _, _, err := InitDirs(); err != nil {
		t.Fatalf("conflicting merge must not block startup: %v", err)
	}
	// Never overwrite, and the non-conflicting entry still moves.
	data, err := os.ReadFile(filepath.Join(root, "secret", "a"))
	if err != nil || string(data) != "current" {
		t.Fatalf("existing secret overwritten: %q %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(root, "secret", "b"))
	if err != nil || string(data) != "B" {
		t.Fatalf("non-conflicting secret not migrated: %q %v", data, err)
	}
	// The skipped entry stays in the legacy dir (harmless leftover).
	if _, err := os.Stat(filepath.Join(cfgDir, "secret", "a")); err != nil {
		t.Errorf("skipped source should remain: %v", err)
	}
}

func TestInitDirsError(t *testing.T) {
	// CHYAN_HOME pointing at a regular file makes MkdirAll fail.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHYAN_HOME", file)
	if _, _, err := InitDirs(); err == nil {
		t.Fatal("want error")
	}
}

func TestHomeDefault(t *testing.T) {
	t.Setenv("CHYAN_HOME", "")
	home := filepath.Join(t.TempDir(), "fakehome")
	t.Setenv("HOME", home)
	h, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if h != filepath.Join(home, ".chyan") {
		t.Errorf("Home = %s", h)
	}
}

func TestMovePathCopyFallback(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "a.txt")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(tmp, "b.txt")
	orig := renamePath
	renamePath = func(_, _ string) error { return errCrossDevice }
	defer func() { renamePath = orig }()
	if err := movePath(src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "data" {
		t.Fatalf("copy fallback failed: %q %v", data, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be removed after copy")
	}
}

func TestMovePathErrors(t *testing.T) {
	tmp := t.TempDir()
	// missing source
	if err := movePath(filepath.Join(tmp, "ghost"), filepath.Join(tmp, "x")); err == nil {
		t.Error("want error for missing source")
	}
	// destination parent missing: rename and copy both fail
	src := filepath.Join(tmp, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := renamePath
	renamePath = func(_, _ string) error { return errCrossDevice }
	defer func() { renamePath = orig }()
	if err := movePath(src, filepath.Join(tmp, "no-dir", "b.txt")); err == nil {
		t.Error("want error for missing destination dir")
	}
}

// TestMovePathDirectoryErrors exercises the directory branches: a destination
// parent that is a regular file, a nested entry that cannot be moved, and the
// copy fallback of a dangling symlink.
func TestMovePathDirectoryErrors(t *testing.T) {
	tmp := t.TempDir()
	// Destination under a regular file: MkdirAll fails.
	dstParent := filepath.Join(tmp, "blocked")
	if err := os.WriteFile(dstParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(filepath.Join(src, "leaf"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "leaf", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := movePath(src, filepath.Join(dstParent, "dst")); err == nil {
		t.Fatal("want MkdirAll error when the destination parent is a file")
	}

	// Nested entry whose destination is an existing directory-shaped file:
	// the recursion error is returned to the caller.
	blocked := filepath.Join(tmp, "b")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "sub"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	src2 := filepath.Join(tmp, "s2")
	if err := os.MkdirAll(filepath.Join(src2, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := movePath(src2, blocked); err == nil {
		t.Fatal("want nested move error")
	}

	// A forced rename failure on a dangling symlink reaches os.Open and fails.
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(filepath.Join(tmp, "gone"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	orig := renamePath
	renamePath = func(_, _ string) error { return errCrossDevice }
	defer func() { renamePath = orig }()
	if err := movePath(link, filepath.Join(tmp, "moved-link")); err == nil {
		t.Fatal("want open error for a dangling symlink")
	}
}

// TestMoveEntriesCycleError: a legacy path that is a regular file is not a
// missing directory, so the error is reported instead of silently skipped.
func TestMoveEntriesCycleError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := moveEntries(file, nil, filepath.Join(t.TempDir(), "drop")); err == nil {
		t.Fatal("want error when the source is not a directory")
	}
}

// errCrossDevice simulates an EXDEV rename failure.
var errCrossDevice = &os.PathError{Op: "rename", Path: "x", Err: errInvalid}

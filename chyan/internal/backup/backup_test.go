package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// seedHome builds a fake ~/.chyan with a db, config, secret and cache file.
func seedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "secret"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "chyan.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT);
		CREATE TABLE audit (id INTEGER PRIMARY KEY, action TEXT, verdict TEXT, approved INTEGER);
		INSERT INTO session VALUES ('s1','first');
		INSERT INTO audit (action,verdict,approved) VALUES ('bash','low',1);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "config.toml"), "api_key = \"super-secret\"\nmodel = \"m\"\n", 0o600)
	write(filepath.Join(home, "secret", "llm_key"), "sk-live", 0o600)
	write(filepath.Join(home, "cache", "stale.bin"), "artifact", 0o600)
	write(filepath.Join(home, "jobs.json"), `[{"id":"j1","enabled":true},{"id":"j2","enabled":false}]`, 0o600)
	return home
}

// TestBackupRestoreRoundTrip: archive -> fresh home keeps every payload and
// the db is a readable snapshot.
func TestBackupRestoreRoundTrip(t *testing.T) {
	src := seedHome(t)
	var buf bytes.Buffer
	if err := Backup(context.Background(), src, &buf, Options{}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dst := t.TempDir()
	if err := Restore(context.Background(), &buf, dst); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, rel := range []string{"chyan.db", "config.toml", filepath.Join("secret", "llm_key"), "jobs.json"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("%s missing after restore: %v", rel, err)
		}
	}
	// cache is excluded by default.
	if _, err := os.Stat(filepath.Join(dst, "cache", "stale.bin")); !os.IsNotExist(err) {
		t.Error("cache must be excluded from the default backup")
	}
	// The restored db opens and has the row.
	db, err := sql.Open("sqlite", filepath.Join(dst, "chyan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow("SELECT title FROM session WHERE id='s1'").Scan(&title); err != nil || title != "first" {
		t.Errorf("restored db row = %q, %v", title, err)
	}
	// secret mode survives (0600).
	if fi, err := os.Stat(filepath.Join(dst, "secret", "llm_key")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("secret mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestBackupIncludeCache: opted-in cache artifacts are packed.
func TestBackupIncludeCache(t *testing.T) {
	src := seedHome(t)
	var buf bytes.Buffer
	if err := Backup(context.Background(), src, &buf, Options{IncludeCache: true}); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Restore(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "cache", "stale.bin")); err != nil {
		t.Errorf("cache artifact missing: %v", err)
	}
}

// TestRestoreRefusesExistingDB: never overwrite live data.
func TestRestoreRefusesExistingDB(t *testing.T) {
	src := seedHome(t)
	var buf bytes.Buffer
	if err := Backup(context.Background(), src, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	// Restoring over the same home (chyan.db present) must refuse.
	if err := Restore(context.Background(), &buf, src); err == nil {
		t.Fatal("restore over existing chyan.db must fail")
	}
}

// TestRestoreRejectsTraversal: archive paths cannot escape the target.
func TestRestoreRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("evil")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	dst := t.TempDir()
	if err := Restore(context.Background(), &buf, dst); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}

// TestRestoreRejectsSymlinkEscape: a symlink planted in the destination must
// not redirect extraction outside it.
func TestRestoreRejectsSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	dst := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "sub")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("evil")
	if err := tw.WriteHeader(&tar.Header{
		Name: "sub/escape.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	if err := Restore(context.Background(), &buf, dst); err == nil {
		t.Fatal("extraction through a symlink must be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("archive escaped through the planted symlink")
	}
}

// TestRestoreBadArchive: corrupt input errors cleanly.
func TestRestoreBadArchive(t *testing.T) {
	if err := Restore(context.Background(), strings.NewReader("not gzip"), t.TempDir()); err == nil {
		t.Fatal("corrupt archive must error")
	}
}

// TestRestoreMissingHome: a fresh target is created.
func TestRestoreMissingHome(t *testing.T) {
	src := seedHome(t)
	var buf bytes.Buffer
	if err := Backup(context.Background(), src, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "newhome")
	if err := Restore(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("target home must be created: %v", err)
	}
}

// TestDiagnoseRedactsSecrets: config values under secret-looking keys are
// masked, secret/ never packed, and summaries carry only counts.
func TestDiagnoseRedactsSecrets(t *testing.T) {
	home := seedHome(t)
	var buf bytes.Buffer
	if err := Diagnose(context.Background(), home, &buf, DiagInfo{Version: "0.1.0", OS: "linux", Arch: "amd64"}); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	var body []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Name == "diagnose.json" {
			body, _ = io.ReadAll(tr)
		}
	}
	if len(names) != 1 || names[0] != "diagnose.json" {
		t.Fatalf("bundle contents = %v, want only diagnose.json", names)
	}
	raw := string(body)
	if strings.Contains(raw, "super-secret") || strings.Contains(raw, "sk-live") {
		t.Fatalf("secret leaked into diagnostics:\n%s", raw)
	}
	if !strings.Contains(raw, `"api_key = \"***\""`) && !strings.Contains(raw, "***") {
		t.Errorf("config redaction marker missing:\n%s", raw)
	}
	var summary map[string]any
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatal(err)
	}
	if summary["version"] != "0.1.0" {
		t.Errorf("version = %v", summary["version"])
	}
	jobs, _ := summary["jobs"].(map[string]any)
	if jobs == nil || jobs["total"] != float64(2) || jobs["enabled"] != float64(1) {
		t.Errorf("jobs summary = %v", summary["jobs"])
	}
	db, _ := summary["db"].(map[string]any)
	if db == nil {
		t.Fatalf("db summary missing: %v", summary)
	}
	tables, _ := db["tables"].(map[string]any)
	if tables["session"] != float64(1) {
		t.Errorf("session rows = %v", tables["session"])
	}
	audit, _ := summary["audit_recent"].([]any)
	if len(audit) != 1 {
		t.Fatalf("audit summary = %v", summary["audit_recent"])
	}
	entry := audit[0].(map[string]any)
	if entry["action"] != "bash" {
		t.Errorf("audit entry = %v", entry)
	}
}

// TestRedactConfigInlineSecrets: inline tables and credential-shaped values
// are masked even when the enclosing key is innocuous (headers = { ... }).
func TestRedactConfigInlineSecrets(t *testing.T) {
	raw := []byte(
		"[llm]\n" +
			"model = \"m\"\n" +
			"api_key = \"sk-live-abcdefgh\"\n" +
			"headers = { Authorization = \"Bearer sk-live-1234567890\" }\n" +
			"[llm.fallback]\n" +
			"headers = { api_key = \"abc123\" }\n")
	out := string(redactConfig(raw))
	if strings.Contains(out, "sk-live") || strings.Contains(out, "abc123") {
		t.Fatalf("inline secret leaked:\n%s", out)
	}
	if !strings.Contains(out, `model = "m"`) {
		t.Errorf("non-secret values must survive:\n%s", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("redaction marker missing:\n%s", out)
	}
}

// TestDiagnoseMissingFiles: a nearly empty home still yields a bundle.
func TestDiagnoseMissingFiles(t *testing.T) {
	var buf bytes.Buffer
	if err := Diagnose(context.Background(), t.TempDir(), &buf, DiagInfo{Version: "x"}); err != nil {
		t.Fatalf("Diagnose on empty home: %v", err)
	}
	gz, _ := gzip.NewReader(&buf)
	tr := tar.NewReader(gz)
	if hdr, err := tr.Next(); err != nil || hdr.Name != "diagnose.json" {
		t.Errorf("bundle = %v, %v", hdr, err)
	}
}

// TestContextCancellation: a cancelled ctx aborts Backup, Restore and
// Diagnose before doing work.
func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if err := Backup(ctx, seedHome(t), &buf, Options{}); err == nil {
		t.Error("Backup with cancelled ctx must fail")
	}
	if err := Restore(ctx, &buf, t.TempDir()); err == nil {
		t.Error("Restore with cancelled ctx must fail")
	}
	if err := Diagnose(ctx, t.TempDir(), &buf, DiagInfo{}); err == nil {
		t.Error("Diagnose with cancelled ctx must fail")
	}
}

// TestBackupWithoutDB: a home with no chyan.db backs up fine.
func TestBackupWithoutDB(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Backup(context.Background(), home, &buf, Options{}); err != nil {
		t.Fatalf("Backup without db: %v", err)
	}
	dst := t.TempDir()
	if err := Restore(context.Background(), &buf, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "config.toml")); err != nil {
		t.Error("config missing after restore")
	}
}

// TestSafeJoinRejectsWindowsTraversal: backslash separators are split too.
func TestSafeJoinRejectsWindowsTraversal(t *testing.T) {
	if _, err := safeJoin("/tmp/x", `..\..\evil.txt`); err == nil {
		t.Error("backslash traversal must be rejected")
	}
	if dst, err := safeJoin("/tmp/x", "sub/file.txt"); err != nil || !strings.HasSuffix(dst, "file.txt") {
		t.Errorf("safe path = %q, %v", dst, err)
	}
}

// TestRestoreForceSnapshotsCurrentHome: --force overwrites only after writing
// a pre-restore archive of the live home.
func TestRestoreForceSnapshotsCurrentHome(t *testing.T) {
	src := seedHome(t)
	var buf bytes.Buffer
	if err := Backup(context.Background(), src, &buf, Options{}); err != nil {
		t.Fatal(err)
	}

	// Destination already has data that must be preserved in the snapshot.
	parent := t.TempDir()
	dst := filepath.Join(parent, "chyan-home")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	oldDB, err := sql.Open("sqlite", filepath.Join(dst, "chyan.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldDB.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT);
		INSERT INTO session VALUES ('old','before-restore');`); err != nil {
		t.Fatal(err)
	}
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}

	if err := RestoreWithOptions(context.Background(), &buf, dst, RestoreOptions{Force: true}); err != nil {
		t.Fatalf("forced restore: %v", err)
	}
	// The pre-restore snapshot sits next to the home and still holds the old row.
	snaps, err := filepath.Glob(filepath.Join(parent, "chyan-pre-restore-*.tar.gz"))
	if err != nil || len(snaps) != 1 {
		t.Fatalf("pre-restore snapshots = %v, %v", snaps, err)
	}
	f, err := os.Open(snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	snapDir := t.TempDir()
	if err := Restore(context.Background(), f, snapDir); err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(snapDir, "chyan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow("SELECT title FROM session WHERE id='old'").Scan(&title); err != nil || title != "before-restore" {
		t.Fatalf("snapshot row = %q, %v", title, err)
	}

	// The destination now carries the restored source row.
	db2, err := sql.Open("sqlite", filepath.Join(dst, "chyan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.QueryRow("SELECT title FROM session WHERE id='s1'").Scan(&title); err != nil || title != "first" {
		t.Fatalf("restored row = %q, %v", title, err)
	}
}

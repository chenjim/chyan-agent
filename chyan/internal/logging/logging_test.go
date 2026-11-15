// Logging tests: daily rotation, retention pruning and credential redaction.
package logging

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterRotatesDaily(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	day := time.Date(2026, 10, 29, 23, 59, 0, 0, time.UTC)
	w.now = func() time.Time { return day }

	if _, err := w.Write([]byte("first line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	w.now = func() time.Time { return day.Add(2 * time.Minute) } // next day
	if _, err := w.Write([]byte("second line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	files := SortedFiles(dir)
	if len(files) != 2 {
		t.Fatalf("log files = %v, want 2", files)
	}
	for i, want := range []string{"first line", "second line"} {
		body, err := os.ReadFile(files[i])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s missing %q: %s", files[i], want, body)
		}
	}
	// Daily files are private.
	if info, err := os.Stat(files[0]); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestRetentionDefaults(t *testing.T) {
	if DefaultKeepDays != 90 {
		t.Errorf("DefaultKeepDays = %d, want 90", DefaultKeepDays)
	}
	if DefaultCompressAfterDays != 3 {
		t.Errorf("DefaultCompressAfterDays = %d, want 3", DefaultCompressAfterDays)
	}
}

func TestPruneRemovesOldLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -100).Format("20060102")
	older := now.AddDate(0, 0, -91).Format("20060102")
	// Exactly 90 days old is still inside the window (cutoff uses Before).
	boundary := now.AddDate(0, 0, -90).Format("20060102")
	recent := now.AddDate(0, 0, -2).Format("20060102")
	// Plain and archived files share the same retention window.
	for _, name := range []string{"chyan-" + old + ".log", "chyan-" + older + ".log.zip"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, day := range []string{boundary, recent} {
		if err := os.WriteFile(filepath.Join(dir, "chyan-"+day+".log"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated files are left alone.
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := Prune(dir, 90, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 2 { // 100 and 91 days old; 90-day boundary and recent files stay
		t.Fatalf("pruned = %d, want 2", n)
	}
	files := SortedFiles(dir)
	if len(files) != 2 {
		t.Fatalf("remaining = %v, want 2 files", files)
	}
	for _, want := range []time.Time{now.AddDate(0, 0, -90), now.AddDate(0, 0, -2)} {
		if _, err := os.Stat(filepath.Join(dir, "chyan-"+want.Format("20060102")+".log")); err != nil {
			t.Errorf("boundary/recent log missing: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Errorf("unrelated file removed: %v", err)
	}
}

func TestCompressArchivesOldLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -9).Format("20060102")
	older := now.AddDate(0, 0, -4).Format("20060102")
	// Exactly 3 days old is not older than the compression window.
	boundary := now.AddDate(0, 0, -3).Format("20060102")
	recent := now.AddDate(0, 0, -2).Format("20060102")
	if err := os.WriteFile(filepath.Join(dir, "chyan-"+old+".log"), []byte("old-log-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{older, boundary, recent} {
		if err := os.WriteFile(filepath.Join(dir, "chyan-"+day+".log"), []byte("keep-"+day), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	n, err := Compress(dir, 3, now)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if n != 2 { // 9 and 4 days old; the 3-day boundary stays plain
		t.Fatalf("archived = %d, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "chyan-"+old+".log")); !os.IsNotExist(err) {
		t.Fatalf("source log still present: %v", err)
	}
	zipPath := filepath.Join(dir, "chyan-"+old+".log.zip")
	info, err := os.Stat(zipPath)
	if err != nil {
		t.Fatalf("zip missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("zip mode = %v, want 0600", info.Mode().Perm())
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	if len(zr.File) != 1 || zr.File[0].Name != "chyan-"+old+".log" {
		t.Fatalf("zip entries = %v", zr.File)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "old-log-data" {
		t.Errorf("zip body = %q", body)
	}
	// Boundary and recent files are never archived.
	for _, day := range []string{boundary, recent} {
		if _, err := os.Stat(filepath.Join(dir, "chyan-"+day+".log")); err != nil {
			t.Fatalf("unarchived log missing: %v", err)
		}
	}

	// An existing non-empty archive wins over the plain file (no duplicates).
	dupDay := now.AddDate(0, 0, -10).Format("20060102")
	dupLog := filepath.Join(dir, "chyan-"+dupDay+".log")
	if err := os.WriteFile(dupLog, []byte("duplicate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dupLog+".zip", []byte("already-archived"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err = Compress(dir, 3, now)
	if err != nil {
		t.Fatalf("Compress again: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived again = %d, want 1", n)
	}
	if _, err := os.Stat(dupLog); !os.IsNotExist(err) {
		t.Fatalf("duplicate log still present: %v", err)
	}

	// A stale temp file from an interrupted run must not block archiving.
	staleDay := now.AddDate(0, 0, -5).Format("20060102")
	staleLog := filepath.Join(dir, "chyan-"+staleDay+".log")
	if err := os.WriteFile(staleLog, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleLog+".zip.tmp", []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err = Compress(dir, 3, now)
	if err != nil {
		t.Fatalf("Compress with stale tmp: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived with stale tmp = %d, want 1", n)
	}
	if _, err := os.Stat(staleLog); !os.IsNotExist(err) {
		t.Fatalf("stale log still present: %v", err)
	}
	if _, err := os.Stat(staleLog + ".zip.tmp"); !os.IsNotExist(err) {
		t.Fatalf("stale temp still present: %v", err)
	}
}

func TestWriterCompressesOldLogsOnRotation(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	day1 := time.Date(2026, 10, 29, 23, 59, 0, 0, time.UTC)
	w.now = func() time.Time { return day1 }
	if _, err := w.Write([]byte("old line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Rotating four days later makes the first file older than the 3-day
	// compression window.
	w.now = func() time.Time { return day1.AddDate(0, 0, 4) }
	if _, err := w.Write([]byte("new line\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "chyan-20261029.log")); !os.IsNotExist(err) {
		t.Fatalf("old plain log still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "chyan-20261029.log.zip")); err != nil {
		t.Fatalf("old log not archived: %v", err)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		`api_key=sk-live-123 model=m`:       `api_key=*** model=m`,
		`Authorization: Bearer abc.DEF-123`: `Authorization=*** ***`,
		`config secret:llm_key loaded`:      `config secret=*** loaded`,
		`token: xyz`:                        `token=***`,
		`nothing sensitive here`:            `nothing sensitive here`,
		`tokens_in=100 tokens_out=50`:       `tokens_in=100 tokens_out=50`,
		`password = hunter2`:                `password=***`,
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetupOffWritesNoFile(t *testing.T) {
	t.Setenv("CHYAN_LOG_LEVEL", "off")
	home := t.TempDir()
	stop := Setup(home, 7)
	stop()
	if files := SortedFiles(filepath.Join(home, "logs")); len(files) != 0 {
		t.Fatalf("CHYAN_LOG_LEVEL=off wrote %v", files)
	}
}

// Package logging mirrors the standard logger into daily files under
// <home>/logs (FR-11): one file per day, 0600, old files pruned, and values
// that look like credentials redacted before they reach the file. stderr
// stays raw so terminals and tests see what the program actually logged.
package logging

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultKeepDays is the retention for daily log files.
const DefaultKeepDays = 90

// DefaultCompressAfterDays is the age at which a daily file is archived to zip.
const DefaultCompressAfterDays = 3

// Writer appends log lines to <dir>/chyan-<date>.log and rotates when the
// date changes. It is safe for concurrent use.
type Writer struct {
	dir               string
	now               func() time.Time
	keepDays          int
	compressAfterDays int

	mu   sync.Mutex
	day  string
	file *os.File
}

// NewWriter builds a daily writer rooted at dir (created on first write).
func NewWriter(dir string) *Writer {
	return &Writer{
		dir:               dir,
		now:               time.Now,
		keepDays:          DefaultKeepDays,
		compressAfterDays: DefaultCompressAfterDays,
	}
}

// Write implements io.Writer: it rotates on a new day, redacts credentials
// and appends the line to the day file.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotateLocked(); err != nil {
		return 0, err
	}
	if _, err := w.file.WriteString(Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close closes the current file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotateLocked opens the file for the current day, closing the previous one.
func (w *Writer) rotateLocked() error {
	day := w.now().Format("20060102")
	if w.file != nil && w.day == day {
		return nil
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return fmt.Errorf("logging: create %s: %w", w.dir, err)
	}
	path := filepath.Join(w.dir, "chyan-"+day+".log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("logging: open %s: %w", path, err)
	}
	w.file = f
	w.day = day
	// Rotation is a good moment to maintain the directory: long-running
	// daemons never restart, so startup-only maintenance would never run again.
	// Prune first: files past the retention window must not be archived just
	// to be deleted immediately afterwards.
	_, _ = Prune(w.dir, w.keepDays, w.now())
	_, _ = Compress(w.dir, w.compressAfterDays, w.now())
	return nil
}

// logDay parses a daily log name (chyan-YYYYMMDD.log or its .zip archive)
// and returns the date it belongs to.
func logDay(name string, loc *time.Location) (time.Time, bool) {
	if !strings.HasPrefix(name, "chyan-") {
		return time.Time{}, false
	}
	base := strings.TrimPrefix(name, "chyan-")
	base = strings.TrimSuffix(base, ".zip")
	if !strings.HasSuffix(base, ".log") {
		return time.Time{}, false
	}
	dayPart := strings.TrimSuffix(base, ".log")
	if len(dayPart) != 8 {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation("20060102", dayPart, loc)
	return day, err == nil
}

// dayCutoff is the midnight `days` before now in the same location. Daily
// files carry no time of day, so comparing dates avoids treating a file
// exactly keepDays old as expired merely because the clock is past midnight.
func dayCutoff(now time.Time, days int) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -days)
}

// Prune removes daily log files (plain or zipped) older than keepDays
// relative to now and returns how many were removed.
func Prune(dir string, keepDays int, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if keepDays <= 0 {
		keepDays = DefaultKeepDays
	}
	cutoff := dayCutoff(now, keepDays)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		day, ok := logDay(e.Name(), now.Location())
		if !ok {
			continue
		}
		if day.Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// Compress archives daily log files older than afterDays into
// chyan-YYYYMMDD.log.zip and removes the plain file. Existing non-empty
// archives are treated as done: a crash between rename and remove must not
// leave duplicate data, while a zero-byte archive is rebuilt.
func Compress(dir string, afterDays int, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if afterDays <= 0 {
		afterDays = DefaultCompressAfterDays
	}
	cutoff := dayCutoff(now, afterDays)
	archived := 0
	for _, e := range entries {
		// Only plain daily files are compressed; existing .zip files stay.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		day, ok := logDay(e.Name(), now.Location())
		if !ok || !day.Before(cutoff) {
			continue
		}
		src := filepath.Join(dir, e.Name())
		dst := src + ".zip"
		if info, statErr := os.Stat(dst); statErr == nil {
			if info.Size() > 0 {
				if err := os.Remove(src); err != nil {
					return archived, err
				}
				archived++
				continue
			}
			// Incomplete archive from an interrupted run: rebuild it.
			_ = os.Remove(dst)
		}
		if err := zipFile(src, dst); err != nil {
			return archived, err
		}
		if err := os.Remove(src); err != nil {
			return archived, err
		}
		archived++
	}
	return archived, nil
}

// zipFile writes src into dst atomically (temp file + rename) so a failed
// archive never replaces a readable one.
func zipFile(src, dst string) (err error) {
	tmp := dst + ".tmp"
	// A previous run interrupted after opening the temp file leaves it behind;
	// O_EXCL would then block every retry. Removing the stale file is safe
	// because only complete archives are renamed to dst.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	zw := zip.NewWriter(out)
	entry, err := zw.Create(filepath.Base(src))
	if err != nil {
		_ = out.Close()
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		_ = out.Close()
		return err
	}
	if _, err = io.Copy(entry, in); err != nil {
		_ = in.Close()
		_ = out.Close()
		return err
	}
	_ = in.Close()
	if err = zw.Close(); err != nil {
		_ = out.Close()
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

var (
	// keyValue matches "api_key=...", "token: ...", "Authorization = ...".
	keyValue = regexp.MustCompile(`(?i)\b(api[_-]?key|token|authorization|password|secret)\b\s*[=:]\s*\S+`)
	// bearer matches an Authorization header value.
	bearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	// secretRef matches "secret:<name>" so the reference name stays visible
	// (it is not itself a credential).
	secretRef = regexp.MustCompile(`(?i)\bsecret:[A-Za-z0-9_.-]+`)
)

// Redact replaces credential-looking values with ***. It is intentionally
// conservative: the reference form secret:<name> is preserved because it names
// a file, not a key.
func Redact(s string) string {
	s = secretRef.ReplaceAllString(s, "secret:***")
	s = bearer.ReplaceAllString(s, "Bearer ***")
	s = keyValue.ReplaceAllString(s, "$1=***")
	return s
}

// Setup mirrors the standard logger to <home>/logs and keeps stderr output.
// CHYAN_LOG_LEVEL=off disables the file mirror. It is best-effort: callers
// keep working with plain stderr when the directory cannot be created.
func Setup(home string, keepDays int) (stop func()) {
	if strings.EqualFold(os.Getenv("CHYAN_LOG_LEVEL"), "off") {
		return func() {}
	}
	w := NewWriter(filepath.Join(home, "logs"))
	w.keepDays = keepDays
	log.SetOutput(io.MultiWriter(os.Stderr, w))
	return func() {
		log.SetOutput(os.Stderr)
		_ = w.Close()
	}
}

// SortedFiles lists the daily files, plain and archived (test helper and
// `status` use).
func SortedFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := logDay(e.Name(), time.Local); ok {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

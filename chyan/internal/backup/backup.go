// Package backup archives and restores the ~/.chyan data root and builds
// redacted diagnostics bundles (P5-4).
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// dbFileName is the SQLite file under the data root.
const dbFileName = "chyan.db"

// Options tunes Backup.
type Options struct {
	// IncludeCache packs cache/ too (default: excluded, artifacts only).
	IncludeCache bool
}

// Backup writes a gzipped tar of srcHome to w. cache/ is excluded unless
// opts.IncludeCache; chyan.db is snapshotted with VACUUM INTO first so the
// archive holds a consistent database even while the daemon runs.
func Backup(ctx context.Context, srcHome string, w io.Writer, opts Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	snap, err := snapshotDB(srcHome)
	if err != nil {
		return err
	}
	if snap != "" {
		defer os.Remove(snap)
	}

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.WalkDir(srcHome, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(srcHome, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		top := strings.Split(rel, string(filepath.Separator))[0]
		if top == "cache" && !opts.IncludeCache {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return writeHeader(tw, rel, info, tar.TypeDir)
		}
		if !info.Mode().IsRegular() {
			return nil // sockets/pipes never belong in an archive
		}
		src := path
		if rel == dbFileName && snap != "" {
			src = snap // consistent snapshot instead of the live file
			// The header must describe the data actually written: the live
			// database and the VACUUM snapshot differ in size, and a mismatched
			// header corrupts or fails the archive.
			if fi, err := os.Stat(snap); err == nil {
				info = fi
			}
		}
		return writeFile(tw, rel, info, src)
	})
}

// snapshotDB copies the live database to a temp file via VACUUM INTO,
// returning "" when there is no database to snapshot.
func snapshotDB(home string) (string, error) {
	src := filepath.Join(home, dbFileName)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("backup: stat db: %w", err)
	}
	tmp, err := os.CreateTemp("", "chyan-backup-*.db")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath) // VACUUM INTO requires a non-existent target

	db, err := sql.Open("sqlite", src)
	if err != nil {
		return "", fmt.Errorf("backup: open db: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), "VACUUM INTO ?", tmpPath); err != nil {
		return "", fmt.Errorf("backup: snapshot db: %w", err)
	}
	return tmpPath, nil
}

// writeHeader writes a tar header for a directory entry.
func writeHeader(tw *tar.Writer, rel string, info fs.FileInfo, kind byte) error {
	h := &tar.Header{
		Name: rel + "/", Typeflag: kind,
		Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(),
	}
	return tw.WriteHeader(h)
}

// writeFile copies one regular file into the tar.
func writeFile(tw *tar.Writer, rel string, info fs.FileInfo, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	h := &tar.Header{
		Name: rel, Typeflag: tar.TypeReg,
		Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime(),
	}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// snapshotHome writes the current home to a sibling archive before a forced
// restore overwrites it. The file name is timestamped and never reused.
func snapshotHome(ctx context.Context, home string) error {
	ts := time.Now().Format("20060102-150405")
	dst := filepath.Join(filepath.Dir(home), "chyan-pre-restore-"+ts+".tar.gz")
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup: pre-restore snapshot %s: %w", dst, err)
	}
	if err := Backup(ctx, home, f, Options{}); err != nil {
		f.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("backup: pre-restore snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("backup: pre-restore snapshot: %w", err)
	}
	return nil
}

// RestoreOptions tune Restore.
type RestoreOptions struct {
	// Force overwrites an existing chyan.db after snapshotting the current
	// home next to it (<parent>/chyan-pre-restore-<ts>.tar.gz). The snapshot
	// happens before any file is touched, so a failed restore is recoverable.
	Force bool
}

// Restore extracts a Backup archive into dstHome. It never overwrites an
// existing chyan.db: restoring over live data is destructive, so that case
// errors out and leaves the destination untouched.
func Restore(ctx context.Context, r io.Reader, dstHome string) error {
	return RestoreWithOptions(ctx, r, dstHome, RestoreOptions{})
}

// RestoreWithOptions is Restore plus the explicit overwrite path.
func RestoreWithOptions(ctx context.Context, r io.Reader, dstHome string, opts RestoreOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, statErr := os.Stat(filepath.Join(dstHome, dbFileName))
	if statErr == nil {
		if !opts.Force {
			return fmt.Errorf("backup: %s already exists in %s: refusing to overwrite (move it aside first, or pass --force)", dbFileName, dstHome)
		}
		if err := snapshotHome(ctx, dstHome); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.MkdirAll(dstHome, 0o700); err != nil {
		return fmt.Errorf("backup: create %s: %w", dstHome, err)
	}

	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("backup: read archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("backup: read archive: %w", err)
		}
		dst, err := safeJoin(dstHome, hdr.Name)
		if err != nil {
			return err
		}
		// Refuse to extract through a symlink planted in the destination:
		// MkdirAll and OpenFile would follow it outside the target home.
		if err := ensureInside(dstHome, dst); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o600
			}
			flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
			if opts.Force {
				// Forced restore replaces files instead of tripping over
				// them; the pre-restore snapshot already captured the old
				// state, and safeJoin/ensureInside still guard the path.
				flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			}
			f, err := os.OpenFile(dst, flags, mode)
			if err != nil {
				return fmt.Errorf("backup: extract %s: %w", hdr.Name, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

// ensureInside rejects an extraction target whose existing path components
// (below root) include a symlink. The lexical checks in safeJoin cannot see a
// link that points outside; this closes that hole before any write.
func ensureInside(root, dst string) error {
	rel, err := filepath.Rel(root, dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("backup: unsafe path: %q", dst)
	}
	cur := root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // the rest of the path does not exist yet
		}
		if err != nil {
			return fmt.Errorf("backup: inspect %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("backup: refusing to extract through symlink %s", cur)
		}
	}
	return nil
}

// safeJoin keeps extraction inside dstHome. Entries with absolute paths or
// ".." segments are rejected outright (checked on the raw name: Clean would
// silently collapse them into a safe-looking path), so a hostile archive is
// loud rather than neutralised.
func safeJoin(root, name string) (string, error) {
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if seg == ".." {
			return "", fmt.Errorf("backup: unsafe path in archive: %q", name)
		}
	}
	dst := filepath.Join(root, filepath.Clean("/"+name))
	if !strings.HasPrefix(dst, filepath.Clean(root)+string(filepath.Separator)) && dst != filepath.Clean(root) {
		return "", fmt.Errorf("backup: unsafe path in archive: %q", name)
	}
	return dst, nil
}

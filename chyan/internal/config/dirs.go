package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// isDirNotEmpty reports ENOTEMPTY/EEXIST from os.Remove on a directory whose
// leftover entries were skipped because their destination already existed.
func isDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// Home returns the private data root: $CHYAN_HOME when set (tests, extra
// instances), otherwise <user home>/.chyan (architecture 4.6).
func Home() (string, error) {
	if h := os.Getenv("CHYAN_HOME"); h != "" {
		return filepath.Clean(h), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".chyan"), nil
}

// Dirs holds the standard locations under the private data root.
type Dirs struct {
	Root   string // ~/.chyan
	Secret string // ~/.chyan/secret
	Cache  string // ~/.chyan/cache
}

// InitDirs creates ~/.chyan/{secret,cache} and migrates legacy data from
// {UserConfigDir}/chyan and {UserCacheDir}/chyan on first start. The
// returned info string is non-empty when a migration happened; the caller
// logs it once. Existing destination files are never overwritten.
func InitDirs() (Dirs, string, error) {
	root, err := Home()
	if err != nil {
		return Dirs{}, "", err
	}
	d := Dirs{
		Root:   root,
		Secret: filepath.Join(root, "secret"),
		Cache:  filepath.Join(root, "cache"),
	}
	for _, dir := range []string{d.Root, d.Secret, d.Cache} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return d, "", fmt.Errorf("create %s: %w", dir, err)
		}
	}
	info, err := migrate(d)
	if err != nil {
		return d, "", err
	}
	return d, info, nil
}

// migrate moves legacy layout entries into ~/.chyan. Returns "" when
// nothing was migrated. Known entries from the legacy config dir go to
// their new homes (secret/ merges into secret/); all legacy cache
// contents go into cache/.
func migrate(d Dirs) (string, error) {
	var moved []string

	if cfgOld, err := os.UserConfigDir(); err == nil {
		items, err := moveEntries(filepath.Join(cfgOld, "chyan"), map[string]string{
			"config.toml": filepath.Join(d.Root, "config.toml"),
			"chyan.db":    filepath.Join(d.Root, "chyan.db"),
			"secret":      d.Secret, // directory: contents merge into secret/
		}, "") // unknown legacy entries stay put
		if err != nil {
			return "", fmt.Errorf("migrate legacy config dir: %w", err)
		}
		moved = append(moved, items...)
	}

	if cacheOld, err := os.UserCacheDir(); err == nil {
		items, err := moveEntries(filepath.Join(cacheOld, "chyan"), nil, d.Cache) // everything into cache/
		if err != nil {
			return "", fmt.Errorf("migrate legacy cache dir: %w", err)
		}
		moved = append(moved, items...)
	}

	if len(moved) == 0 {
		return "", nil
	}
	sort.Strings(moved)
	return fmt.Sprintf("migrated legacy data into %s: %s", d.Root, strings.Join(moved, ", ")), nil
}

// moveEntries relocates entries of the src dir according to targets
// (entry name -> final destination path). Entries missing from targets go
// to the fallback dir when it is non-empty, otherwise they stay in place.
func moveEntries(src string, targets map[string]string, fallback string) ([]string, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var moved []string
	for _, e := range entries {
		dst, ok := targets[e.Name()]
		if !ok {
			if fallback == "" {
				continue
			}
			dst = filepath.Join(fallback, e.Name())
		}
		if err := movePath(filepath.Join(src, e.Name()), dst); err != nil {
			return moved, err
		}
		moved = append(moved, e.Name())
	}
	return moved, nil
}

// renamePath is os.Rename; a test seam for the cross-device fallback.
var renamePath = os.Rename

// movePath renames src to dst, merging directories and skipping files
// whose destination already exists. Falls back to copy+remove across
// filesystems.
func movePath(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if srcInfo.IsDir() {
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := movePath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		// Remove the source dir when every entry moved. Entries whose
		// destination already existed are intentionally left behind, so a
		// non-empty dir is a merge outcome, not a failure: reporting it would
		// abort startup for a harmless first-run conflict.
		if err := os.Remove(src); err != nil && !isDirNotEmpty(err) {
			return err
		}
		return nil
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil // never overwrite existing destination
	}
	if err := renamePath(src, dst); err == nil {
		return nil
	}
	// Cross-device fallback: copy then remove.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

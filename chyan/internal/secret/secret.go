// Package secret stores credentials as plaintext files (0600) under
// ~/.chyan/secret/<name> and resolves "secret:<name>" references (D20:
// no encryption, no master key; the directory itself is the backup unit).
package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Store reads and writes credential files inside one directory.
type Store struct {
	dir string
}

// New returns a Store rooted at dir (normally ~/.chyan/secret).
func New(dir string) *Store {
	return &Store{dir: dir}
}

// Dir returns the credential directory.
func (s *Store) Dir() string { return s.dir }

// nameRe matches a plain credential name: no path separators or dots
// traversal, so a name can never escape the secret directory.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (s *Store) validate(name string) error {
	if !nameRe.MatchString(name) || name == ".." || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("invalid secret name %q", name)
	}
	return nil
}

// Put writes value to secret/<name> with 0600 permissions. The write goes
// through a temp file and a rename, so a crash never leaves a half-written
// credential, and the new value is never briefly readable with a wider mode.
func (s *Store) Put(name, value string) error {
	if err := s.validate(name); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create secret dir: %w", err)
	}
	path := filepath.Join(s.dir, name)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("write secret %s: refusing to follow symlink", name)
	}
	tmp, err := os.CreateTemp(s.dir, ".secret-*") // 0600 by default
	if err != nil {
		return fmt.Errorf("write secret %s: %w", name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(value); err != nil {
		tmp.Close()
		return fmt.Errorf("write secret %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write secret %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write secret %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("write secret %s: %w", name, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write secret %s: %w", name, err)
	}
	return nil
}

// Get returns the credential value with surrounding whitespace trimmed.
func (s *Store) Get(name string) (string, error) {
	if err := s.validate(name); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("secret %q not found", name)
		}
		return "", fmt.Errorf("read secret %s: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// List returns the stored credential names (sorted by directory order).
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// secretRefRe matches one "secret:<name>" reference token.
var secretRefRe = regexp.MustCompile(`secret:([A-Za-z0-9][A-Za-z0-9._-]*)`)

// ResolveSecrets replaces every "secret:<name>" occurrence in raw with the
// stored credential value. Strings without references pass through
// unchanged; a reference to a missing secret is an error.
func (s *Store) ResolveSecrets(raw string) (string, error) {
	if !strings.Contains(raw, "secret:") {
		return raw, nil
	}
	var resolveErr error
	out := secretRefRe.ReplaceAllStringFunc(raw, func(ref string) string {
		name := strings.TrimPrefix(ref, "secret:")
		val, err := s.Get(name)
		if err != nil {
			if resolveErr == nil {
				resolveErr = err
			}
			return ref
		}
		return val
	})
	if resolveErr != nil {
		return "", resolveErr
	}
	return out, nil
}

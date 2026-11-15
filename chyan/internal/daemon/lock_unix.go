//go:build !windows

// lock_unix.go — one daemon per data root. The unix listener unlinks and
// rebinds <home>/chyan.sock, so without a lock two concurrent `chyan serve`
// processes would steal each other's socket and both write the store.
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireHomeLock takes an exclusive advisory lock on <home>/daemon.lock.
// The returned file holds the lock until closed (process exit releases it).
func acquireHomeLock(home string) (*os.File, error) {
	path := filepath.Join(home, "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: another instance is already serving %s", home)
	}
	return f, nil
}

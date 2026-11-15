//go:build !windows

package daemon

import "testing"

// TestHomeLockExcludesSecondInstance: one serve per data root; the lock is
// released when the holder closes it.
func TestHomeLockExcludesSecondInstance(t *testing.T) {
	home := t.TempDir()
	first, err := acquireHomeLock(home)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	second, err := acquireHomeLock(home)
	if err == nil {
		_ = second.Close()
		t.Fatal("second daemon acquired the same home")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release: %v", err)
	}
	third, err := acquireHomeLock(home)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = third.Close()
}

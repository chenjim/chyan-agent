// In-memory Store tests: shared contract suite plus in-memory-only cases.
package store

import (
	"context"
	"testing"
)

func openMemory(t *testing.T) (Store, func()) {
	t.Helper()
	s := NewMemory()
	return s, func() { _ = s.Close() }
}

func TestMemoryContract(t *testing.T) {
	runContractTests(t, openMemory)
}

func TestMemoryCloseIsNoop(t *testing.T) {
	s := NewMemory()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close must not invalidate the store (in-memory has no resources).
	if err := s.SetKV(context.Background(), "k", []byte("v")); err != nil {
		t.Fatalf("SetKV after Close: %v", err)
	}
}

// interrupt_test.go — SIGINT wiring for one interactive turn.
package main

import (
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestInterrupterFiresInstalledFunc(t *testing.T) {
	ir := &interrupter{}
	var calls atomic.Int32
	ir.set(func() { calls.Add(1) })
	ir.fire()
	ir.fire() // repeated Ctrl+C is harmless
	if got := calls.Load(); got != 2 {
		t.Fatalf("interrupt calls = %d, want 2", got)
	}
}

func TestInterrupterLatchesEarlySignal(t *testing.T) {
	ir := &interrupter{}
	var calls atomic.Int32
	// Ctrl+C before the agent is built: the interrupt must still apply once
	// the run exposes its function.
	ir.fire()
	ir.set(func() { calls.Add(1) })
	if got := calls.Load(); got != 1 {
		t.Fatalf("interrupt calls = %d, want 1", got)
	}
}

func TestInterrupterIgnoresSignalAfterStop(t *testing.T) {
	ir := &interrupter{}
	var calls atomic.Int32
	stop := ir.watchTurn()
	ir.set(func() { calls.Add(1) })
	stop()
	ir.fire()
	if got := calls.Load(); got != 0 {
		t.Fatalf("interrupt calls after stop = %d, want 0", got)
	}
}

func TestInterrupterDeliversSIGINT(t *testing.T) {
	ir := &interrupter{}
	fired := make(chan struct{}, 1)
	stop := ir.watchTurn()
	defer stop()
	ir.set(func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT did not reach the interrupter")
	}
}

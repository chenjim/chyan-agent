// interrupt.go — Ctrl+C handling shared by the REPL and one-shot runs.
package main

import (
	"os"
	"os/signal"
	"sync"
)

// interrupter connects SIGINT to the running turn's interrupt function. A
// signal that arrives before the agent exposes its function is latched and
// applied as soon as it appears, so Ctrl+C during startup is not lost.
type interrupter struct {
	mu    sync.Mutex
	fn    func()
	fired bool
	// done disables further delivery once the turn returned.
	done bool
}

// set installs the run's interrupt function (app.RunOptions.OnInterruptReady).
func (i *interrupter) set(fn func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.done {
		return
	}
	i.fn = fn
	if i.fired && fn != nil {
		fn()
	}
}

// fire interrupts the active run, if any.
func (i *interrupter) fire() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.done {
		return
	}
	i.fired = true
	if i.fn != nil {
		i.fn()
	}
}

// finish detaches the current run and ignores later signals.
func (i *interrupter) finish() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.done = true
	i.fn = nil
}

// watchTurn routes SIGINT to one turn: it returns a stop function that must
// be called when the turn ends. Output is untouched; the caller decides how
// to report an interrupt (REPL keeps the session, `run` exits 130).
func (i *interrupter) watchTurn() (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			select {
			case <-ch:
				i.fire()
			case <-done:
				return
			}
		}
	}()
	return func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
			i.finish()
		})
	}
}

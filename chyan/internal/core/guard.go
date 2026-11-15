// Tool-call guard: a hard, code-level stop for repeated identical failures.
// The article lesson is explicit — "do not retry a falsified direction" does
// not work as a prompt, because the model always finds a reason to retry.
// Counting identical signatures here blocks the call before execution.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// toolGuard counts failed invocations by signature within one user turn.
// A successful call clears its counter, so transient failures do not
// accumulate forever.
type toolGuard struct {
	limit  int
	failed map[string]int
}

// newToolGuard builds a guard; limit <= 0 disables blocking.
func newToolGuard(limit int) *toolGuard {
	return &toolGuard{limit: limit, failed: map[string]int{}}
}

// signature hashes tool name + canonical JSON arguments (encoding/json sorts
// map keys, so the same call always hashes the same).
func toolSignature(call ToolCall) string {
	raw, err := json.Marshal(call.Input)
	if err != nil {
		raw = []byte("{}")
	}
	sum := sha256.Sum256(append(append([]byte(call.Name), 0), raw...))
	return hex.EncodeToString(sum[:12])
}

// allow reports whether the call may run. It is false once the signature has
// already failed limit times in this turn.
func (g *toolGuard) allow(call ToolCall) (bool, int) {
	if g == nil || g.limit <= 0 {
		return true, 0
	}
	n := g.failed[toolSignature(call)]
	return n < g.limit, n
}

// fail records one failed attempt for the call signature.
func (g *toolGuard) fail(call ToolCall) {
	if g == nil || g.limit <= 0 {
		return
	}
	g.failed[toolSignature(call)]++
}

// ok clears the failure counter after a successful invocation.
func (g *toolGuard) ok(call ToolCall) {
	if g == nil || g.limit <= 0 {
		return
	}
	delete(g.failed, toolSignature(call))
}

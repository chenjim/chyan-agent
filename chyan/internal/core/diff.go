// Unified diff preview for approval tickets (architecture 4.2): compares the
// last-read content against the pending write — no git dependency. Minimal
// LCS-based line diff, no external deps.
package core

import (
	"fmt"
	"strings"
)

// diffContextLines is the unified diff context radius.
const diffContextLines = 3

// maxLCSCells bounds the LCS dynamic-programming table; larger middles fall
// back to a coarse whole-block replace so huge files never OOM the daemon.
const maxLCSCells = 1 << 20

// PreviewDiff renders the pending change for an approval ticket. File
// mutations get a unified diff; bash/web/mcp previews carry the command text
// instead (there is no file diff to show).
func PreviewDiff(action, resource string, before, after []byte) string {
	switch normalizeAction(action) {
	case "edit", "write", "delete":
	default:
		return strings.TrimSpace(resource)
	}
	if isBinary(before) || isBinary(after) {
		return "Binary content: diff preview unavailable"
	}
	ops := diffLines(splitLines(before), splitLines(after))
	hunks := buildHunks(ops, diffContextLines)
	if len(hunks) == 0 {
		return "" // no changes
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", resource, resource)
	for _, h := range hunks {
		b.WriteString(hunkHeader(ops, h[0], h[1]))
		for _, op := range ops[h[0]:h[1]] {
			b.WriteByte(op.kind)
			b.WriteString(op.line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// diffOp is one rendered diff line: ' ' equal, '-' removed, '+' added.
type diffOp struct {
	kind byte
	line string
}

// splitLines splits content into lines; a trailing newline does not yield an
// extra empty line.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := strings.Split(string(b), "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// isBinary reports NUL bytes in the leading chunk (text diff would be noise).
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}

// diffLines builds the full op sequence: common prefix/suffix pass through,
// the changed middle goes through LCS.
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []diffOp
	ops = appendOps(ops, ' ', a[:pre]...)
	ops = append(ops, diffMiddle(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	ops = appendOps(ops, ' ', a[len(a)-suf:]...)
	return ops
}

func appendOps(ops []diffOp, kind byte, lines ...string) []diffOp {
	for _, l := range lines {
		ops = append(ops, diffOp{kind: kind, line: l})
	}
	return ops
}

// diffMiddle diffs two non-common slices: LCS backtrack when small, coarse
// replace when the table would exceed maxLCSCells.
func diffMiddle(a, b []string) []diffOp {
	switch {
	case len(a) == 0:
		return appendOps(nil, '+', b...)
	case len(b) == 0:
		return appendOps(nil, '-', a...)
	case len(a)*len(b) > maxLCSCells:
		ops := appendOps(nil, '-', a...)
		return appendOps(ops, '+', b...)
	}
	// dp[i][j] = LCS length of a[i:] vs b[j:].
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = appendOps(ops, ' ', a[i])
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = appendOps(ops, '-', a[i])
			i++
		default:
			ops = appendOps(ops, '+', b[j])
			j++
		}
	}
	ops = appendOps(ops, '-', a[i:]...)
	ops = appendOps(ops, '+', b[j:]...)
	return ops
}

// buildHunks groups changed ops into hunks with ctx lines of context; change
// clusters separated by more than 2*ctx equal lines stay in separate hunks.
// Each returned pair is a [lo, hi) op range.
func buildHunks(ops []diffOp, ctx int) [][2]int {
	var hunks [][2]int
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i
		end := i + 1
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			gap := end
			for gap < len(ops) && ops[gap].kind == ' ' {
				gap++
			}
			if gap == len(ops) || gap-end > 2*ctx {
				break
			}
			end = gap
		}
		lo, hi := max(start-ctx, 0), min(end+ctx, len(ops))
		hunks = append(hunks, [2]int{lo, hi})
		i = end
	}
	return hunks
}

// hunkHeader renders the "@@ -old +new @@" line per unified diff convention
// (count omitted when 1; a zero count anchors at the preceding line).
func hunkHeader(ops []diffOp, lo, hi int) string {
	oldNo, newNo := 1, 1
	for _, op := range ops[:lo] {
		switch op.kind {
		case ' ':
			oldNo++
			newNo++
		case '-':
			oldNo++
		case '+':
			newNo++
		}
	}
	oldCount, newCount := 0, 0
	for _, op := range ops[lo:hi] {
		switch op.kind {
		case ' ':
			oldCount++
			newCount++
		case '-':
			oldCount++
		case '+':
			newCount++
		}
	}
	oldStart, newStart := oldNo, newNo
	if oldCount == 0 {
		oldStart--
	}
	if newCount == 0 {
		newStart--
	}
	return fmt.Sprintf("@@ -%s +%s @@\n", rangeSpec(oldStart, oldCount), rangeSpec(newStart, newCount))
}

func rangeSpec(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// Reference expansion (P1 chained recall): resolve an artifact pointer or a
// handoff note back to its full text. Only chyan-owned locations are readable
// this way; arbitrary files stay behind the normal read tool's rules.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"chyan.tech/chyan/internal/core"
	"chyan.tech/chyan/internal/store"
)

const (
	refArtifactPrefix = "artifact:"
	refHandoffPrefix  = "handoff:"
	// refDefaultLimit keeps one expand window inside the normal tool output
	// cap so the result cannot immediately re-trigger spilling.
	maxRefWindow = 20000
)

// ExpandRef implements core.RefReader. Supported refs:
//   - artifact:<path> (relative to the artifact root) or an absolute path
//     inside the artifact root;
//   - handoff:<session_id> (the latest handoff note of that session).
func (a *App) ExpandRef(ctx context.Context, ref string, offset, limit int) (core.RefContent, error) {
	if err := ctx.Err(); err != nil {
		return core.RefContent{}, err
	}
	switch {
	case strings.HasPrefix(ref, refArtifactPrefix):
		path := strings.TrimSpace(strings.TrimPrefix(ref, refArtifactPrefix))
		return a.expandArtifact(path, offset, limit)
	case strings.HasPrefix(ref, refHandoffPrefix):
		id := strings.TrimSpace(strings.TrimPrefix(ref, refHandoffPrefix))
		return a.expandHandoff(ctx, id)
	default:
		// Bare absolute paths are accepted when they point into the artifact
		// root (the "[artifact: /abs/path]" pointer shape).
		if filepath.IsAbs(ref) {
			return a.expandArtifact(ref, offset, limit)
		}
		return core.RefContent{}, fmt.Errorf("unsupported ref %q (use artifact:<path> or handoff:<session>)", ref)
	}
}

// artifactRoot is where oversized tool outputs are spilled.
func (a *App) artifactRoot() string { return filepath.Join(a.home, "artifacts") }

// expandArtifact reads one byte window of an artifact file. Paths must stay
// inside the artifact root; symlinks are resolved before the check.
func (a *App) expandArtifact(path string, offset, limit int) (core.RefContent, error) {
	root := a.artifactRoot()
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	if !withinRoot(root, path) {
		return core.RefContent{}, fmt.Errorf("artifact path escapes the artifact root")
	}
	info, err := os.Stat(path)
	if err != nil {
		return core.RefContent{}, fmt.Errorf("artifact %s: %w", path, err)
	}
	if info.IsDir() {
		return core.RefContent{}, fmt.Errorf("artifact %s is a directory", path)
	}
	if offset < 0 {
		offset = 0
	}
	if offset > int(info.Size()) {
		offset = int(info.Size())
	}
	if limit <= 0 {
		limit = maxRefWindow
	}
	if cap := core.ToolOutputMaxBytes(); limit > cap && cap > 0 {
		limit = cap
	}
	remain := int(info.Size()) - offset
	if remain > limit {
		remain = limit
	}
	f, err := os.Open(path)
	if err != nil {
		return core.RefContent{}, fmt.Errorf("artifact %s: %w", path, err)
	}
	defer f.Close()
	data := make([]byte, remain)
	n, err := io.ReadFull(io.NewSectionReader(f, int64(offset), int64(remain)), data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return core.RefContent{}, fmt.Errorf("artifact %s: %w", path, err)
	}
	text := trimInvalidTail(string(data[:n]))
	return core.RefContent{
		Text: text, Total: int(info.Size()), Next: artifactRefsIn(text),
	}, nil
}

// expandHandoff returns a session's persisted handoff note (small enough to
// return whole; the offset/limit window does not apply).
func (a *App) expandHandoff(ctx context.Context, sessionID string) (core.RefContent, error) {
	if sessionID == "" {
		return core.RefContent{}, fmt.Errorf("handoff ref needs a session id")
	}
	raw, err := a.store.GetKV(ctx, "handoff:"+sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return core.RefContent{}, fmt.Errorf("no handoff note for session %s", sessionID)
		}
		return core.RefContent{}, fmt.Errorf("handoff %s: %w", sessionID, err)
	}
	text := string(raw)
	return core.RefContent{
		Text: text, Total: len(raw), Next: artifactRefsIn(text),
	}, nil
}

// withinRoot reports whether path lies inside root (both cleaned absolute
// paths). Symlinks are resolved when possible so an artifact symlink cannot
// point outside.
func withinRoot(root, path string) bool {
	root = filepath.Clean(root)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err == nil {
		root = resolvedRoot
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else {
		path = filepath.Clean(path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// artifactRefsIn collects "[artifact: ...]" pointers embedded in expanded
// text so the model can chain to the next spilled output.
func artifactRefsIn(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "[artifact: ") {
			continue
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(line, "[artifact: "), "]")
		if i := strings.Index(ref, " sha256:"); i >= 0 {
			ref = ref[:i]
		}
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, refArtifactPrefix+ref)
		if len(out) >= 5 {
			break
		}
	}
	return out
}

// trimInvalidTail drops an incomplete trailing UTF-8 sequence produced by a
// byte-window cut, keeping the expanded text well-formed.
func trimInvalidTail(s string) string {
	for i := 0; i < 3 && len(s) > 0; i++ {
		if utf8.ValidString(s) {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

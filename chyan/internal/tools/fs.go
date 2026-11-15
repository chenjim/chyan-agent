// Package tools implements the built-in file and bash tools (core.Tool):
// work_dir confinement, sensitive-file blacklist, write conflict checks,
// output truncation and dangerous-command interception.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"chyan.tech/chyan/internal/core"
)

const (
	maxFileBytes   = 10 << 20 // single file read/write cap (appendix A)
	maxListResults = 30
	maxGrepResults = 10
	grepSnippets   = 20 // per-file line matches reported
)

// sensitiveFiles are exact file names that must never be read, written or
// deleted (legacy blacklist from the earlier Python implementation).
var sensitiveFiles = map[string]bool{
	"config.toml":         true,
	"config.example.toml": true,
	".env":                true,
	".env.local":          true,
}

// sensitiveRes are path patterns additionally refused (keys, certs and
// credential files must go through the secret store, not file tools).
// Matched against the absolute path; (|/) boundaries anchor per segment.
var sensitiveRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(^|/)\.ssh/`),
	regexp.MustCompile(`(?i)(^|/)\.gnupg/`),
	regexp.MustCompile(`(?i)(^|/)\.git/(config|credentials)$`),
	regexp.MustCompile(`(?i)(^|/)(id_(rsa|dsa|ecdsa|ed25519)|[^/]+\.(pem|key|p12|pfx|keystore))$`),
	regexp.MustCompile(`(?i)(^|/)(credentials|\.netrc|\.npmrc|\.pypirc)$`),
	regexp.MustCompile(`(?i)(^|/)\.env\.[^/]+$`),
	regexp.MustCompile(`(?i)(^|/)[^/]*secret[^/]*$`),
}

// excludeDirs are skipped when walking (legacy EXCLUDE_DIRS + dot dirs).
var excludeDirs = map[string]bool{
	".venv": true, "venv": true, ".git": true, "__pycache__": true,
	"node_modules": true, ".idea": true, ".vscode": true,
}

// conflictError is returned by edit/write when the file changed since the
// last read. CurrentSHA256 lets the caller re-read before retrying.
type conflictError struct {
	msg           string
	CurrentSHA256 string
}

func (e *conflictError) Error() string { return e.msg }

// FSTool implements core.Tool for one file operation. NewReadTool and the
// sibling constructors return tools ready for registration in the tool
// loop; all share one work_dir root and output cap.
type FSTool struct {
	kind    string // read|write|edit|list|search|grep|delete
	workDir string // absolute, cleaned; all paths must resolve inside it
	maxOut  int    // tool output cap in bytes (tool_output_max_bytes)
}

// NewReadTool and friends build one file tool rooted at workDir. maxOut
// is the unified tool output cap; zero falls back to 20000 bytes.
func NewReadTool(workDir string, maxOut int) *FSTool   { return newFS("read", workDir, maxOut) }
func NewWriteTool(workDir string, maxOut int) *FSTool  { return newFS("write", workDir, maxOut) }
func NewEditTool(workDir string, maxOut int) *FSTool   { return newFS("edit", workDir, maxOut) }
func NewListTool(workDir string, maxOut int) *FSTool   { return newFS("list", workDir, maxOut) }
func NewSearchTool(workDir string, maxOut int) *FSTool { return newFS("search", workDir, maxOut) }
func NewGrepTool(workDir string, maxOut int) *FSTool   { return newFS("grep", workDir, maxOut) }
func NewDeleteTool(workDir string, maxOut int) *FSTool { return newFS("delete", workDir, maxOut) }

func newFS(kind, workDir string, maxOut int) *FSTool {
	if maxOut <= 0 {
		maxOut = 20000
	}
	return &FSTool{kind: kind, workDir: filepath.Clean(workDir), maxOut: maxOut}
}

// Name implements core.Tool.
func (t *FSTool) Name() string { return t.kind }

// Schema implements core.Tool; argument shapes match the legacy tools.
func (t *FSTool) Schema() map[string]any {
	str := map[string]any{"type": "string"}
	switch t.kind {
	case "read":
		return obj(map[string]any{
			"path":   str,
			"offset": map[string]any{"type": "integer", "description": "1-based start line"},
			"limit":  map[string]any{"type": "integer", "description": "max lines to return"},
		}, "path")
	case "write":
		return obj(map[string]any{
			"path":    str,
			"content": str,
			"mode":    map[string]any{"type": "string", "enum": []any{"write", "append"}},
			// optional pre-write conflict check, as in edit
			"expected_mtime":  map[string]any{"type": "string"},
			"expected_sha256": map[string]any{"type": "string"},
		}, "path", "content")
	case "edit":
		return obj(map[string]any{
			"path":            str,
			"old_string":      str,
			"new_string":      str,
			"replace_all":     map[string]any{"type": "boolean"},
			"expected_mtime":  map[string]any{"type": "string", "description": "RFC3339Nano of last read"},
			"expected_sha256": map[string]any{"type": "string", "description": "sha256 hex of last read"},
		}, "path", "old_string", "new_string")
	case "list":
		return obj(map[string]any{
			"path":  str,
			"depth": map[string]any{"type": "integer"},
		})
	case "search":
		return obj(map[string]any{
			"path":    str,
			"pattern": str,
		}, "path", "pattern")
	case "grep":
		return obj(map[string]any{
			"path":    str,
			"pattern": str,
			"include": str,
		}, "path", "pattern")
	case "delete":
		return obj(map[string]any{"path": str}, "path")
	}
	return map[string]any{"type": "object"}
}

func obj(props map[string]any, required ...string) map[string]any {
	if len(required) == 0 {
		return map[string]any{"type": "object", "properties": props}
	}
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return map[string]any{"type": "object", "properties": props, "required": req}
}

// Execute implements core.Tool.
func (t *FSTool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	var res map[string]any
	switch t.kind {
	case "read":
		if img, ok := t.readImage(call.Input); ok {
			return img
		}
		res = t.read(call.Input)
	case "write":
		res = t.write(call.Input)
	case "edit":
		res = t.edit(call.Input)
	case "list":
		res = t.list(call.Input)
	case "search":
		res = t.search(call.Input)
	case "grep":
		res = t.grep(call.Input)
	case "delete":
		res = t.delete(call.Input)
	default:
		res = map[string]any{"success": false, "error": "unknown tool " + t.kind}
	}
	return marshalResult(res, t.maxOut)
}

// envelopeAllowance is the fixed JSON metadata budget on top of maxOut.
const envelopeAllowance = 512

// marshalResult serializes the payload as compact JSON and applies the
// unified tool_output_max_bytes truncation (core.ToolResult.Truncated).
// Content fields are pre-capped at maxOut; when the JSON payload as a
// whole still overshoots maxOut+envelopeAllowance it is replaced by a
// valid-JSON preview envelope (legacy _cap_output behavior).
func marshalResult(res map[string]any, maxOut int) core.ToolResult {
	truncated, _ := res["output_truncated"].(bool)
	data, jerr := json.Marshal(res)
	if jerr != nil {
		return core.ToolResult{Content: fmt.Sprintf("%v", res)}
	}
	if len(data) <= maxOut+envelopeAllowance {
		return core.ToolResult{Content: string(data), Truncated: truncated}
	}
	preview := string(data[:cutRune(data, maxOut)])
	env := map[string]any{
		"success":          res["success"],
		"truncated_output": true,
		"note":             fmt.Sprintf("output exceeded %d bytes, truncated", maxOut),
		"preview":          preview,
	}
	edata, err := json.Marshal(env)
	if err != nil {
		return core.ToolResult{Content: preview, Truncated: true}
	}
	return core.ToolResult{Content: string(edata), Truncated: true}
}

// cutRune returns a byte length <= max whose cut is at a rune boundary.
func cutRune(b []byte, max int) int {
	cut := max
	for cut > 0 && !utf8Start(b[cut]) {
		cut--
	}
	return cut
}

// utf8Start reports whether b[i] begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// strArg reads a required string argument.
func strArg(in map[string]any, key string) (string, bool) {
	v, ok := in[key].(string)
	return v, ok
}

// intArg reads an optional integer argument (absent -> def).
func intArg(in map[string]any, key string, def int) int {
	if v, ok := in[key].(float64); ok {
		return int(v)
	}
	if v, ok := in[key].(int); ok {
		return v
	}
	return def
}

// resolvePath resolves p inside this tool's workDir.
func (t *FSTool) resolvePath(p string) (string, error) {
	return resolveIn(t.workDir, p)
}

// resolveIn resolves p against root and guarantees the result stays
// inside root, including through symlinked components. A dangling link is
// refused: writing through it would create a file wherever it points.
func resolveIn(root, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path is required")
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, p)
	}
	full = filepath.Clean(full)
	if !within(root, full) {
		return "", fmt.Errorf("path escapes work_dir %s: %s", root, p)
	}
	baseReal := root
	if res, err := filepath.EvalSymlinks(root); err == nil {
		baseReal = res
	}
	// Resolve the deepest existing ancestor: any symlinked component on
	// the way must still land inside root (final component may not exist
	// yet for write).
	for cur := filepath.Dir(full); within(root, cur); cur = filepath.Dir(cur) {
		if res, err := filepath.EvalSymlinks(cur); err == nil {
			if !within(baseReal, res) {
				return "", fmt.Errorf("path escapes work_dir via symlink: %s", p)
			}
			break
		}
	}
	if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
		res, rerr := evalLink(full)
		if rerr != nil {
			return "", fmt.Errorf("cannot verify symlink %s: %w", p, rerr)
		}
		if !within(baseReal, res) {
			return "", fmt.Errorf("path escapes work_dir via symlink: %s", p)
		}
	}
	return full, nil
}

// maxLinkHops bounds symlink chain resolution.
const maxLinkHops = 16

// evalLink follows a symlink chain and returns the absolute target, resolving
// dangling links lexically (filepath.EvalSymlinks errors on those). A missing
// or looping target is an error: the caller must not act on an unverifiable link.
func evalLink(link string) (string, error) {
	cur := link
	for i := 0; i < maxLinkHops; i++ {
		info, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			// Lstat follows symlinks in intermediate components. Resolve the
			// complete existing target so an alias cannot hide an escaping
			// directory link behind a lexically internal path.
			return filepath.EvalSymlinks(cur)
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		cur = filepath.Clean(target)
	}
	return "", fmt.Errorf("too many symlink levels")
}

// within reports whether p is root or inside root (lexically).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkSensitive refuses blacklisted paths (read/write/edit/delete). It
// checks both the alias the caller supplied and the symlink-resolved target,
// so a benign name pointing at .env is still refused.
func checkSensitive(p string) error {
	if err := checkSensitivePath(p); err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
		return checkSensitivePath(real)
	}
	return nil
}

// checkSensitivePath matches one concrete path against the blacklist.
func checkSensitivePath(p string) error {
	name := filepath.Base(p)
	if sensitiveFiles[name] {
		return fmt.Errorf("refusing to operate on sensitive file: %s", name)
	}
	rel := strings.ReplaceAll(p, "\\", "/")
	for _, re := range sensitiveRes {
		if re.MatchString(rel) {
			return fmt.Errorf("refusing to operate on sensitive file: %s", name)
		}
	}
	return nil
}

// checkConflict verifies the file has not changed since the last read:
// expectedSHA256 (hex) or expectedMtime (RFC3339Nano) from the read
// result. Both provided must match.
func checkConflict(p, expectedMtime, expectedSHA string) error {
	if expectedMtime == "" && expectedSHA == "" {
		return nil
	}
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("conflict check failed: %w", err)
	}
	cur := fileSHA(p)
	if expectedSHA != "" && !strings.EqualFold(expectedSHA, cur) {
		return &conflictError{
			msg:           fmt.Sprintf("file changed since last read (expected sha256 %s, current %s); re-read before editing", expectedSHA, cur),
			CurrentSHA256: cur,
		}
	}
	if expectedMtime != "" {
		want, err := time.Parse(time.RFC3339Nano, expectedMtime)
		if err != nil {
			return fmt.Errorf("invalid expected_mtime: %w", err)
		}
		// Compare at nanosecond precision; stat mtime is preserved as-is.
		if !info.ModTime().Equal(want) {
			return &conflictError{
				msg:           fmt.Sprintf("file changed since last read (expected mtime %s, current %s); re-read before editing", expectedMtime, info.ModTime().Format(time.RFC3339Nano)),
				CurrentSHA256: cur,
			}
		}
	}
	return nil
}

// fileSHA returns the hex sha256 of the file, or "" when unreadable.
func fileSHA(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ---- read ----

// readImage returns image files as multimodal tool output (FR-10). ok is
// false for non-image files, so the caller falls back to the text reader.
func (t *FSTool) readImage(in map[string]any) (core.ToolResult, bool) {
	p, ok := strArg(in, "path")
	if !ok {
		return core.ToolResult{}, false
	}
	full, err := t.resolvePath(p)
	if err != nil {
		return core.ToolResult{}, false
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return core.ToolResult{}, false
	}
	// Cap the read at limit+1 so an oversized image is never fully loaded;
	// the extra byte only tells us the file exceeds the limit.
	f, err := os.Open(full)
	if err != nil {
		return core.ToolResult{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, core.MaxImageBytes+1))
	_ = f.Close()
	if err != nil {
		return core.ToolResult{}, false
	}
	mime := core.DetectImageMIME(data)
	if mime == "" {
		return core.ToolResult{}, false
	}
	if len(data) > core.MaxImageBytes {
		size := info.Size()
		if size < int64(len(data)) {
			size = int64(len(data))
		}
		return core.ToolResult{Err: fmt.Errorf(
			"image too large: %d bytes (limit %d)", size, core.MaxImageBytes)}, true
	}
	if err := checkSensitive(full); err != nil {
		return core.ToolResult{Err: err}, true
	}
	meta, _ := json.Marshal(map[string]any{
		"success": true, "path": full, "mime": mime,
		"bytes": len(data), "sha256": fileSHA(full),
		"note": "image returned to the model",
	})
	return core.ToolResult{Content: string(meta),
		Images: []core.ToolImage{{MIME: mime, Data: data}}}, true
}

func (t *FSTool) read(in map[string]any) map[string]any {
	p, ok := strArg(in, "path")
	if !ok {
		return fail("path is required")
	}
	full, err := t.resolvePath(p)
	if err != nil {
		return fail(err.Error())
	}
	if err := checkSensitive(full); err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(full)
	if err != nil {
		return fail("file not found: " + p)
	}
	if !info.IsDir() && info.Size() > maxFileBytes {
		return fail(fmt.Sprintf("file too large: %d bytes (limit %d); use offset/limit", info.Size(), maxFileBytes))
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return fail("read failed: " + err.Error())
	}
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // trailing newline artifact
	}
	total := len(lines)
	offset, limit := intArg(in, "offset", 0), intArg(in, "limit", 0)
	out := map[string]any{
		"success":     true,
		"path":        full,
		"total_lines": total,
		"sha256":      fileSHA(full),
		"mtime":       info.ModTime().Format(time.RFC3339Nano),
	}
	start, end := 0, total
	if offset > 0 {
		start = offset - 1
		if start > total {
			start = total
		}
	}
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	out["start_line"] = start + 1
	out["end_line"] = end
	out["content"] = strings.Join(lines[start:end], "")
	out["truncated"] = start > 0 || end < total
	capField(out, "content", t.maxOut)
	return out
}

// ---- write ----

func (t *FSTool) write(in map[string]any) map[string]any {
	p, ok := strArg(in, "path")
	if !ok {
		return fail("path is required")
	}
	content, ok := strArg(in, "content")
	if !ok {
		return fail("content is required")
	}
	full, err := t.resolvePath(p)
	if err != nil {
		return fail(err.Error())
	}
	if err := checkSensitive(full); err != nil {
		return fail(err.Error())
	}
	mode, _ := strArg(in, "mode")
	if mode == "" {
		mode = "write"
	}
	if mode != "write" && mode != "append" {
		return fail("mode must be write or append")
	}
	if int64(len(content)) > maxFileBytes {
		return fail(fmt.Sprintf("content too large: %d bytes (limit %d)", len(content), maxFileBytes))
	}
	perm := os.FileMode(0o644)
	if info, err := os.Stat(full); err == nil {
		perm = info.Mode().Perm()
		if mode == "append" && info.Size()+int64(len(content)) > maxFileBytes {
			return fail(fmt.Sprintf("resulting file too large (limit %d bytes)", maxFileBytes))
		}
		if err := checkConflict(full, str(in, "expected_mtime"), str(in, "expected_sha256")); err != nil {
			return conflictFail(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fail("write failed: " + err.Error())
	}
	if mode == "append" {
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
		if err != nil {
			return fail("write failed: " + err.Error())
		}
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			return fail("write failed: " + err.Error())
		}
		if err := f.Close(); err != nil {
			return fail("write failed: " + err.Error())
		}
		return map[string]any{"success": true, "path": full, "mode": mode, "bytes": len(content)}
	}
	if err := writeFileAtomic(full, []byte(content), perm); err != nil {
		return fail("write failed: " + err.Error())
	}
	return map[string]any{"success": true, "path": full, "mode": mode, "bytes": len(content)}
}

// writeFileAtomic replaces path through a temp file in the same directory and
// a rename, so a crash or disk-full never leaves a half-written file behind.
// A final symlink is preserved: the atomic replace applies to its target.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		path = resolved
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ---- edit ----

func (t *FSTool) edit(in map[string]any) map[string]any {
	p, ok := strArg(in, "path")
	if !ok {
		return fail("path is required")
	}
	oldS, ok := strArg(in, "old_string")
	if !ok || oldS == "" {
		return fail("old_string is required and must not be empty")
	}
	newS, _ := strArg(in, "new_string")
	full, err := t.resolvePath(p)
	if err != nil {
		return fail(err.Error())
	}
	if err := checkSensitive(full); err != nil {
		return fail(err.Error())
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return fail("file not found: " + p)
	}
	perm := os.FileMode(0o644)
	if info, serr := os.Stat(full); serr == nil {
		perm = info.Mode().Perm()
	}
	if err := checkConflict(full, str(in, "expected_mtime"), str(in, "expected_sha256")); err != nil {
		return conflictFail(err)
	}
	text := string(data)
	count := strings.Count(text, oldS)
	if count == 0 {
		return fail("old_string not found; re-read the file and retry with exact text")
	}
	replaceAll, _ := in["replace_all"].(bool)
	if count > 1 && !replaceAll {
		return fail(fmt.Sprintf("old_string matches %d places; add context to make it unique or set replace_all", count))
	}
	var out string
	if replaceAll {
		out = strings.ReplaceAll(text, oldS, newS)
	} else {
		out = strings.Replace(text, oldS, newS, 1)
	}
	if int64(len(out)) > maxFileBytes {
		return fail(fmt.Sprintf("resulting file too large (limit %d bytes)", maxFileBytes))
	}
	if err := writeFileAtomic(full, []byte(out), perm); err != nil {
		return fail("write failed: " + err.Error())
	}
	n := 1
	if replaceAll {
		n = count
	}
	return map[string]any{"success": true, "path": full, "replacements": n}
}

// ---- list ----

func (t *FSTool) list(in map[string]any) map[string]any {
	p, _ := strArg(in, "path")
	if p == "" {
		p = "."
	}
	full, err := t.resolvePath(p)
	if err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(full)
	if err != nil || !info.IsDir() {
		return fail("not a directory: " + p)
	}
	depth := intArg(in, "depth", 1)
	if depth < 1 {
		depth = 1
	}
	entries := []any{}
	walkList(full, 1, depth, &entries)
	out := map[string]any{"success": true, "path": full, "count": len(entries), "entries": entries}
	if len(entries) >= maxListResults {
		out["truncated"] = true
	}
	return out
}

func walkList(dir string, level, depth int, out *[]any) {
	children, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].IsDir() != children[j].IsDir() {
			return children[i].IsDir()
		}
		return strings.ToLower(children[i].Name()) < strings.ToLower(children[j].Name())
	})
	for _, c := range children {
		if len(*out) >= maxListResults {
			return
		}
		name := c.Name()
		if strings.HasPrefix(name, ".") || excludeDirs[name] {
			continue
		}
		item := map[string]any{"name": name, "type": "file"}
		if c.IsDir() {
			item["type"] = "dir"
		} else if info, err := c.Info(); err == nil {
			item["size"] = info.Size()
		}
		*out = append(*out, item)
		if c.IsDir() && level < depth {
			walkList(filepath.Join(dir, name), level+1, depth, out)
		}
	}
}

// ---- search (glob by file name) ----

func (t *FSTool) search(in map[string]any) map[string]any {
	p, ok := strArg(in, "path")
	if !ok {
		return fail("path is required")
	}
	pattern, ok := strArg(in, "pattern")
	if !ok || pattern == "" {
		return fail("pattern is required")
	}
	full, err := t.resolvePath(p)
	if err != nil {
		return fail(err.Error())
	}
	if _, err := os.Stat(full); err != nil {
		return fail("path not found: " + p)
	}
	var results []string
	_ = filepath.WalkDir(full, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		rel, rerr := filepath.Rel(full, f)
		if rerr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") || excludeDirs[part] {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if !d.IsDir() {
			if ok, _ := filepath.Match(pattern, d.Name()); ok {
				results = append(results, filepath.ToSlash(rel))
			}
		}
		if len(results) >= maxListResults {
			return fs.SkipAll
		}
		return nil
	})
	out := map[string]any{"success": true, "path": full, "pattern": pattern, "count": len(results)}
	if results == nil {
		results = []string{}
	}
	out["results"] = results
	if len(results) >= maxListResults {
		out["truncated"] = true
	}
	return out
}

// ---- grep ----

var grepLineRe = regexp.MustCompile(`\r?\n`)

func (t *FSTool) grep(in map[string]any) map[string]any {
	p, ok := strArg(in, "path")
	if !ok {
		return fail("path is required")
	}
	pattern, ok := strArg(in, "pattern")
	if !ok || pattern == "" {
		return fail("pattern is required")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fail("invalid pattern: " + err.Error())
	}
	include, _ := strArg(in, "include")
	full, err := t.resolvePath(p)
	if err != nil {
		return fail(err.Error())
	}
	if err := checkSensitive(full); err != nil {
		return fail(err.Error())
	}
	info, err := os.Stat(full)
	if err != nil {
		return fail("path not found: " + p)
	}
	files := []string{}
	if !info.IsDir() {
		files = append(files, full)
	} else {
		_ = filepath.WalkDir(full, func(f string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return nil
			}
			rel, _ := filepath.Rel(full, f)
			if rel != "." {
				for _, part := range strings.Split(rel, string(filepath.Separator)) {
					if strings.HasPrefix(part, ".") || excludeDirs[part] {
						if d.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
				}
			}
			if d.IsDir() {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil // never read through a link: the target may be outside
			}
			if include != "" {
				if ok, _ := filepath.Match(include, d.Name()); !ok {
					return nil
				}
			}
			if checkSensitive(f) != nil {
				return nil // never grep sensitive files
			}
			files = append(files, f)
			if len(files) > maxGrepResults*10 {
				return fs.SkipAll
			}
			return nil
		})
	}
	type matchFile struct {
		File  string           `json:"file"`
		Count int              `json:"count"`
		Lines []map[string]any `json:"lines"`
	}
	results := []matchFile{}
	for _, f := range files {
		if len(results) >= maxGrepResults {
			break
		}
		data, rerr := os.ReadFile(f)
		if rerr != nil || int64(len(data)) > maxFileBytes {
			continue
		}
		lines := grepLineRe.Split(string(data), -1)
		hits := []map[string]any{}
		count := 0
		for i, line := range lines {
			if re.MatchString(line) {
				count++
				if len(hits) < grepSnippets {
					snippet := line
					if len(snippet) > 120 {
						snippet = snippet[:120]
					}
					hits = append(hits, map[string]any{"line": i + 1, "text": strings.TrimSpace(snippet)})
				}
			}
		}
		if count > 0 {
			rel, _ := filepath.Rel(t.workDir, f)
			results = append(results, matchFile{File: filepath.ToSlash(rel), Count: count, Lines: hits})
		}
	}
	out := map[string]any{"success": true, "path": full, "pattern": pattern, "count": len(results)}
	resAny := make([]any, len(results))
	for i, r := range results {
		resAny[i] = r
	}
	out["results"] = resAny
	if len(results) >= maxGrepResults {
		out["truncated"] = true
	}
	return out
}

// ---- delete ----

func (t *FSTool) delete(in map[string]any) map[string]any {
	p, ok := strArg(in, "path")
	if !ok {
		return fail("path is required")
	}
	full, err := t.resolvePath(p)
	if err != nil {
		return fail(err.Error())
	}
	if err := checkSensitive(full); err != nil {
		return fail(err.Error())
	}
	if _, err := os.Lstat(full); err != nil {
		return fail("path not found: " + p)
	}
	info, err := os.Stat(full)
	if err != nil {
		return fail("path not found: " + p)
	}
	if info.IsDir() {
		if err := os.Remove(full); err != nil { // only empty dirs
			return fail("delete failed (directory must be empty): " + err.Error())
		}
	} else if err := os.Remove(full); err != nil {
		return fail("delete failed: " + err.Error())
	}
	return map[string]any{"success": true, "path": full, "action": "deleted"}
}

// ---- helpers ----

func fail(msg string) map[string]any {
	return map[string]any{"success": false, "error": msg}
}

// conflictFail converts a conflict (or other) error into a failure map
// carrying the current hash hint when available.
func conflictFail(err error) map[string]any {
	var ce *conflictError
	if errors.As(err, &ce) {
		return map[string]any{
			"success":        false,
			"error":          ce.msg,
			"current_sha256": ce.CurrentSHA256,
			"conflict":       true,
		}
	}
	return fail(err.Error())
}

// str reads an optional string argument.
func str(in map[string]any, key string) string {
	v, _ := in[key].(string)
	return v
}

// capField truncates string field key to maxOut bytes (rune-safe) and
// marks the payload with output_truncated so callers can tell.
func capField(m map[string]any, key string, maxOut int) {
	v, ok := m[key].(string)
	if !ok || len(v) <= maxOut {
		return
	}
	m[key] = v[:cutRune([]byte(v), maxOut)]
	m["output_truncated"] = true
}

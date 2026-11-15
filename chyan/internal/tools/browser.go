// browser.go: playwright subprocess bridge tool (core.Tool) driving a
// bundled node script over JSON args.
package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"chyan.tech/chyan/internal/core"
)

//go:embed browser_bridge.js
var browserBridgeJS string

const browserScriptName = "browser_bridge.js"

// browserActions is the set of supported bridge actions.
var browserActions = map[string]bool{
	"open": true, "click": true, "type": true, "text": true,
	"screenshot": true, "close": true,
}

// BrowserTool implements core.Tool: drive chromium through the bundled
// playwright node bridge. The bridge is a one-shot script, so every action
// is self-contained (launch, act, close); the "close" action is accepted
// for schema completeness and is a no-op.
type BrowserTool struct {
	nodePath string        // node binary; empty = resolve "node" from PATH
	cacheDir string        // where screenshots and the temp bridge script live
	timeout  time.Duration // per-action timeout
	maxOut   int           // tool output cap in bytes
}

// NewBrowserTool builds the browser tool. nodePath empty resolves "node"
// from PATH; cacheDir empty falls back to ~/.chyan/cache (D12: private data
// lives under ~/.chyan); timeout <= 0 falls back to 60s; maxOut <= 0 falls
// back to 20000 bytes.
func NewBrowserTool(nodePath, cacheDir string, timeout time.Duration, maxOut int) *BrowserTool {
	if cacheDir == "" {
		if d, err := os.UserHomeDir(); err == nil {
			cacheDir = filepath.Join(d, ".chyan", "cache")
		}
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if maxOut <= 0 {
		maxOut = 20000
	}
	return &BrowserTool{nodePath: nodePath, cacheDir: cacheDir, timeout: timeout, maxOut: maxOut}
}

// Name implements core.Tool.
func (t *BrowserTool) Name() string { return "browser" }

// Schema implements core.Tool.
func (t *BrowserTool) Schema() map[string]any {
	return obj(map[string]any{
		"action":   map[string]any{"type": "string", "enum": []any{"open", "click", "type", "text", "screenshot", "close"}, "description": "what to do"},
		"url":      map[string]any{"type": "string", "description": "page URL for open/text/screenshot"},
		"selector": map[string]any{"type": "string", "description": "CSS selector for click/type"},
		"text":     map[string]any{"type": "string", "description": "text to type for type"},
		"path":     map[string]any{"type": "string", "description": "screenshot output path (absolute)"},
		"timeout":  map[string]any{"type": "integer", "description": "timeout seconds; default from config"},
	}, "action")
}

// Execute implements core.Tool. A successful screenshot also returns the
// image bytes so the model can look at it directly (FR-10 screenshot
// feedback); the JSON result still carries the path for audit.
func (t *BrowserTool) Execute(ctx context.Context, call core.ToolCall) core.ToolResult {
	res := t.run(ctx, call.Input)
	out := marshalResult(res, t.maxOut)
	if str(call.Input, "action") != "screenshot" {
		return out
	}
	if ok, _ := res["success"].(bool); !ok {
		return out
	}
	path := str(call.Input, "path")
	data, err := os.ReadFile(path)
	if err != nil || len(data) > core.MaxImageBytes {
		return out
	}
	if mime := core.DetectImageMIME(data); mime != "" {
		out.Images = []core.ToolImage{{MIME: mime, Data: data}}
	}
	return out
}

func (t *BrowserTool) run(ctx context.Context, in map[string]any) map[string]any {
	action := str(in, "action")
	if action == "" {
		return fail("action is required")
	}
	if !browserActions[action] {
		return fail("unknown action " + action + ": want open|click|type|text|screenshot|close")
	}
	args := map[string]any{"action": action}
	for _, k := range []string{"url", "selector", "text", "path"} {
		if v := str(in, k); v != "" {
			args[k] = v
		}
	}
	switch action {
	case "open", "text", "screenshot":
		if args["url"] == nil {
			return fail("url is required for action " + action)
		}
		// Only http(s) pages may reach the bridge: file://, data: and
		// javascript: URLs would let the model read local files or run code.
		if err := checkBrowserURL(args["url"].(string)); err != nil {
			return fail(err.Error())
		}
	case "click":
		if args["selector"] == nil {
			return fail("selector is required for action click")
		}
	case "type":
		if args["selector"] == nil {
			return fail("selector is required for action type")
		}
	}
	if action == "screenshot" {
		if args["path"] == nil {
			return fail("path is required for action screenshot")
		}
		// screenshots must land in cacheDir so the model cannot write
		// arbitrary locations through the bridge
		if err := t.checkScreenshotPath(args["path"].(string)); err != nil {
			return fail(err.Error())
		}
	}
	// the bridge is one-shot (launch/act/close per call); explicit close
	// is still accepted so the model can express intent
	if action == "close" {
		return map[string]any{"success": true, "action": action}
	}

	node := t.nodePath
	if node == "" {
		node = "node"
	}
	if _, err := exec.LookPath(node); err != nil {
		return fail("browser tool requires node + playwright: node not found in PATH")
	}

	script, err := t.writeScript()
	if err != nil {
		return fail("write bridge script: " + err.Error())
	}
	defer os.Remove(script)

	payload, err := json.Marshal(args)
	if err != nil {
		return fail("marshal args: " + err.Error())
	}

	timeout := t.timeout
	if secs := intArg(in, "timeout", 0); secs > 0 {
		timeout = time.Duration(secs) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var buf cappedBuffer
	buf.max = t.maxOut * 32 // bound worst-case collection; marshalResult caps later
	cmd := exec.CommandContext(rctx, node, script, string(payload))
	// exec copies stdout and stderr from two goroutines concurrently: they
	// must not share one bytes.Buffer (data race corrupts it and crashes the
	// process on large output). cappedBuffer is mutex-guarded and bounded.
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// a shim or node child holding the pipe must not stall Run past the
	// deadline
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	output := buf.String()

	if rctx.Err() == context.DeadlineExceeded {
		return map[string]any{
			"success":   false,
			"error":     fmt.Sprintf("browser action timed out after %s", timeout),
			"timed_out": true,
			"output":    output,
		}
	}
	if ctx.Err() != nil {
		return map[string]any{"success": false, "error": "browser action canceled"}
	}
	res := parseBridgeJSON([]byte(output))
	if runErr != nil {
		if msg, _ := res["error"].(string); msg == "" {
			res["error"] = "bridge failed: " + runErr.Error()
		}
		res["success"] = false
		if raw := output; raw != "" {
			res["output"] = raw
			capField(res, "output", t.maxOut)
		}
		return res
	}
	capField(res, "text", t.maxOut)
	capField(res, "output", t.maxOut)
	return res
}

// checkBrowserURL admits only http/https URLs with a host.
func checkBrowserURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("url scheme %q not allowed: only http/https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("url must include a host")
	}
	return nil
}

// checkScreenshotPath confines screenshot output to cacheDir.
func (t *BrowserTool) checkScreenshotPath(p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("screenshot path must be absolute")
	}
	if t.cacheDir == "" {
		return errors.New("no cache dir configured for screenshots")
	}
	root := filepath.Clean(t.cacheDir)
	target := filepath.Clean(p)
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return fmt.Errorf("screenshot path must be inside %s", root)
	}
	return nil
}

// writeScript materializes the embedded bridge next to the cache dir.
func (t *BrowserTool) writeScript() (string, error) {
	dir := t.cacheDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "browser_bridge_*.js")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(browserBridgeJS); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// parseBridgeJSON decodes the bridge stdout object; non-JSON output is
// wrapped so callers still see the raw text.
func parseBridgeJSON(b []byte) map[string]any {
	res := map[string]any{}
	if err := json.Unmarshal(b, &res); err != nil {
		return map[string]any{"success": false, "error": "bridge returned non-JSON output", "output": string(b)}
	}
	return res
}

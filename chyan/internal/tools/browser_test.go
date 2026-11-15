// browser_test.go: browser tool tests with a fake node shim (no real
// node/playwright required).
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chyan.tech/chyan/internal/core"
)

func runBrowser(t *testing.T, tool *BrowserTool, in map[string]any) map[string]any {
	t.Helper()
	res := tool.Execute(context.Background(), core.ToolCall{ID: "1", Name: "browser", Input: in})
	out := map[string]any{}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result not JSON: %v: %s", err, res.Content)
	}
	return out
}

// writeShim installs a fake node in dir that records its JSON args to
// $SHIM_ARGS_FILE and prints out on stdout.
func writeShim(t *testing.T, dir, script string) {
	t.Helper()
	p := filepath.Join(dir, "node")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserMissingNode(t *testing.T) {
	tool := NewBrowserTool("node", t.TempDir(), 5*time.Second, 20000)
	t.Setenv("PATH", t.TempDir()) // empty dir: node cannot resolve
	out := runBrowser(t, tool, map[string]any{"action": "open", "url": "http://example.com"})
	if out["success"] == true {
		t.Fatal("missing node must fail, not panic")
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "browser tool requires node + playwright") {
		t.Errorf("error = %q", msg)
	}
}

func TestBrowserMissingNodeExplicitPath(t *testing.T) {
	tool := NewBrowserTool(filepath.Join(t.TempDir(), "no-such-node"), t.TempDir(), 5*time.Second, 20000)
	out := runBrowser(t, tool, map[string]any{"action": "open", "url": "http://e.com"})
	if out["success"] == true {
		t.Fatal("bad node path must fail")
	}
}

func TestBrowserArgValidation(t *testing.T) {
	tool := NewBrowserTool("node", t.TempDir(), 5*time.Second, 20000)
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"missing action", map[string]any{}, "action is required"},
		{"unknown action", map[string]any{"action": "hover"}, "unknown action"},
		{"open no url", map[string]any{"action": "open"}, "url is required"},
		{"text no url", map[string]any{"action": "text"}, "url is required"},
		{"screenshot no url", map[string]any{"action": "screenshot", "path": "/x/y.png"}, "url is required"},
		{"click no selector", map[string]any{"action": "click"}, "selector is required"},
		{"type no selector", map[string]any{"action": "type", "text": "hi"}, "selector is required"},
		{"screenshot no path", map[string]any{"action": "screenshot", "url": "http://e.com"}, "path is required"},
		{"screenshot relative path", map[string]any{"action": "screenshot", "url": "http://e.com", "path": "shot.png"}, "must be absolute"},
		{"screenshot outside cache", map[string]any{"action": "screenshot", "url": "http://e.com", "path": "/etc/shot.png"}, "must be inside"},
		{"file url", map[string]any{"action": "open", "url": "file:///etc/passwd"}, "only http/https"},
		{"data url", map[string]any{"action": "text", "url": "data:text/html,hi"}, "only http/https"},
		{"javascript url", map[string]any{"action": "open", "url": "javascript:alert(1)"}, "only http/https"},
		{"no host", map[string]any{"action": "open", "url": "http://"}, "must include a host"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			out := runBrowser(t, tool, tt.in)
			if out["success"] == true {
				t.Fatalf("want failure, got %v", out)
			}
			msg, _ := out["error"].(string)
			if !strings.Contains(msg, tt.want) {
				t.Errorf("error = %q, want contain %q", msg, tt.want)
			}
		})
	}
}

func TestBrowserCloseIsNoop(t *testing.T) {
	// close must succeed even without node (one-shot bridge)
	tool := NewBrowserTool("node", t.TempDir(), 5*time.Second, 20000)
	t.Setenv("PATH", t.TempDir())
	out := runBrowser(t, tool, map[string]any{"action": "close"})
	if out["success"] != true {
		t.Fatalf("close must succeed, got %v", out)
	}
}

func TestBrowserShimSuccessAndArgs(t *testing.T) {
	shimDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args.json")
	writeShim(t, shimDir, `echo "$2" > "$SHIM_ARGS_FILE"
echo '{"success":true,"action":"open","url":"http://example.com","title":"T","text":"hello world"}'`)
	t.Setenv("SHIM_ARGS_FILE", argsFile)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cache := t.TempDir()
	tool := NewBrowserTool("node", cache, 5*time.Second, 20000)
	out := runBrowser(t, tool, map[string]any{"action": "open", "url": "http://example.com"})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if out["title"] != "T" {
		t.Errorf("title = %v", out["title"])
	}
	// args passed to the bridge must be the serialized JSON payload
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("bridge args not JSON: %v: %s", err, raw)
	}
	if args["action"] != "open" || args["url"] != "http://example.com" {
		t.Errorf("bridge args = %v", args)
	}
}

func TestBrowserShimAllArgs(t *testing.T) {
	shimDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args.json")
	writeShim(t, shimDir, `echo "$2" > "$SHIM_ARGS_FILE"
echo '{"success":true}'`)
	t.Setenv("SHIM_ARGS_FILE", argsFile)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cache := t.TempDir()
	shot := filepath.Join(cache, "shots", "p.png")
	tool := NewBrowserTool("node", cache, 5*time.Second, 20000)
	out := runBrowser(t, tool, map[string]any{
		"action": "screenshot", "url": "http://example.com", "path": shot, "timeout": 9,
	})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != shot {
		t.Errorf("path arg = %v, want %v", args["path"], shot)
	}
	// timeout must not leak into the bridge payload
	if _, present := args["timeout"]; present {
		t.Errorf("timeout leaked into bridge args: %v", args)
	}
}

func TestBrowserShimTextTruncation(t *testing.T) {
	shimDir := t.TempDir()
	writeShim(t, shimDir, `echo '{"success":true,"text":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}'`)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tool := NewBrowserTool("node", t.TempDir(), 5*time.Second, 10)
	out := runBrowser(t, tool, map[string]any{"action": "text", "url": "http://e.com"})
	if out["success"] != true {
		t.Fatalf("want success, got %v", out)
	}
	if out["output_truncated"] != true {
		t.Errorf("want output_truncated, got %v", out)
	}
	if n := len(out["text"].(string)); n > 10 {
		t.Errorf("text not capped: %d bytes", n)
	}
}

func TestBrowserShimTimeout(t *testing.T) {
	shimDir := t.TempDir()
	writeShim(t, shimDir, "sleep 5\necho '{\"success\":true}'")
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tool := NewBrowserTool("node", t.TempDir(), 30*time.Second, 20000)
	start := time.Now()
	out := runBrowser(t, tool, map[string]any{"action": "open", "url": "http://e.com", "timeout": 1})
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("timeout override not honored: %v", elapsed)
	}
	if out["success"] == true {
		t.Fatal("want timeout failure")
	}
	if out["timed_out"] != true {
		t.Errorf("timed_out = %v", out["timed_out"])
	}
	// configured timeout path
	tool = NewBrowserTool("node", t.TempDir(), 500*time.Millisecond, 20000)
	out = runBrowser(t, tool, map[string]any{"action": "open", "url": "http://e.com"})
	if out["timed_out"] != true {
		t.Errorf("timed_out = %v", out["timed_out"])
	}
}

func TestBrowserShimNonJSONOutput(t *testing.T) {
	shimDir := t.TempDir()
	writeShim(t, shimDir, "echo 'SyntaxError: boom'; exit 1")
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tool := NewBrowserTool("node", t.TempDir(), 5*time.Second, 20000)
	out := runBrowser(t, tool, map[string]any{"action": "open", "url": "http://e.com"})
	if out["success"] == true {
		t.Fatal("non-JSON bridge output must fail")
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "non-JSON") {
		t.Errorf("error = %q", msg)
	}
	if !strings.Contains(out["output"].(string), "SyntaxError") {
		t.Errorf("raw output lost: %v", out["output"])
	}
}

func TestBrowserShimBridgeErrorJSON(t *testing.T) {
	shimDir := t.TempDir()
	writeShim(t, shimDir, `echo '{"success":false,"error":"browser tool requires node + playwright: cannot find module"}'`)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tool := NewBrowserTool("node", t.TempDir(), 5*time.Second, 20000)
	out := runBrowser(t, tool, map[string]any{"action": "open", "url": "http://e.com"})
	if out["success"] == true {
		t.Fatal("bridge error must fail")
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "requires node + playwright") {
		t.Errorf("error = %q", msg)
	}
}

func TestBrowserCanceledContext(t *testing.T) {
	shimDir := t.TempDir()
	writeShim(t, shimDir, `echo '{"success":true}'`)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tool := NewBrowserTool("node", t.TempDir(), 5*time.Second, 20000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := tool.Execute(ctx, core.ToolCall{Input: map[string]any{"action": "open", "url": "http://e.com"}})
	out := map[string]any{}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out["success"] == true {
		t.Error("canceled context must not succeed")
	}
}

func TestBrowserSchemaAndEmbed(t *testing.T) {
	tool := NewBrowserTool("", "", 0, 0)
	if tool.Name() != "browser" {
		t.Errorf("name = %s", tool.Name())
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Errorf("schema = %v", schema)
	}
	if !strings.Contains(browserBridgeJS, "playwright") {
		t.Error("embedded bridge script missing playwright usage")
	}
	if !strings.Contains(browserBridgeJS, "chromium") {
		t.Error("embedded bridge script missing chromium launch")
	}
	// screenshot confinement with default cache dir must not error for a
	// path under it
	def := NewBrowserTool("", "", 0, 0)
	if err := def.checkScreenshotPath(filepath.Join(def.cacheDir, "a.png")); err != nil {
		t.Errorf("default cache path rejected: %v", err)
	}
}

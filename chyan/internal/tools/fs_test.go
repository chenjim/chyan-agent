package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chyan.tech/chyan/internal/core"
)

// run executes tool and decodes the JSON payload.
func run(t *testing.T, tool *FSTool, in map[string]any) (map[string]any, core.ToolResult) {
	t.Helper()
	res := tool.Execute(context.Background(), core.ToolCall{ID: "1", Name: tool.Name(), Input: in})
	out := map[string]any{}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result not JSON: %v: %s", err, res.Content)
	}
	return out, res
}

func ok(t *testing.T, out map[string]any) {
	t.Helper()
	if out["success"] != true {
		t.Fatalf("expected success, got %v", out)
	}
}

func failMsg(t *testing.T, out map[string]any) string {
	t.Helper()
	if out["success"] == true {
		t.Fatalf("expected failure, got %v", out)
	}
	msg, _ := out["error"].(string)
	return msg
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTool(t *testing.T, kind string) (*FSTool, string) {
	t.Helper()
	dir := t.TempDir()
	return newFS(kind, dir, 20000), dir
}

func TestReadBasic(t *testing.T) {
	tool, dir := newTool(t, "read")
	writeFile(t, filepath.Join(dir, "a.txt"), "l1\nl2\nl3\n")
	out, _ := run(t, tool, map[string]any{"path": "a.txt"})
	ok(t, out)
	if out["content"] != "l1\nl2\nl3\n" {
		t.Errorf("content = %q", out["content"])
	}
	if out["total_lines"] != float64(3) {
		t.Errorf("total_lines = %v", out["total_lines"])
	}
	sha, _ := out["sha256"].(string)
	if len(sha) != 64 {
		t.Errorf("sha256 = %q", sha)
	}
	if m, _ := out["mtime"].(string); m == "" {
		t.Errorf("mtime = %v", out["mtime"])
	}
	if out["truncated"] != false {
		t.Errorf("truncated = %v", out["truncated"])
	}
}

func TestReadWindow(t *testing.T) {
	tool, dir := newTool(t, "read")
	writeFile(t, filepath.Join(dir, "a.txt"), "l1\nl2\nl3\nl4\n")
	tests := []struct {
		name          string
		in            map[string]any
		want          string
		start, end    float64
		wantTruncated bool
	}{
		{"all", map[string]any{"path": "a.txt"}, "l1\nl2\nl3\nl4\n", 1, 4, false},
		{"offset", map[string]any{"path": "a.txt", "offset": 2}, "l2\nl3\nl4\n", 2, 4, true},
		{"limit", map[string]any{"path": "a.txt", "limit": 2}, "l1\nl2\n", 1, 2, true},
		{"both", map[string]any{"path": "a.txt", "offset": 2, "limit": 2}, "l2\nl3\n", 2, 3, true},
		{"offset beyond", map[string]any{"path": "a.txt", "offset": 99}, "", 5, 4, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _ := run(t, tool, tt.in)
			ok(t, out)
			if out["content"] != tt.want {
				t.Errorf("content = %q, want %q", out["content"], tt.want)
			}
			if out["truncated"] != tt.wantTruncated {
				t.Errorf("truncated = %v", out["truncated"])
			}
		})
	}
}

func TestReadErrors(t *testing.T) {
	tool, dir := newTool(t, "read")
	writeFile(t, filepath.Join(dir, "big.bin"), strings.Repeat("x", maxFileBytes+1))
	writeFile(t, filepath.Join(dir, "sub"), "x") // placeholder replaced below
	if err := os.Remove(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		in   map[string]any
	}{
		{"missing path", map[string]any{}},
		{"empty path", map[string]any{"path": ""}},
		{"not found", map[string]any{"path": "nope.txt"}},
		{"directory", map[string]any{"path": "sub"}},
		{"too large", map[string]any{"path": "big.bin"}},
		{"sensitive", map[string]any{"path": ".env"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _ := run(t, tool, tt.in)
			if failMsg(t, out) == "" {
				t.Error("empty error message")
			}
		})
	}
}

func TestWriteModes(t *testing.T) {
	tool, dir := newTool(t, "write")
	out, _ := run(t, tool, map[string]any{"path": "dir/a.txt", "content": "one"})
	ok(t, out)
	out, _ = run(t, tool, map[string]any{"path": "dir/a.txt", "content": "two", "mode": "append"})
	ok(t, out)
	data, err := os.ReadFile(filepath.Join(dir, "dir", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "onetwo" {
		t.Errorf("got %q", data)
	}
	out, _ = run(t, tool, map[string]any{"path": "dir/a.txt", "content": "three"})
	ok(t, out)
	data, _ = os.ReadFile(filepath.Join(dir, "dir", "a.txt"))
	if string(data) != "three" {
		t.Errorf("got %q", data)
	}
}

func TestWriteAndEditPreserveInternalSymlink(t *testing.T) {
	for _, kind := range []string{"write", "edit"} {
		t.Run(kind, func(t *testing.T) {
			tool, dir := newTool(t, kind)
			target := filepath.Join(dir, "target.txt")
			alias := filepath.Join(dir, "alias.txt")
			writeFile(t, target, "before")
			if err := os.Symlink("target.txt", alias); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
			in := map[string]any{"path": "alias.txt", "content": "after"}
			if kind == "edit" {
				in = map[string]any{"path": "alias.txt", "old_string": "before", "new_string": "after"}
			}
			ok(t, mustRun(t, tool, in))
			info, err := os.Lstat(alias)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("atomic write replaced the symlink instead of its target")
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "after" {
				t.Errorf("target content = %q, want after", data)
			}
		})
	}
}

func TestWriteErrors(t *testing.T) {
	tool, dir := newTool(t, "write")
	writeFile(t, filepath.Join(dir, ".env"), "x")
	tests := []struct {
		name string
		in   map[string]any
	}{
		{"no content", map[string]any{"path": "a"}},
		{"no path", map[string]any{"content": "x"}},
		{"bad mode", map[string]any{"path": "a", "content": "x", "mode": "swap"}},
		{"content too large", map[string]any{"path": "a", "content": strings.Repeat("x", maxFileBytes+1)}},
		{"sensitive write", map[string]any{"path": ".env", "content": "x"}},
		{"escape", map[string]any{"path": "../out", "content": "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _ := run(t, tool, tt.in)
			if failMsg(t, out) == "" {
				t.Error("empty error message")
			}
		})
	}
	// append growth over the cap is refused
	writeFile(t, filepath.Join(dir, "g.txt"), strings.Repeat("x", maxFileBytes))
	out, _ := run(t, tool, map[string]any{"path": "g.txt", "content": "yy", "mode": "append"})
	if failMsg(t, out) == "" {
		t.Error("want size error")
	}
}

func TestEdit(t *testing.T) {
	tool, dir := newTool(t, "edit")
	writeFile(t, filepath.Join(dir, "a.txt"), "one two one")
	out, _ := run(t, tool, map[string]any{"path": "a.txt", "old_string": "one", "new_string": "1"})
	if msg := failMsg(t, out); !strings.Contains(msg, "2 places") {
		t.Errorf("want ambiguity error, got %q", msg)
	}
	out, _ = run(t, tool, map[string]any{"path": "a.txt", "old_string": "one", "new_string": "1", "replace_all": true})
	ok(t, out)
	if out["replacements"] != float64(2) {
		t.Errorf("replacements = %v", out["replacements"])
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(data) != "1 two 1" {
		t.Errorf("got %q", data)
	}
}

func TestEditErrors(t *testing.T) {
	tool, dir := newTool(t, "edit")
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	tests := []struct {
		name string
		in   map[string]any
	}{
		{"empty old", map[string]any{"path": "a.txt", "old_string": "", "new_string": "x"}},
		{"not found", map[string]any{"path": "a.txt", "old_string": "zzz", "new_string": "x"}},
		{"missing file", map[string]any{"path": "nope", "old_string": "a", "new_string": "x"}},
		{"no path", map[string]any{"old_string": "a", "new_string": "x"}},
		{"sensitive", map[string]any{"path": "config.toml", "old_string": "a", "new_string": "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _ := run(t, tool, tt.in)
			if failMsg(t, out) == "" {
				t.Error("empty error message")
			}
		})
	}
	// resulting size over the cap
	writeFile(t, filepath.Join(dir, "b.txt"), strings.Repeat("x", maxFileBytes-10))
	out, _ := run(t, tool, map[string]any{"path": "b.txt", "old_string": "x", "new_string": strings.Repeat("y", 100), "replace_all": true})
	if failMsg(t, out) == "" {
		t.Error("want size error")
	}
}

func TestConflictCheck(t *testing.T) {
	tool, dir := newTool(t, "edit")
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "hello")

	readTool := newFS("read", dir, 20000)
	out, _ := run(t, readTool, map[string]any{"path": "a.txt"})
	ok(t, out)
	sha, _ := out["sha256"].(string)
	mtime, _ := out["mtime"].(string)

	// matching expectations pass
	out, _ = run(t, tool, map[string]any{
		"path": "a.txt", "old_string": "hello", "new_string": "world",
		"expected_sha256": sha, "expected_mtime": mtime,
	})
	ok(t, out)

	// stale sha is refused with a conflict hint
	writeFile(t, path, "externally changed")
	out, _ = run(t, tool, map[string]any{
		"path": "a.txt", "old_string": "changed", "new_string": "x",
		"expected_sha256": sha,
	})
	msg := failMsg(t, out)
	if !strings.Contains(msg, "re-read") {
		t.Errorf("want re-read hint, got %q", msg)
	}
	cur, _ := out["current_sha256"].(string)
	if cur == "" || cur == sha {
		t.Errorf("current_sha256 = %q", cur)
	}
	if out["conflict"] != true {
		t.Errorf("conflict flag = %v", out["conflict"])
	}

	// stale mtime is refused too
	out, _ = run(t, tool, map[string]any{
		"path": "a.txt", "old_string": "changed", "new_string": "x",
		"expected_mtime": "2000-01-01T00:00:00Z",
	})
	if msg := failMsg(t, out); !strings.Contains(msg, "mtime") {
		t.Errorf("want mtime conflict, got %q", msg)
	}

	// malformed mtime
	out, _ = run(t, tool, map[string]any{
		"path": "a.txt", "old_string": "changed", "new_string": "x",
		"expected_mtime": "not-a-time",
	})
	if msg := failMsg(t, out); !strings.Contains(msg, "expected_mtime") {
		t.Errorf("want mtime parse error, got %q", msg)
	}

	// write conflict via sha
	wtool := newFS("write", dir, 20000)
	out, _ = run(t, wtool, map[string]any{
		"path": "a.txt", "content": "x", "expected_sha256": strings.Repeat("0", 64),
	})
	if msg := failMsg(t, out); !strings.Contains(msg, "re-read") {
		t.Errorf("want conflict, got %q", msg)
	}
}

func TestListAndSearchAndGrepAndDelete(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "src", "main.go"), "package main\n// TODO fix\n")
	writeFile(t, filepath.Join(dir, "src", "util.go"), "package main\n// done\n")
	writeFile(t, filepath.Join(dir, "src", "deep", "x.txt"), "TODO deep\n")
	writeFile(t, filepath.Join(dir, "node_modules", "skip.js"), "TODO skip\n")
	writeFile(t, filepath.Join(dir, ".hidden", "h.txt"), "TODO hide\n")

	t.Run("list", func(t *testing.T) {
		tool := newFS("list", dir, 20000)
		out, _ := run(t, tool, map[string]any{"path": "."})
		ok(t, out)
		names := []string{}
		for _, e := range out["entries"].([]any) {
			names = append(names, e.(map[string]any)["name"].(string))
		}
		joined := strings.Join(names, ",")
		if !strings.Contains(joined, "src") {
			t.Errorf("entries = %v", names)
		}
		if strings.Contains(joined, "node_modules") || strings.Contains(joined, ".hidden") {
			t.Errorf("excluded dirs leaked: %v", names)
		}
	})
	t.Run("list depth error", func(t *testing.T) {
		tool := newFS("list", dir, 20000)
		out, _ := run(t, tool, map[string]any{"path": "src/main.go"})
		if failMsg(t, out) == "" {
			t.Error("want error for non-dir")
		}
		out, _ = run(t, tool, map[string]any{"path": "missing"})
		if failMsg(t, out) == "" {
			t.Error("want error for missing")
		}
	})
	t.Run("search", func(t *testing.T) {
		tool := newFS("search", dir, 20000)
		out, _ := run(t, tool, map[string]any{"path": ".", "pattern": "*.go"})
		ok(t, out)
		if out["count"] != float64(2) {
			t.Errorf("count = %v (%v)", out["count"], out["results"])
		}
	})
	t.Run("search errors", func(t *testing.T) {
		tool := newFS("search", dir, 20000)
		for _, in := range []map[string]any{
			{"path": "missing", "pattern": "*"},
			{"path": "."},
			{"pattern": "*"},
		} {
			if failMsg(t, mustRun(t, tool, in)) == "" {
				t.Errorf("want error for %v", in)
			}
		}
	})
	t.Run("grep", func(t *testing.T) {
		tool := newFS("grep", dir, 20000)
		out, _ := run(t, tool, map[string]any{"path": ".", "pattern": "TODO", "include": "*.go"})
		ok(t, out)
		if out["count"] != float64(1) {
			t.Errorf("count = %v (%v)", out["count"], out["results"])
		}
		res := out["results"].([]any)[0].(map[string]any)
		if res["count"] != float64(1) {
			t.Errorf("file match count = %v", res)
		}
		// single-file target works too
		out, _ = run(t, tool, map[string]any{"path": "src/main.go", "pattern": "TODO"})
		ok(t, out)
		if out["count"] != float64(1) {
			t.Errorf("single-file count = %v", out["count"])
		}
	})
	t.Run("grep errors", func(t *testing.T) {
		tool := newFS("grep", dir, 20000)
		if failMsg(t, mustRun(t, tool, map[string]any{"path": ".", "pattern": "["})) == "" {
			t.Error("want invalid pattern error")
		}
		if failMsg(t, mustRun(t, tool, map[string]any{"path": "missing", "pattern": "x"})) == "" {
			t.Error("want missing error")
		}
		if out := mustRun(t, tool, map[string]any{"path": ".", "pattern": "x"}); out["success"] != true {
			t.Errorf("valid pattern must succeed: %v", out)
		}
		// sensitive target refused
		writeFile(t, filepath.Join(dir, ".env"), "SECRET=1")
		if failMsg(t, mustRun(t, tool, map[string]any{"path": ".env", "pattern": "SECRET"})) == "" {
			t.Error("want sensitive error")
		}
	})
	t.Run("delete", func(t *testing.T) {
		tool := newFS("delete", dir, 20000)
		writeFile(t, filepath.Join(dir, "gone.txt"), "x")
		out, _ := run(t, tool, map[string]any{"path": "gone.txt"})
		ok(t, out)
		if _, err := os.Stat(filepath.Join(dir, "gone.txt")); !os.IsNotExist(err) {
			t.Error("file should be gone")
		}
		// non-empty dir refuses
		if failMsg(t, mustRun(t, tool, map[string]any{"path": "src"})) == "" {
			t.Error("want non-empty dir error")
		}
		// missing
		if failMsg(t, mustRun(t, tool, map[string]any{"path": "gone.txt"})) == "" {
			t.Error("want missing error")
		}
		// sensitive
		if failMsg(t, mustRun(t, tool, map[string]any{"path": ".env"})) == "" {
			t.Error("want sensitive error")
		}
	})
}

func mustRun(t *testing.T, tool *FSTool, in map[string]any) map[string]any {
	t.Helper()
	out, _ := run(t, tool, in)
	return out
}

func TestEscapeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "outside data")
	writeFile(t, filepath.Join(dir, "ok.txt"), "inside")

	tests := []struct {
		name string
		path string
	}{
		{"dotdot", "../ok"},
		{"deep dotdot", "a/../../ok"},
		{"absolute outside", filepath.Join(outside, "secret.txt")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := newFS("read", dir, 20000)
			out, _ := run(t, tool, map[string]any{"path": tt.path})
			if msg := failMsg(t, out); !strings.Contains(msg, "escape") {
				t.Errorf("want escape error, got %q", msg)
			}
		})
	}

	t.Run("symlink file escape", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		tool := newFS("read", dir, 20000)
		out, _ := run(t, tool, map[string]any{"path": "link.txt"})
		if msg := failMsg(t, out); !strings.Contains(msg, "escape") {
			t.Errorf("want symlink escape error, got %q", msg)
		}
	})
	t.Run("symlink dir escape", func(t *testing.T) {
		if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		tool := newFS("read", dir, 20000)
		out, _ := run(t, tool, map[string]any{"path": "sub/secret.txt"})
		if msg := failMsg(t, out); !strings.Contains(msg, "escape") {
			t.Errorf("want dir symlink escape error, got %q", msg)
		}
	})
	t.Run("inner symlink ok", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(dir, "ok.txt"), filepath.Join(dir, "inner.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		tool := newFS("read", dir, 20000)
		out, _ := run(t, tool, map[string]any{"path": "inner.txt"})
		ok(t, out)
	})
	t.Run("alias through escaping directory refused", func(t *testing.T) {
		if err := os.Symlink(outside, filepath.Join(dir, "dirlink")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if err := os.Symlink(filepath.Join("dirlink", "secret.txt"), filepath.Join(dir, "alias.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		for _, kind := range []string{"read", "write"} {
			tool := newFS(kind, dir, 20000)
			in := map[string]any{"path": "alias.txt"}
			if kind == "write" {
				in["content"] = "overwritten"
			}
			out := mustRun(t, tool, in)
			if msg := failMsg(t, out); !strings.Contains(msg, "escape") {
				t.Errorf("%s: want symlink escape error, got %q", kind, msg)
			}
		}
		data, err := os.ReadFile(filepath.Join(outside, "secret.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "outside data" {
			t.Errorf("outside file changed to %q", data)
		}
	})
	t.Run("dangling symlink write refused", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(outside, "new.txt"), filepath.Join(dir, "dangling.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		tool := newFS("write", dir, 20000)
		out := mustRun(t, tool, map[string]any{"path": "dangling.txt", "content": "x"})
		if failMsg(t, out) == "" {
			t.Error("want error for dangling symlink")
		}
		if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
			t.Error("write escaped through dangling symlink")
		}
	})
	t.Run("symlink alias to .env refused", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, ".env"), "SECRET=1")
		if err := os.Symlink(filepath.Join(dir, ".env"), filepath.Join(dir, "notes.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		tool := newFS("read", dir, 20000)
		out := mustRun(t, tool, map[string]any{"path": "notes.txt"})
		if msg := failMsg(t, out); !strings.Contains(msg, "sensitive") {
			t.Errorf("want sensitive error, got %q", msg)
		}
	})
	t.Run("grep skips symlink targets", func(t *testing.T) {
		outsideDir := t.TempDir()
		writeFile(t, filepath.Join(outsideDir, "leak.txt"), "TODO outside\n")
		if err := os.Symlink(filepath.Join(outsideDir, "leak.txt"), filepath.Join(dir, "leak-link.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		tool := newFS("grep", dir, 20000)
		out, _ := run(t, tool, map[string]any{"path": ".", "pattern": "TODO"})
		ok(t, out)
		if out["count"] != float64(0) {
			t.Errorf("grep followed a symlink: %v", out["results"])
		}
	})
}

func TestSensitiveBlacklist(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"config.toml", "config.example.toml", ".env", ".env.local"} {
		writeFile(t, filepath.Join(dir, name), "x")
	}
	writeFile(t, filepath.Join(dir, "id_rsa"), "key")
	writeFile(t, filepath.Join(dir, "server.pem"), "cert")
	writeFile(t, filepath.Join(dir, ".ssh", "config"), "cfg")
	writeFile(t, filepath.Join(dir, "sub", "app.keystore"), "ks")
	writeFile(t, filepath.Join(dir, "my_secrets.txt"), "s")

	cases := []string{
		"config.toml", "config.example.toml", ".env", ".env.local",
		"id_rsa", "server.pem", ".ssh/config", "sub/app.keystore", "my_secrets.txt",
	}
	for _, name := range cases {
		for _, kind := range []string{"read", "write", "edit", "delete"} {
			tool := newFS(kind, dir, 20000)
			in := map[string]any{"path": name}
			switch kind {
			case "write":
				in["content"] = "x"
			case "edit":
				in["old_string"] = "x"
				in["new_string"] = "y"
			}
			out := mustRun(t, tool, in)
			msg := failMsg(t, out)
			if !strings.Contains(msg, "sensitive") {
				t.Errorf("%s %s: want sensitive error, got %q", kind, name, msg)
			}
		}
	}
	// non-sensitive names pass
	readTool := newFS("read", dir, 20000)
	writeFile(t, filepath.Join(dir, "normal.txt"), "fine")
	out := mustRun(t, readTool, map[string]any{"path": "normal.txt"})
	ok(t, out)
}

func TestOutputTruncation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), strings.Repeat("abc\n", 1000))
	tool := newFS("read", dir, 100)
	out, res := run(t, tool, map[string]any{"path": "a.txt"})
	ok(t, out)
	if !res.Truncated {
		t.Error("want Truncated flag")
	}
	if out["output_truncated"] != true {
		t.Error("want output_truncated marker")
	}
	// the content field itself is capped at tool_output_max_bytes
	if content, _ := out["content"].(string); len(content) > 100 {
		t.Errorf("content field not capped: %d bytes", len(content))
	}
}

func TestReadSchemaAndNames(t *testing.T) {
	tools := []*FSTool{
		NewReadTool(t.TempDir(), 0), NewWriteTool(t.TempDir(), 0),
		NewEditTool(t.TempDir(), 0), NewListTool(t.TempDir(), 0),
		NewSearchTool(t.TempDir(), 0), NewGrepTool(t.TempDir(), 0),
		NewDeleteTool(t.TempDir(), 0),
	}
	for _, tool := range tools {
		if tool.Name() == "" {
			t.Error("empty name")
		}
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Errorf("%s schema = %v", tool.Name(), schema)
		}
	}
	// unknown kind still yields a valid empty schema
	unknown := newFS("bogus", t.TempDir(), 0)
	if unknown.Schema()["type"] != "object" {
		t.Error("bad schema")
	}
	res := unknown.Execute(context.Background(), core.ToolCall{Input: map[string]any{}})
	if !strings.Contains(res.Content, "unknown") {
		t.Errorf("want unknown tool error, got %s", res.Content)
	}
}

func TestGrepSnippetCap(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), strings.Repeat("hit line\n", 30))
	tool := newFS("grep", dir, 20000)
	out, _ := run(t, tool, map[string]any{"path": ".", "pattern": "hit"})
	ok(t, out)
	res := out["results"].([]any)[0].(map[string]any)
	lines := res["lines"].([]any)
	if len(lines) != grepSnippets {
		t.Errorf("lines = %d, want %d", len(lines), grepSnippets)
	}
	if res["count"] != float64(30) {
		t.Errorf("count = %v", res["count"])
	}
}

func TestMarshalResultFallback(t *testing.T) {
	// unserializable content path is exercised via a payload holding a
	// channel; json.Marshal fails and falls back to fmt.
	res := marshalResult(map[string]any{"bad": make(chan int)}, 100)
	if res.Content == "" {
		t.Error("want fallback content")
	}
	// oversized payload becomes a valid-JSON preview envelope
	res = marshalResult(map[string]any{"success": true, "v": strings.Repeat("汉", 1000)}, 20)
	if !res.Truncated {
		t.Error("want truncated")
	}
	env := map[string]any{}
	if err := json.Unmarshal([]byte(res.Content), &env); err != nil {
		t.Fatalf("preview envelope not JSON: %v", err)
	}
	if env["truncated_output"] != true {
		t.Errorf("want truncated_output marker, got %v", env)
	}
}

// TestReadImageReturnsMultimodal: png/jpg files come back as image payloads
// (FR-10), text files keep the text path.
func TestReadImageReturnsMultimodal(t *testing.T) {
	tool, dir := newTool(t, "read")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR") // magic bytes are enough
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(context.Background(), core.ToolCall{
		ID: "1", Name: "read", Input: map[string]any{"path": "shot.png"},
	})
	if len(res.Images) != 1 || res.Images[0].MIME != "image/png" {
		t.Fatalf("images = %+v", res.Images)
	}
	if string(res.Images[0].Data) != string(png) {
		t.Fatal("image bytes differ")
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(res.Content), &meta); err != nil {
		t.Fatalf("meta not JSON: %v", err)
	}
	if meta["mime"] != "image/png" || meta["success"] != true {
		t.Fatalf("meta = %v", meta)
	}

	// A text file still takes the plain reader (no Images).
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	text := tool.Execute(context.Background(), core.ToolCall{
		ID: "2", Name: "read", Input: map[string]any{"path": "a.txt"},
	})
	if len(text.Images) != 0 {
		t.Fatalf("text read returned images: %+v", text.Images)
	}
}

// TestReadImageRejectsOversizedFile: an image over the size cap fails on the
// reported size and is never returned as a payload.
func TestReadImageRejectsOversizedFile(t *testing.T) {
	tool, dir := newTool(t, "read")
	big := make([]byte, core.MaxImageBytes+2)
	copy(big, "\x89PNG\r\n\x1a\n")
	if err := os.WriteFile(filepath.Join(dir, "big.png"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(context.Background(), core.ToolCall{
		ID: "1", Name: "read", Input: map[string]any{"path": "big.png"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "image too large") {
		t.Fatalf("err = %v, want image too large", res.Err)
	}
	if len(res.Images) != 0 {
		t.Fatalf("oversized image returned payload: %d bytes", len(res.Images[0].Data))
	}
}

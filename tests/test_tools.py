import json
import tempfile
from pathlib import Path

from chyan_agent.tools import ToolExecutor
from chyan_agent.memory import MemoryManager


class TestToolExecutor:
    def setup_method(self):
        self.tmpdir = tempfile.mkdtemp()
        self.mm = MemoryManager(self.tmpdir, recall_mode="grep")
        self.te = ToolExecutor(self.mm)
        self.te.update_config({"read_max_lines": 500, "read_truncate_mode": "auto"})

    def teardown_method(self):
        import shutil
        shutil.rmtree(self.tmpdir)

    def test_schemas_count(self):
        schemas = self.te.get_schemas()
        names = [s["function"]["name"] for s in schemas]
        for s in schemas:
            assert s["type"] == "function"
            assert "name" in s["function"]
        for expected in (
            "read", "write", "edit", "list", "search", "grep",
            "delete", "remember", "recall", "todo_write", "todo_read",
        ):
            assert expected in names, f"缺少工具 {expected}"
        # bash/web 默认关闭，不注册
        assert "bash" not in names
        assert "web_fetch" not in names
        assert len(schemas) == 11

    def test_schemas_gated_by_config(self):
        self.te.update_config({"bash_enabled": True, "web_enabled": True})
        names = [s["function"]["name"] for s in self.te.get_schemas()]
        assert "bash" in names and "web_fetch" in names and "web_search" in names
        assert len(names) == 14

    def test_read_file(self):
        path = Path(self.tmpdir) / "test.txt"
        path.write_text("hello world")
        result = self.te.execute("read", path=str(path))
        assert result["success"]
        assert "hello world" in result["content"]
        assert result["total_lines"] == 1

    def test_read_not_found(self):
        result = self.te.execute("read", path=str(Path(self.tmpdir) / "nope.txt"))
        assert not result["success"]
        assert "不存在" in result["error"]

    def test_write_file(self):
        path = Path(self.tmpdir) / "new.txt"
        result = self.te.execute("write", path=str(path), content="test content")
        assert result["success"]
        assert path.read_text() == "test content"

    def test_write_append(self):
        path = Path(self.tmpdir) / "append.txt"
        path.write_text("line1\n")
        self.te.execute("write", path=str(path), content="line2\n", mode="append")
        assert path.read_text() == "line1\nline2\n"

    def test_write_sensitive_file_blocked(self):
        for name in ("config.toml", ".env"):
            result = self.te.execute("write", path=name, content="hack")
            assert not result["success"], f"expected {name} to be blocked"
            assert "禁止" in result["error"]

    def test_search(self):
        (Path(self.tmpdir) / "a.py").write_text("")
        (Path(self.tmpdir) / "b.py").write_text("")
        (Path(self.tmpdir) / "sub").mkdir()
        (Path(self.tmpdir) / "sub" / "c.py").write_text("")
        result = self.te.execute("search", path=self.tmpdir, pattern="*.py")
        assert result["success"]
        assert result["count"] == 3

    def test_grep(self):
        path = Path(self.tmpdir) / "sample.txt"
        path.write_text("hello world\nfoo bar\nhello again")
        result = self.te.execute("grep", path=str(path), pattern="hello")
        assert result["success"]
        assert result["count"] == 1
        assert result["results"][0]["matches"] == 2

    def test_remember_tool(self):
        self.te.current_turn = 1
        result = self.te.execute("remember", content="test fact")
        assert result["success"]
        assert result["memory_id"] == "m1"

    def test_recall_tool(self):
        self.te.current_turn = 1
        self.te.execute("remember", content="Python is great")
        self.te.execute("remember", content="PostgreSQL is the DB")
        result = self.te.execute("recall", query="Python", top_k=5)
        assert result["success"]
        assert result["count"] >= 1

    def test_read_truncate_head(self):
        path = Path(self.tmpdir) / "big.txt"
        path.write_text("\n".join(f"line {i}" for i in range(100)))
        self.te.update_config({"read_max_lines": 10, "read_truncate_mode": "head"})
        result = self.te.execute("read", path=str(path))
        assert result["success"]
        assert result["truncated"]
        assert "[截断" in result["content"]

    def test_read_truncate_tail(self):
        path = Path(self.tmpdir) / "big.txt"
        path.write_text("\n".join(f"line {i}" for i in range(100)))
        self.te.update_config({"read_max_lines": 10, "read_truncate_mode": "tail"})
        result = self.te.execute("read", path=str(path))
        assert result["success"]
        assert "[截断" in result["content"]

    def test_read_truncate_auto(self):
        path = Path(self.tmpdir) / "big.txt"
        path.write_text("\n".join(f"line {i}" for i in range(100)))
        self.te.update_config({"read_max_lines": 20, "read_truncate_mode": "auto"})
        result = self.te.execute("read", path=str(path))
        assert result["success"]
        assert result["truncated"]

    def test_delete_file(self):
        path = Path(self.tmpdir) / "to_delete.txt"
        path.write_text("delete me")
        result = self.te.execute("delete", path=str(path))
        assert result["success"]
        assert result["action"] == "deleted"
        assert not path.exists()

    def test_delete_sensitive_blocked(self):
        result = self.te.execute("delete", path="config.toml")
        assert not result["success"]
        assert "禁止" in result["error"]

    def test_delete_not_found(self):
        result = self.te.execute("delete", path="/not/exist")
        assert not result["success"]

    def test_unknown_tool(self):
        result = self.te.execute("nonexistent", foo="bar")
        assert not result["success"]

    # ---------------------------------------------------------------- read range

    def test_read_range(self):
        path = Path(self.tmpdir) / "lines.txt"
        path.write_text("\n".join(f"L{i}" for i in range(1, 11)))
        result = self.te.execute("read", path=str(path), offset=3, limit=4)
        assert result["success"]
        assert result["start_line"] == 3
        assert "L3" in result["content"] and "L6" in result["content"]
        assert "L7" not in result["content"]

    def test_read_range_offset_only(self):
        path = Path(self.tmpdir) / "lines.txt"
        path.write_text("\n".join(f"L{i}" for i in range(1, 6)))
        result = self.te.execute("read", path=str(path), offset=4)
        assert result["start_line"] == 4
        assert "L4" in result["content"]

    # ---------------------------------------------------------------------- edit

    def test_edit_unique(self):
        p = Path(self.tmpdir) / "e.txt"
        p.write_text("foo bar baz")
        r = self.te.execute("edit", path=str(p), old_string="bar", new_string="BAR")
        assert r["success"] and r["replacements"] == 1
        assert p.read_text() == "foo BAR baz"

    def test_edit_not_found(self):
        p = Path(self.tmpdir) / "e.txt"
        p.write_text("abc")
        r = self.te.execute("edit", path=str(p), old_string="zzz", new_string="x")
        assert not r["success"] and "未找到" in r["error"]

    def test_edit_multiple_requires_replace_all(self):
        p = Path(self.tmpdir) / "e.txt"
        p.write_text("x x x")
        r = self.te.execute("edit", path=str(p), old_string="x", new_string="y")
        assert not r["success"] and "匹配到" in r["error"]
        r2 = self.te.execute("edit", path=str(p), old_string="x", new_string="y", replace_all=True)
        assert r2["success"] and r2["replacements"] == 3
        assert p.read_text() == "y y y"

    def test_edit_sensitive_blocked(self):
        r = self.te.execute("edit", path="config.toml", old_string="a", new_string="b")
        assert not r["success"]

    # ---------------------------------------------------------------------- list

    def test_list(self):
        d = Path(self.tmpdir)
        (d / "f.txt").write_text("hi")
        (d / "sub").mkdir()
        (d / ".hidden").write_text("x")
        r = self.te.execute("list", path=self.tmpdir)
        assert r["success"]
        names = [e["path"] for e in r["entries"]]
        assert "f.txt" in names and "sub" in names
        assert ".hidden" not in names

    def test_list_depth(self):
        d = Path(self.tmpdir) / "a" / "b"
        d.mkdir(parents=True)
        (d / "deep.txt").write_text("x")
        r = self.te.execute("list", path=self.tmpdir, depth=3)
        assert any(e["path"] == "a/b/deep.txt" for e in r["entries"])

    # ---------------------------------------------------------------------- todo

    def test_todo_write_read(self):
        r = self.te.execute("todo_write", todos=[
            {"content": "a", "status": "pending"},
            {"content": "b", "status": "completed"},
        ])
        assert r["success"] and r["count"] == 2
        r2 = self.te.execute("todo_read")
        assert r2["summary"]["completed"] == 1

    def test_todo_invalid(self):
        assert not self.te.execute("todo_write", todos=[{"content": "a", "status": "bad"}])["success"]
        assert not self.te.execute("todo_write", todos=[{"status": "pending"}])["success"]

    # ---------------------------------------------------------------- output budget

    def test_output_budget_truncates(self):
        self.te.update_config({"tool_output_max_bytes": 200})
        p = Path(self.tmpdir) / "big.txt"
        p.write_text("A" * 2000)
        r = self.te.execute("read", path=str(p))
        assert r.get("truncated_output") is True
        assert "preview" in r

    # ------------------------------------------------------------------ work_dir

    def test_work_dir_confinement(self):
        outside = Path(tempfile.mkdtemp())
        try:
            self.te.update_config({"work_dir": self.tmpdir})
            r = self.te.execute("read", path=str(outside / "x.txt"))
            assert not r["success"] and "超出工作目录" in r["error"]
            ok = self.te.execute("write", path=str(Path(self.tmpdir) / "in.txt"), content="x")
            assert ok["success"]
        finally:
            import shutil
            shutil.rmtree(outside)

    # ----------------------------------------------------------------------- bash

    def test_bash_disabled(self):
        r = self.te.execute("bash", command="echo hi")
        assert not r["success"] and "未启用" in r["error"]

    def test_bash_simple(self):
        self.te.update_config({"bash_enabled": True})
        r = self.te.execute("bash", command="echo hello")
        assert r["success"] and r["exit_code"] == 0
        assert "hello" in r["stdout"]

    def test_bash_dangerous_blocked(self):
        self.te.update_config({"bash_enabled": True})
        r = self.te.execute("bash", command="rm -rf /")
        assert not r["success"] and "危险" in r["error"]

    def test_bash_nonzero_exit(self):
        self.te.update_config({"bash_enabled": True})
        r = self.te.execute("bash", command="exit 3")
        assert r["exit_code"] == 3 and not r["success"]

    def test_bash_timeout(self):
        self.te.update_config({"bash_enabled": True, "bash_timeout": 1})
        r = self.te.execute("bash", command="sleep 5")
        assert not r["success"] and "超时" in r["error"]

    # ------------------------------------------------------------------------ web

    def test_web_disabled(self):
        assert not self.te.execute("web_fetch", url="https://example.com")["success"]
        assert not self.te.execute("web_search", query="x")["success"]


class TestWebHelpers:
    def test_html_to_text(self):
        from chyan_agent.tools import _html_to_text
        text = _html_to_text(
            "<html><body><h1>Hello</h1><script>x=1</script><p>world</p></body></html>"
        )
        assert "Hello" in text and "world" in text
        assert "x=1" not in text

    def test_parse_search_results(self):
        from chyan_agent.tools import _parse_search_results
        html = ('<a class="result__a" '
                'href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa">Example</a>')
        res = _parse_search_results(html)
        assert res[0]["url"] == "https://example.com/a"
        assert res[0]["title"] == "Example"

    def test_parse_search_json(self):
        from chyan_agent.tools import _parse_search_json
        body = json.dumps({"results": [{"title": "T", "url": "https://a", "content": "S"}]})
        res = _parse_search_json(body)
        assert res[0] == {"title": "T", "url": "https://a", "snippet": "S"}
        assert _parse_search_json("<html></html>") is None
        assert _parse_search_json("not json") is None

    def test_looks_like_challenge(self):
        from chyan_agent.tools import _looks_like_challenge
        assert _looks_like_challenge("Please complete the following challenge")
        assert not _looks_like_challenge("<html>normal page</html>")

    def test_ddg_unwrap_plain(self):
        from chyan_agent.tools import _ddg_unwrap
        assert _ddg_unwrap("https://x.com") == "https://x.com"

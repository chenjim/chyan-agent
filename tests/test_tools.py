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
        assert len(schemas) == 7
        for s in schemas:
            assert s["type"] == "function"
            assert "name" in s["function"]
        names = [s["function"]["name"] for s in schemas]
        assert "read" in names
        assert "write" in names
        assert "search" in names
        assert "grep" in names
        assert "remember" in names
        assert "recall" in names

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

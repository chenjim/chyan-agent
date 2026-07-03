import json
import tempfile
from pathlib import Path

from chyan_agent.memory import MemoryManager


class TestMemoryManager:
    def setup_method(self):
        self.tmpdir = tempfile.mkdtemp()

    def teardown_method(self):
        import shutil
        shutil.rmtree(self.tmpdir)

    def test_init_creates_dir(self):
        path = Path(self.tmpdir) / "new_dir"
        mm = MemoryManager(str(path))
        assert path.exists()
        mm.reset()

    def test_remember_and_recall(self):
        mm = MemoryManager(self.tmpdir, recall_mode="grep")
        mid = mm.remember("favorite language Python", 1)
        assert mid == "m1"
        results = mm.recall("Python", top_k=5)
        assert len(results) == 1
        assert results[0]["content"] == "favorite language Python"
        mm.reset()

    def test_remember_multiple(self):
        mm = MemoryManager(self.tmpdir, recall_mode="grep")
        mm.remember("喜欢Python", 1)
        mm.remember("使用PostgreSQL", 2)
        mm.remember("端口8080", 3)
        all_m = mm.get_all_memories()
        assert len(all_m) == 3
        mm.reset()

    def test_recall_no_match(self):
        mm = MemoryManager(self.tmpdir, recall_mode="grep")
        mm.remember("Python", 1)
        results = mm.recall("Java", top_k=5)
        assert len(results) == 0
        mm.reset()

    def test_save_and_load_conversation(self):
        mm = MemoryManager(self.tmpdir)
        mm.save_turn(1, "user", "hello")
        mm.save_turn(1, "assistant", "hi", [{"name": "test"}])
        conv = mm.load_conversation()
        assert len(conv) == 2
        assert conv[0]["turn"] == 1
        assert conv[0]["role"] == "user"
        assert conv[1]["tool_calls"] == [{"name": "test"}]
        mm.reset()

    def test_meta_roundtrip(self):
        mm = MemoryManager(self.tmpdir)
        meta = {"summary": "test", "recent": [], "total_turns": 5, "version": 1}
        mm.save_meta(meta)
        loaded = mm.load_meta()
        assert loaded["total_turns"] == 5
        assert loaded["version"] == 1
        mm.reset()

    def test_meta_not_found(self):
        mm = MemoryManager(self.tmpdir)
        assert mm.load_meta() is None
        mm.reset()

    def test_rebuild_context(self):
        mm = MemoryManager(self.tmpdir)
        mm.save_turn(1, "user", "q1")
        mm.save_turn(1, "assistant", "a1")
        mm.save_turn(2, "user", "q2")
        mm.save_turn(2, "assistant", "a2")
        summary, recent, total = mm.rebuild_context()
        assert total == 2
        assert len(recent) == 4
        assert recent[0]["content"] == "q1"
        mm.reset()

    def test_reset_cleans_up(self):
        mm = MemoryManager(self.tmpdir)
        mm.remember("test", 1)
        mm.save_turn(1, "user", "hello")
        mm.reset()
        assert mm.get_all_memories() == []
        assert mm.load_conversation() == []

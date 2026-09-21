import json
import time
from types import SimpleNamespace

from chyan_agent import model_windows as mw


class TestBuiltinMatch:
    def test_specific_models(self):
        assert mw.match_builtin("gpt-4o-mini") == 128000
        assert mw.match_builtin("openai/gpt-4o") == 128000
        assert mw.match_builtin("claude-3-5-sonnet") == 200000
        assert mw.match_builtin("deepseek-v4-flash") == 1000000
        assert mw.match_builtin("deepseek-v4.1-flash") == 1000000
        assert mw.match_builtin("kimi-k2.7-code") == 262144
        assert mw.match_builtin("glm-5.2") == 1000000
        assert mw.match_builtin("qwen3.7-plus") == 1000000
        assert mw.match_builtin("mimo-v2.5-pro") == 1048576

    def test_version_suffix_prefix_fallback(self):
        # 具体模型名 + 日期/版本后缀：取最长前缀命中的具体模型
        assert mw.match_builtin("deepseek-v4-flash-0731") == 1000000
        assert mw.match_builtin("claude-sonnet-4-5-20250929") == 1000000
        assert mw.match_builtin("gpt-4o-2024-08-06") == 128000

    def test_segment_boundary(self):
        assert mw.match_builtin("foo1") is None
        assert mw.match_builtin("o1-preview") == 200000

    def test_unknown_or_empty(self):
        assert mw.match_builtin("totally-unknown-model") is None
        assert mw.match_builtin("") is None
        assert mw.match_builtin(None) is None


class TestResolveContextLimit:
    def _cfg(self, **kw):
        base = dict(
            context_limit="auto",
            default_limit=131072,
            online_catalog=False,
            catalog_url="https://example.invalid/api.json",
            catalog_proxy="",
            catalog_cache="/tmp/chyan-nonexistent.json",
            catalog_ttl_hours=1,
        )
        base.update(kw)
        return SimpleNamespace(**base)

    def test_explicit_int_wins(self):
        assert mw.resolve_context_limit("gpt-4o", self._cfg(context_limit=9999)) == (9999, "config")

    def test_explicit_numeric_string(self):
        assert mw.resolve_context_limit("gpt-4o", self._cfg(context_limit="12345")) == (12345, "config")

    def test_builtin(self):
        assert mw.resolve_context_limit("gpt-4o", self._cfg()) == (128000, "builtin")

    def test_fallback(self):
        assert mw.resolve_context_limit("unknown-xyz", self._cfg()) == (131072, "fallback")

    def test_online(self, monkeypatch):
        catalog = {"models": {"unknown-xyz": 4242}, "families": {}}
        monkeypatch.setattr(mw, "fetch_catalog", lambda *a, **k: catalog)
        assert mw.resolve_context_limit("unknown-xyz", self._cfg(online_catalog=True)) == (4242, "online")

    def test_online_overrides_builtin(self, monkeypatch):
        # 目录优先于内置表：内置表是保守近似
        catalog = {"models": {"gpt-4o": 111111}, "families": {}}
        monkeypatch.setattr(mw, "fetch_catalog", lambda *a, **k: catalog)
        assert mw.resolve_context_limit("gpt-4o", self._cfg(online_catalog=True)) == (111111, "online")

    def test_online_miss_uses_builtin_then_fallback(self, monkeypatch):
        monkeypatch.setattr(mw, "fetch_catalog", lambda *a, **k: {})
        assert mw.resolve_context_limit("gpt-4o", self._cfg(online_catalog=True)) == (128000, "builtin")
        assert mw.resolve_context_limit("unknown-xyz", self._cfg(online_catalog=True)) == (131072, "fallback")


class TestWindowTable:
    def test_bundled_table_loaded(self):
        table = mw.load_window_table()
        assert isinstance(table, dict) and len(table) > 100
        assert table["deepseek-v4.1-flash"] == 1000000
        assert table["gpt-4o"] == 128000

    def test_read_structured_json(self, tmp_path):
        p = tmp_path / "w.json"
        p.write_text(json.dumps({"source": "x", "windows": {"My-Model": 12345}}), encoding="utf-8")
        table = mw._read_window_table(str(p))
        assert table == {"my-model": 12345}  # 键归一化为小写

    def test_read_flat_json_and_filter(self, tmp_path):
        p = tmp_path / "w.json"
        p.write_text(json.dumps({"a": 1, "b": 0, "c": -5, "d": "x", "e": True}), encoding="utf-8")
        assert mw._read_window_table(str(p)) == {"a": 1}

    def test_read_missing_or_bad(self, tmp_path):
        assert mw._read_window_table(str(tmp_path / "nope.json")) == {}
        bad = tmp_path / "bad.json"
        bad.write_text("{not json", encoding="utf-8")
        assert mw._read_window_table(str(bad)) == {}

    def test_match_with_custom_table(self):
        table = {"my-model": 5555}
        assert mw.match_builtin("my-model", table) == 5555
        assert mw.match_builtin("provider/my-model", table) == 5555
        assert mw.match_builtin("gpt-4o", table) is None
        assert mw.match_builtin("anything", {}) is None

    def test_resolve_uses_custom_window_table(self, tmp_path):
        p = tmp_path / "w.json"
        p.write_text(json.dumps({"windows": {"my-model": 5555}}), encoding="utf-8")
        cfg = SimpleNamespace(
            context_limit="auto", default_limit=131072, online_catalog=False,
            catalog_url="", catalog_proxy="", catalog_cache="", catalog_ttl_hours=1,
            window_table=str(p),
        )
        assert mw.resolve_context_limit("my-model", cfg) == (5555, "builtin")


class TestCatalog:
    def test_distill_and_match(self):
        raw = {
            "provA": {"models": {"provA/gpt-4o": {"limit": {"context": 128000}, "family": "gpt"}}},
            "provB": {"models": {"provB/gpt-4o": {"limit": {"context": 64000}, "family": "gpt"}}},
        }
        cat = mw._distill(raw)
        assert cat["models"]["gpt-4o"] == 64000
        assert cat["families"]["gpt"] == 64000
        assert mw.match_catalog("gpt-4o", cat) == 64000
        assert mw.match_catalog("some/gpt-4o", cat) == 64000
        assert mw.match_catalog("my-gpt-4o-variant", cat) == 64000

    def test_match_none(self):
        assert mw.match_catalog("nope", {}) is None
        assert mw.match_catalog("", {"models": {"a": 1}}) is None

    def test_cache_expiry(self, tmp_path):
        p = tmp_path / "c.json"
        p.write_text(
            json.dumps({"models": {}, "families": {}, "fetched_at": time.time()}),
            encoding="utf-8",
        )
        cat = mw._load_cache(str(p))
        assert mw._is_expired(cat, 1) is False
        assert mw._is_expired({"fetched_at": time.time() - 7200}, 1) is True
        assert mw._is_expired({}, 1) is True

    def test_fetch_falls_back_to_expired_cache(self, tmp_path, monkeypatch):
        p = tmp_path / "c.json"
        old = {"models": {"m": 1}, "families": {}, "fetched_at": time.time() - 10 ** 9}
        p.write_text(json.dumps(old), encoding="utf-8")

        def boom(*a, **k):
            raise OSError("no net")

        monkeypatch.setattr(mw, "_http_get_json", boom)
        cat = mw.fetch_catalog(url="x", cache_path=str(p), ttl_hours=1)
        assert cat["models"] == {"m": 1}

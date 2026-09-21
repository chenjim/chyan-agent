import tempfile
from pathlib import Path

from chyan_agent.config import Config


def _write_toml(path, data):
    sections = []
    top_keys = []
    for k, v in data.items():
        if isinstance(v, dict):
            sections.append((k, v))
        else:
            top_keys.append((k, v))
    lines = [f'{k} = {_toml_val(v)}' for k, v in top_keys]
    for name, kv in sections:
        lines.append(f"\n[{name}]")
        for k, v in kv.items():
            lines.append(f'{k} = {_toml_val(v)}')
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _toml_val(v):
    if isinstance(v, str):
        return f'"{v}"'
    if isinstance(v, bool):
        return "true" if v else "false"
    return str(v)


class TestConfig:
    def test_load_valid(self):
        data = {
            "prompt": {"system": "You are a test agent."},
            "llm": {"base_url": "https://test.com/v1", "api_key": "sk-test"},
        }
        with tempfile.NamedTemporaryFile(mode="w", suffix=".toml", delete=False) as f:
            path = Path(f.name)
        _write_toml(path, data)

        try:
            cfg = Config.load(str(path))
            assert cfg.llm.base_url == "https://test.com/v1"
            assert cfg.llm.api_key == "sk-test"
            assert cfg.llm.model == "gpt-4o-mini"
            assert cfg.system_prompt == "You are a test agent."
            assert cfg.context.context_limit == "auto"
            assert cfg.context.trigger_ratio == 0.7
            assert cfg.context.trigger_bytes == 204800
            assert cfg.context.compress_target_ratio == 0.8
            assert cfg.context.max_recent_turns == 4
            assert cfg.context.online_catalog is False
            assert cfg.memory.data_dir == "./data"
            assert cfg.tools.read_max_lines == 500
            assert cfg.logging.level == "INFO"
        finally:
            path.unlink()

    def test_load_full(self):
        data = {
            "llm": {
                "base_url": "https://test.com/v1",
                "api_key": "sk-test",
                "model": "gpt-4o",
                "temperature": 0.7,
                "max_tokens": 2048,
            },
            "context": {
                "context_limit": 16000,
                "reserved_tokens": 4000,
                "max_recent_turns": 6,
                "summary_ratio": 0.3,
                "trigger_ratio": 0.8,
                "trigger_bytes": 102400,
                "summary_max_tokens": 2048,
                "online_catalog": True,
                "catalog_proxy": "http://127.0.0.1:7890",
            },
            "memory": {
                "data_dir": "/tmp/test_data",
                "recall_mode": "grep",
                "recall_top_k": 3,
            },
            "prompt": {"system": "custom prompt"},
            "tools": {
                "read_max_lines": 200,
                "read_truncate_mode": "head",
            },
            "logging": {
                "level": "DEBUG",
                "log_file": "/tmp/test.log",
                "max_bytes": 1024,
                "backup_count": 5,
            },
        }
        with tempfile.NamedTemporaryFile(mode="w", suffix=".toml", delete=False) as f:
            path = Path(f.name)
        _write_toml(path, data)

        try:
            cfg = Config.load(str(path))
            assert cfg.llm.model == "gpt-4o"
            assert cfg.llm.temperature == 0.7
            assert cfg.llm.max_tokens == 2048
            assert cfg.context.context_limit == 16000
            assert cfg.context.max_recent_turns == 6
            assert cfg.context.trigger_ratio == 0.8
            assert cfg.context.trigger_bytes == 102400
            assert cfg.context.summary_max_tokens == 2048
            assert cfg.context.online_catalog is True
            assert cfg.context.catalog_proxy == "http://127.0.0.1:7890"
            assert cfg.memory.recall_mode == "grep"
            assert cfg.tools.read_truncate_mode == "head"
            assert cfg.logging.level == "DEBUG"
        finally:
            path.unlink()

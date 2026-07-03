import json
import tempfile
from pathlib import Path

from chyan_agent.config import Config


class TestConfig:
    def test_load_valid(self):
        data = {
            "llm": {"base_url": "https://test.com/v1", "api_key": "sk-test"},
            "system_prompt": "You are a test agent.",
        }
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
            json.dump(data, f)
            path = f.name

        try:
            cfg = Config.load(path)
            assert cfg.llm.base_url == "https://test.com/v1"
            assert cfg.llm.api_key == "sk-test"
            assert cfg.llm.model == "gpt-4o-mini"
            assert cfg.system_prompt == "You are a test agent."
            assert cfg.context.context_limit == 20000
            assert cfg.memory.data_dir == "./data"
            assert cfg.tools.read_max_lines == 500
            assert cfg.logging.level == "INFO"
        finally:
            Path(path).unlink()

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
            },
            "memory": {
                "data_dir": "/tmp/test_data",
                "recall_mode": "grep",
                "recall_top_k": 3,
            },
            "system_prompt": "custom prompt",
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
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
            json.dump(data, f)
            path = f.name

        try:
            cfg = Config.load(path)
            assert cfg.llm.model == "gpt-4o"
            assert cfg.llm.temperature == 0.7
            assert cfg.llm.max_tokens == 2048
            assert cfg.context.context_limit == 16000
            assert cfg.context.max_recent_turns == 6
            assert cfg.memory.recall_mode == "grep"
            assert cfg.tools.read_truncate_mode == "head"
            assert cfg.logging.level == "DEBUG"
        finally:
            Path(path).unlink()

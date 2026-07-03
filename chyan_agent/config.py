import tomllib
from dataclasses import dataclass
from pathlib import Path


@dataclass
class LLMConfig:
    base_url: str
    api_key: str
    model: str = "gpt-4o-mini"
    temperature: float = 0.3
    max_tokens: int = 1024


@dataclass
class ContextConfig:
    context_limit: int = 10000
    reserved_tokens: int = 3500
    max_recent_turns: int = 2
    summary_ratio: float = 0.3


@dataclass
class MemoryConfig:
    data_dir: str = "./data"
    recall_mode: str = "bm25"
    recall_top_k: int = 5


@dataclass
class ToolConfig:
    read_max_lines: int = 500
    read_truncate_mode: str = "auto"
    max_tool_iterations: int = 5


@dataclass
class LoggingConfig:
    level: str = "INFO"
    log_file: str = "./data/agent.log"
    max_bytes: int = 10485760
    backup_count: int = 3


@dataclass
class Config:
    llm: LLMConfig
    context: ContextConfig
    memory: MemoryConfig
    system_prompt: str
    tools: ToolConfig
    logging: LoggingConfig

    @classmethod
    def load(cls, path: str = "config.toml") -> "Config":
        raw = Path(path).read_text(encoding="utf-8")
        data = tomllib.loads(raw)

        llm = LLMConfig(**data.get("llm", {}))
        context = ContextConfig(**data.get("context", {}))
        memory = MemoryConfig(**data.get("memory", {}))
        tools = ToolConfig(**data.get("tools", {}))
        logging = LoggingConfig(**data.get("logging", {}))

        return cls(
            llm=llm,
            context=context,
            memory=memory,
            system_prompt=data.get("system_prompt", ""),
            tools=tools,
            logging=logging,
        )

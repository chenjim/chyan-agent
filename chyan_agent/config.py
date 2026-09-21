import tomllib
from dataclasses import dataclass
from pathlib import Path
from typing import Union


@dataclass
class LLMConfig:
    base_url: str
    api_key: str
    model: str = "gpt-4o-mini"
    temperature: float = 0.1
    max_tokens: int = 4096


@dataclass
class ContextConfig:
    # "auto" 表示按模型自动解析窗口；也可显式写整数（如 10000）覆盖
    context_limit: Union[int, str] = "auto"
    # auto 解析不到时的兜底窗口
    default_limit: int = 131072
    # 为工具结果与模型回复预留的 token 数
    reserved_tokens: int = 4096
    # 压缩触发阈值：占「可用预算」的比例（软阈值）
    trigger_ratio: float = 0.7
    # 压缩触发阈值：原始历史文本字节数（第二道保险，任一满足即压缩）
    trigger_bytes: int = 204800
    # 压缩目标水位：压缩到「触发阈值 * 该比例」，避免临界抖动导致每轮都压缩
    compress_target_ratio: float = 0.8
    # 压缩时保护的最近轮数（不再作为硬性裁剪上限）
    max_recent_turns: int = 4
    # 单条历史摘要的目标压缩比例
    summary_ratio: float = 0.3
    # summary 二级压缩上限；0 表示按 summary_max_ratio 自动
    summary_max_tokens: int = 0
    summary_max_ratio: float = 0.15
    # 内置模型窗口表 JSON 路径；空字符串表示使用随包的 chyan_agent/model_windows.json
    window_table: str = ""
    # 可选联网模型目录（models.dev），默认关闭以保证离线确定性
    online_catalog: bool = False
    catalog_url: str = "https://models.dev/api.json"
    catalog_proxy: str = ""
    catalog_cache: str = "./data/model_catalog.json"
    catalog_ttl_hours: int = 168


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
    tool_output_max_bytes: int = 20000
    work_dir: str = ""
    bash_enabled: bool = False
    bash_timeout: int = 30
    bash_workdir: str = ""
    bash_max_output_bytes: int = 20000
    web_enabled: bool = False
    web_proxy: str = ""
    web_max_bytes: int = 20000
    web_search_url: str = "https://html.duckduckgo.com/html/?q={query}"


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
            system_prompt=data.get("prompt", {}).get("system", ""),
            tools=tools,
            logging=logging,
        )

#!/usr/bin/env python3
"""100 轮压力测试：验证 10K 上下文下 Agent 的稳定性。"""
import json
import logging
import sys
import time
from datetime import datetime
from pathlib import Path

import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from chyan_agent.config import Config
from chyan_agent.llm import LLMClient
from chyan_agent.context import ContextManager
from chyan_agent.memory import MemoryManager
from chyan_agent.tools import ToolExecutor
from chyan_agent.agent import Agent

logger = logging.getLogger("chyan-agent")

SCENARIOS = [
    {
        "type": "linear",
        "description": "独立问题，测试基本上下文管理",
        "turns": 30,
        "prompts": [
            "介绍一下 Python 语言的特点",
            "什么是 Docker 容器技术",
            "解释 REST API 的设计原则",
            "说说微服务架构的优缺点",
            "什么是 CI/CD",
            "解释 SQL 与 NoSQL 数据库的区别",
            "什么是 Git 分支策略",
            "介绍 Linux 文件系统结构",
            "什么是 OAuth 2.0",
            "解释 HTTPS 的工作原理",
            "什么是 Kubernetes",
            "介绍 TCP 与 UDP 的区别",
            "什么是 GraphQL",
            "解释 MVC 架构模式",
            "什么是 Redis 缓存",
            "介绍消息队列的应用场景",
            "什么是 WebSocket",
            "解释 CAP 定理",
            "什么是 JWT 认证",
            "介绍分布式事务的实现方式",
            "什么是缓存击穿和缓存雪崩",
            "解释 RPC 与 REST 的区别",
            "什么是服务网格（Service Mesh）",
            "介绍 CQRS 模式",
            "什么是不可变基础设施",
            "解释蓝绿部署策略",
            "什么是混沌工程",
            "介绍事件溯源架构",
            "什么是零信任安全模型",
            "解释 Sidecar 模式",
        ],
    },
    {
        "type": "dependency",
        "description": "每条输入依赖前几轮结果，测试 recent 保留和 summary 压缩",
        "turns": 40,
        "prompts": [
            "创建一个名为 test.txt 的文件",
            "往 test.txt 里写入 'Hello World'",
            "读取 test.txt 的内容",
            "在 test.txt 末尾追加一行 'Goodbye'",
            "再读一次 test.txt，确认内容",
            "创建 data.json 文件",
            "往 data.json 写入 {\"name\": \"test\", \"version\": 1}",
            "读取 data.json 的内容",
            "修改 data.json 增加字段 \"author\": \"chyan\"",
            "确认 data.json 的最终内容",
            "创建一个名为 src 的目录",
            "在 src 下创建 main.py 文件",
            "往 main.py 写入一个打印 Hello 的 Python 函数",
            "读取 main.py 内容确认",
            "用 search 查找当前目录下的所有 .py 文件",
            "在 src 下创建 utils.py",
            "往 utils.py 写入一个加法函数",
            "用 grep 在 src 中搜索 'def' 关键字",
            "读取 utils.py 确认内容",
            "把 utils.py 复制为 src/utils_backup.py",
            "读取 utils_backup.py 确认",
            "在 src 下创建 __init__.py",
            "查看 src 目录下的所有文件",
            "删除 test.txt 文件",
            "确认 test.txt 已不存在",
            "更新 data.json version 为 2",
            "确认 data.json 最终内容",
            "创建一个笔记文件 notes.md",
            "在 notes.md 中写入今天的测试记录",
            "读取 notes.md 确认内容",
            "在 notes.md 末尾追加一条备注",
            "读取 notes.md 最终内容",
            "用 search 查找所有 .json 文件",
            "在 data.json 中增加字段 \"status\": \"active\"",
            "确认 data.json 完整内容",
            "删除 src/__init__.py",
            "确认 src 目录当前文件列表",
            "创建一个 README.md 写一行项目简介",
            "读取 README.md 确认",
            "清理所有测试文件（删除 src 目录、data.json、notes.md）",
        ],
    },
    {
        "type": "memory_recall",
        "description": "频繁调用 remember/recall，测试长期记忆索引效果",
        "turns": 30,
        "prompts": [
            "记住：我最喜欢的编程语言是 Python",
            "记住：项目数据库使用 PostgreSQL 15",
            "记住：API 服务运行在 8080 端口",
            "记住：日志存储在 /var/log/app",
            "记住：Redis 缓存过期时间是 3600 秒",
            "介绍一下 Rust 语言的特性",
            "什么是函数式编程",
            "解释设计模式中的单例模式",
            "什么是单元测试",
            "介绍 TDD 开发流程",
            "什么是 DDD 领域驱动设计",
            "解释 CNCF 的职责",
            "什么是 eBPF 技术",
            "介绍 WebAssembly 的应用",
            "什么是 RISC-V 架构",
            "我最喜欢的编程语言是什么",
            "项目用的什么数据库",
            "API 服务的端口号是多少",
            "日志文件在哪里",
            "Redis 缓存多久过期",
            "数据库和 Redis 的配置信息都有哪些",
            "解释 AI Agent 的工作原理",
            "什么是 RAG 检索增强生成",
            "介绍 MCP 协议",
            "什么是函数调用（Function Calling）",
            "我的所有偏好设置信息",
            "API 端口和数据库信息汇总",
            "什么是 Agent 编排模式",
            "项目相关的全部配置信息",
            "总结一下你记得的关于我的所有信息",
        ],
    },
]


def setup_logging():
    logger = logging.getLogger("chyan-agent")
    logger.setLevel(logging.WARNING)
    handler = logging.StreamHandler()
    handler.setFormatter(logging.Formatter("[%(levelname)s] %(message)s"))
    logger.addHandler(handler)


def create_agent(config_path: str):
    config = Config.load(config_path)
    llm = LLMClient(config.llm)
    memory = MemoryManager(
        data_dir=config.memory.data_dir,
        recall_mode=config.memory.recall_mode,
        recall_top_k=config.memory.recall_top_k,
    )
    tool_executor = ToolExecutor(memory)
    tool_executor.update_config({
        "read_max_lines": config.tools.read_max_lines,
        "read_truncate_mode": config.tools.read_truncate_mode,
    })
    tool_schemas = tool_executor.get_schemas()
    context = ContextManager(
        context_config=config.context,
        system_prompt=config.system_prompt,
        tool_definitions=tool_schemas,
        llm_client=llm,
    )
    agent = Agent(
        llm_client=llm,
        context_manager=context,
        memory_manager=memory,
        tool_executor=tool_executor,
    )
    return agent, config


def run_turn_safe(agent: Agent, prompt: str):
    try:
        start = time.time()
        response = agent.run_turn(prompt)
        elapsed = time.time() - start
        return response, elapsed, None
    except Exception as e:
        return None, 0, str(e)


def main():
    setup_logging()
    config_path = sys.argv[1] if len(sys.argv) > 1 else "config.toml"

    output_dir = Path("output")
    output_dir.mkdir(exist_ok=True)

    all_records = []
    total_turns = 0

    for scenario in SCENARIOS:
        print(f"\n=== 场景: {scenario['type']} ({scenario['description']}) ===")

        agent, config = create_agent(config_path)
        scenario_turns = 0

        for prompt in scenario["prompts"]:
            total_turns += 1
            scenario_turns += 1
            print(f"  [{scenario['type']}] Turn {total_turns}: {prompt[:40]}...")

            response, elapsed, error = run_turn_safe(agent, prompt)

            s = agent.get_status()

            tool_names = []
            turn_logs = agent.memory.load_conversation()
            last_turn_entries = [e for e in turn_logs if e.get("turn") == agent.current_turn]
            for entry in last_turn_entries:
                if entry.get("tool_calls"):
                    for tc in entry["tool_calls"]:
                        if isinstance(tc, dict) and "name" in tc:
                            tool_names.append(tc["name"])

            record = {
                "turn": total_turns,
                "scenario": scenario["type"],
                "scenario_turn": scenario_turns,
                "prompt": prompt,
                "system_tokens": s["system_tokens"],
                "summary_tokens": s["summary_tokens"],
                "recent_tokens": s["recent_tokens"],
                "total_tokens": s["total_history"],
                "usable_limit": s["usable_limit"],
                "context_limit": s["context_limit"],
                "compression_triggered": s["summary_tokens"] > 0,
                "tool_calls": tool_names,
                "response_length": len(response or ""),
                "elapsed_seconds": round(elapsed, 2),
                "error": error,
                "timestamp": datetime.now().isoformat(),
            }
            all_records.append(record)

            if error:
                print(f"    [错误] {error}")
            else:
                pct = s["total_history"] / s["usable_limit"] * 100 if s["usable_limit"] > 0 else 0
                print(f"    [OK] tokens={s['total_history']}/{s['usable_limit']} ({pct:.0f}%), tools={tool_names}")

        records_file = output_dir / "turns_log.jsonl"
        with open(records_file, "w", encoding="utf-8") as f:
            for r in all_records:
                f.write(json.dumps(r, ensure_ascii=False) + "\n")

        scenarios_file = output_dir / "test_scenarios.json"
        scenario_data = {
            "scenarios": [
                {
                    "type": s["type"],
                    "description": s["description"],
                    "turns": len(s["prompts"]),
                    "prompts": s["prompts"],
                }
                for s in SCENARIOS
            ],
            "metadata": {
                "total_turns": total_turns,
                "created_at": datetime.now().isoformat(),
                "description": "100 轮压力测试场景定义",
            },
        }
        with open(scenarios_file, "w", encoding="utf-8") as f:
            json.dump(scenario_data, f, ensure_ascii=False, indent=2)

    # Generate visualization
    _generate_chart(output_dir / "turns_log.jsonl", output_dir / "context_usage.png")

    print(f"\n=== 测试完成 ===")
    print(f"总轮次: {total_turns}")
    print(f"日志: {records_file}")
    print(f"图表: {output_dir / 'context_usage.png'}")


def _generate_chart(jsonl_path: Path, png_path: Path):
    try:
        import matplotlib.pyplot as plt

        df = pd.read_json(jsonl_path, lines=True)

        fig, (ax1, ax2) = plt.subplots(2, 1, figsize=(12, 8))

        ax1.fill_between(df["turn"], 0, df["system_tokens"], label="System", alpha=0.7)
        ax1.fill_between(
            df["turn"],
            df["system_tokens"],
            df["system_tokens"] + df["summary_tokens"],
            label="Summary",
            alpha=0.7,
        )
        ax1.fill_between(
            df["turn"],
            df["system_tokens"] + df["summary_tokens"],
            df["total_tokens"],
            label="Recent",
            alpha=0.7,
        )
        if "usable_limit" in df.columns:
            ax1.axhline(y=df["usable_limit"].iloc[0], color="r", linestyle="--", label="Usable Limit")
        if "context_limit" in df.columns:
            ax1.axhline(y=df["context_limit"].iloc[0], color="k", linestyle="--", label="Context Limit")
        ax1.set_xlabel("Turn")
        ax1.set_ylabel("Tokens")
        ax1.set_title("上下文占用趋势（堆叠）")
        ax1.legend()
        ax1.grid(alpha=0.3)

        compression_turns = df[df["compression_triggered"] == True]["turn"]
        ax2.scatter(compression_turns, [1] * len(compression_turns), color="red", s=100, alpha=0.6, label="Compression")
        ax2.set_xlabel("Turn")
        ax2.set_yticks([])
        ax2.set_title("压缩触发时间线")
        ax2.legend()
        ax2.grid(axis="x", alpha=0.3)

        plt.tight_layout()
        plt.savefig(png_path, dpi=150)
        print(f"图表已保存到 {png_path}")
    except ImportError as e:
        print(f"可视化依赖缺失，跳过图表生成: {e}")
    except Exception as e:
        print(f"图表生成失败: {e}")


if __name__ == "__main__":
    main()

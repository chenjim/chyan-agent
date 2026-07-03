#!/usr/bin/env python3
import argparse
import json
import logging
import sys
from datetime import datetime
from pathlib import Path
from logging.handlers import RotatingFileHandler
from prompt_toolkit import prompt
from prompt_toolkit.history import FileHistory
from prompt_toolkit.auto_suggest import AutoSuggestFromHistory

from chyan_agent.config import Config
from chyan_agent.llm import LLMClient
from chyan_agent.context import ContextManager
from chyan_agent.memory import MemoryManager
from chyan_agent.tools import ToolExecutor
from chyan_agent.agent import Agent


def setup_logging(config):
    logger = logging.getLogger("chyan-agent")
    logger.setLevel(getattr(logging, config.logging.level))

    log_path = Path(config.logging.log_file)
    log_path.parent.mkdir(parents=True, exist_ok=True)

    handler = RotatingFileHandler(
        str(log_path),
        maxBytes=config.logging.max_bytes,
        backupCount=config.logging.backup_count,
        encoding="utf-8",
    )
    formatter = logging.Formatter(
        "[%(asctime)s] [%(levelname)s] %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )
    handler.setFormatter(formatter)
    logger.addHandler(handler)

    console = logging.StreamHandler()
    console.setLevel(logging.INFO)
    console.setFormatter(formatter)
    logger.addHandler(console)

    return logger


def main():
    parser = argparse.ArgumentParser(description="chyan-agent - 10K 上下文轻量 Agent")
    parser.add_argument("--config", default="config.json", help="配置文件路径")
    parser.add_argument("--resume-from-turn", type=int, default=None, help="从指定轮次恢复")
    parser.add_argument("--rebuild-context", action="store_true", help="强制从 conversation.jsonl 重建上下文")
    args = parser.parse_args()

    config = Config.load(args.config)
    setup_logging(config)
    logger = logging.getLogger("chyan-agent")

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
        max_tool_iterations=config.tools.max_tool_iterations,
    )

    if args.rebuild_context:
        summary, recent, total = memory.rebuild_context()
        context.summary = summary
        context.recent = recent
        agent.current_turn = total
        logger.info(f"强制从 conversation.jsonl 重建: {total} 轮")
    else:
        agent._try_recover()

    if args.resume_from_turn is not None:
        agent.current_turn = args.resume_from_turn - 1
        logger.info(f"从 Turn {args.resume_from_turn} 继续")

    commands = {
        "/reset": "清空当前对话上下文与记忆",
        "/status": "打印当前上下文占用情况",
        "/memories": "列出所有长期记忆",
        "/export": "导出对话历史为 JSON",
        "/help": "显示命令列表",
        "/exit": "退出",
    }

    print("chyan-agent - 10K 上下文轻量 Agent")
    print("输入 /help 查看命令列表")
    print()

    history = FileHistory(str(Path(config.memory.data_dir) / ".input_history"))

    while True:
        try:
            user_input = prompt("chyan-agent > ", history=history, auto_suggest=AutoSuggestFromHistory()).strip()
        except (EOFError, KeyboardInterrupt):
            print()
            break

        if not user_input:
            continue

        if user_input == "/exit":
            break
        elif user_input == "/help":
            for cmd, desc in commands.items():
                print(f"  {cmd:<12} {desc}")
            continue
        elif user_input == "/reset":
            agent.reset()
            print("已清空所有对话历史和记忆")
            continue
        elif user_input == "/status":
            s = agent.get_status()
            pct = s["total_history"] / s["usable_limit"] * 100 if s["usable_limit"] > 0 else 0
            print(
                f"system={s['system_tokens']}, "
                f"summary={s['summary_tokens']}, "
                f"recent={s['recent_tokens']}, "
                f"total={s['total_history']}/{s['usable_limit']} "
                f"({pct:.0f}%)"
            )
            continue
        elif user_input == "/memories":
            memories = memory.get_all_memories()
            if not memories:
                print("暂无记忆")
            else:
                for m in memories:
                    print(f"  [{m['id']}] {m['content']} (Turn {m['source_turn']})")
            continue
        elif user_input == "/export":
            conversations = memory.load_conversation()
            timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
            export_path = Path(config.memory.data_dir) / f"export_{timestamp}.json"
            with open(export_path, "w", encoding="utf-8") as f:
                json.dump(conversations, f, ensure_ascii=False, indent=2)
            print(f"已导出到 {export_path}")
            continue

        try:
            response = agent.run_turn(user_input)
            print(response)
        except Exception as e:
            logger.error(f"运行出错: {e}", exc_info=True)
            print(f"错误: {e}")

    print("再见！")


if __name__ == "__main__":
    main()

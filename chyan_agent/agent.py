import json
import logging
from datetime import datetime

from .context import ContextManager
from .llm import LLMClient, LLMResponse
from .memory import MemoryManager
from .tools import ToolExecutor

logger = logging.getLogger("chyan-agent")


class Agent:
    MAX_TOOL_ITERATIONS = 5

    def __init__(
        self,
        llm_client: LLMClient,
        context_manager: ContextManager,
        memory_manager: MemoryManager,
        tool_executor: ToolExecutor,
    ):
        self.llm = llm_client
        self.context = context_manager
        self.memory = memory_manager
        self.tool_executor = tool_executor

        self.current_turn = 0

    def _try_recover(self) -> bool:
        meta = self.memory.load_meta()
        if meta and meta.get("version") == 1:
            self.context.summary = meta.get("summary", "")
            self.context.recent = meta.get("recent", [])
            self.current_turn = meta.get("total_turns", 0)
            logger.info(
                f"从 meta.json 恢复: summary={self.context.summary_tokens_count} tokens, "
                f"recent={len(self.context.recent) // 2} 轮, "
                f"从 Turn {self.current_turn + 1} 继续"
            )
            return True
        else:
            summary, recent, total = self.memory.rebuild_context()
            self.context.summary = summary
            self.context.recent = recent
            self.current_turn = total
            if total > 0:
                logger.info(f"从 conversation.jsonl 重建: {total} 轮")
            return total > 0

    def _save_meta(self):
        meta = {
            "summary": self.context.summary,
            "recent": self.context.recent,
            "total_turns": self.current_turn,
            "last_updated": datetime.now().isoformat(),
            "version": 1,
        }
        self.memory.save_meta(meta)

    def run_turn(self, user_input: str) -> str:
        self.current_turn += 1
        self.tool_executor.current_turn = self.current_turn

        messages = self.context.build_messages(user_input)
        tool_schemas = self.tool_executor.get_schemas()

        for iteration in range(self.MAX_TOOL_ITERATIONS):
            response = self.llm.chat(messages, tools=tool_schemas)

            if not response.tool_calls:
                break

            assistant_msg = {"role": "assistant", "content": None}
            assistant_msg["tool_calls"] = [
                {
                    "id": tc.id,
                    "type": "function",
                    "function": {
                        "name": tc.function_name,
                        "arguments": tc.function_arguments,
                    },
                }
                for tc in response.tool_calls
            ]
            messages.append(assistant_msg)

            for tc in response.tool_calls:
                try:
                    kwargs = json.loads(tc.function_arguments)
                except json.JSONDecodeError:
                    kwargs = {}
                result = self.tool_executor.execute(tc.function_name, **kwargs)
                messages.append({
                    "role": "tool",
                    "tool_call_id": tc.id,
                    "content": json.dumps(result, ensure_ascii=False),
                })
        else:
            return "工具调用次数超过限制，请简化你的请求或分步操作"

        assistant_content = response.content or ""

        self.context.add_turn(user_input, assistant_content)
        self.memory.save_turn(self.current_turn, "user", user_input)
        tool_calls_data = (
            [{"name": tc.function_name, "arguments": tc.function_arguments}
             for tc in response.tool_calls]
            if response.tool_calls
            else []
        )
        self.memory.save_turn(self.current_turn, "assistant", assistant_content, tool_calls_data)
        self._save_meta()

        logger.info(
            f"Turn {self.current_turn}: "
            f"tokens={self.context.current_tokens}/{self.context.usable_tokens}"
        )

        return assistant_content

    def get_status(self) -> dict:
        return {
            "current_turn": self.current_turn,
            "system_tokens": self.context.system_tokens,
            "summary_tokens": self.context.summary_tokens_count,
            "recent_tokens": self.context.recent_tokens_count,
            "total_history": self.context.current_tokens,
            "usable_limit": self.context.usable_tokens,
            "context_limit": self.context.config.context_limit,
            "summary_length": len(self.context.summary),
            "recent_turns": len(self.context.recent) // 2,
            "memories_count": len(self.memory.get_all_memories()),
        }

    def reset(self):
        self.memory.reset()
        self.context.reset()
        self.current_turn = 0
        logger.info("Agent 已重置")

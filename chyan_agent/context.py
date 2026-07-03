import json
import logging
from typing import Optional

import tiktoken

logger = logging.getLogger("chyan-agent")


class ContextManager:
    def __init__(
        self,
        context_config,
        system_prompt: str,
        tool_definitions: list,
        encoding_name: str = "o200k_base",
        llm_client=None,
    ):
        self.config = context_config
        self.system_prompt = system_prompt
        self.tool_definitions = tool_definitions
        self.llm_client = llm_client

        self.encoding = tiktoken.get_encoding(encoding_name)

        self.summary = ""
        self.recent = []

        self.system_tokens = self._count_text_tokens(system_prompt)
        self.tools_tokens = self._count_text_tokens(json.dumps(tool_definitions, ensure_ascii=False))
        self.fixed_cost = self.system_tokens + self.tools_tokens
        self.usable_tokens = (
            context_config.context_limit
            - self.fixed_cost
            - context_config.reserved_tokens
        )

        logger.info(
            f"上下文预算: total={context_config.context_limit}, "
            f"fixed={self.fixed_cost}, reserved={context_config.reserved_tokens}, "
            f"usable={self.usable_tokens}"
        )

    def _count_text_tokens(self, text: str) -> int:
        return len(self.encoding.encode(text))

    def _count_messages_tokens(self, messages: list[dict]) -> int:
        total = 0
        for msg in messages:
            total += self._count_text_tokens(msg.get("role", ""))
            content = msg.get("content", "") or ""
            total += self._count_text_tokens(content)
            if "tool_calls" in msg and msg["tool_calls"]:
                for tc in msg["tool_calls"]:
                    fn = tc.get("function", {})
                    total += self._count_text_tokens(fn.get("name", ""))
                    total += self._count_text_tokens(fn.get("arguments", ""))
            total += 4
        return total

    def _count_history_tokens(self) -> int:
        summary_tokens = self._count_text_tokens(self.summary) if self.summary else 0
        recent_tokens = self._count_messages_tokens(self.recent)
        return summary_tokens + recent_tokens

    def _simple_summarize(self, text: str) -> str:
        ratio = self.config.summary_ratio
        max_chars = int(len(text) * ratio)
        if len(text) <= max_chars:
            return text
        return text[:max_chars] + "…"

    def _llm_summarize(self, text: str) -> str:
        ratio = self.config.summary_ratio
        prompt = (
            f"请把以下对话内容压缩为关键事实列表。保留：实体、决策、数值、待办。"
            f"删除：寒暄、重复、语气词。\n"
            f"输出用简洁中文 bullet points，不超过原文的 {int(ratio * 100)}% 长度。\n\n{text}"
        )
        try:
            response = self.llm_client.chat([
                {"role": "user", "content": prompt}
            ])
            return response.content.strip()
        except Exception as e:
            logger.warning(f"LLM 摘要失败，使用简单截断: {e}")
            return self._simple_summarize(text)

    def _compress(self):
        if len(self.recent) < 2:
            return

        user_msg = self.recent.pop(0)
        assistant_msg = self.recent.pop(0)

        turn_text = (
            f"User: {user_msg.get('content', '')}\n"
            f"Assistant: {assistant_msg.get('content', '')}"
        )

        if self.llm_client:
            summary_text = self._llm_summarize(turn_text)
        else:
            summary_text = self._simple_summarize(turn_text)

        if self.summary:
            self.summary += "\n" + summary_text
        else:
            self.summary = summary_text

        logger.debug(
            f"压缩: 将一轮移入 summary, "
            f"节省 ~{self._count_text_tokens(turn_text)} tokens"
        )

    def build_messages(self, user_input: str) -> list[dict]:
        user_tokens = self._count_text_tokens(user_input)
        history_tokens = self._count_history_tokens()
        total_needed = history_tokens + user_tokens

        while total_needed > self.usable_tokens and len(self.recent) >= 2:
            self._compress()
            history_tokens = self._count_history_tokens()
            total_needed = history_tokens + user_tokens

        messages = [{"role": "system", "content": self.system_prompt}]
        if self.summary:
            messages.append({
                "role": "system",
                "content": f"[对话摘要]\n{self.summary}",
            })
        messages.extend(self.recent)
        messages.append({"role": "user", "content": user_input})

        current_total = self._count_messages_tokens(messages)
        logger.debug(
            f"构建上下文: system={self.system_tokens}, "
            f"summary={self._count_text_tokens(self.summary)}, "
            f"recent={self._count_messages_tokens(self.recent)}, "
            f"user={user_tokens}, total={current_total}/{self.usable_tokens}"
        )

        return messages

    def add_turn(self, user_content: str, assistant_content: str):
        self.recent.append({"role": "user", "content": user_content})
        self.recent.append({"role": "assistant", "content": assistant_content})

        max_messages = self.config.max_recent_turns * 2
        if len(self.recent) > max_messages:
            excess = len(self.recent) - max_messages
            if excess % 2 != 0:
                excess += 1
            self.recent = self.recent[excess:]

    def reset(self):
        self.summary = ""
        self.recent = []

    @property
    def current_tokens(self):
        return self._count_history_tokens()

    @property
    def summary_tokens_count(self):
        return self._count_text_tokens(self.summary) if self.summary else 0

    @property
    def recent_tokens_count(self):
        return self._count_messages_tokens(self.recent)

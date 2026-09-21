import json
import logging
from typing import Optional

import tiktoken

logger = logging.getLogger("chyan-agent")

# 单次 build_messages 内最多压缩轮数，异常情况下防止死循环
_MAX_COMPRESS_ROUNDS = 16


class ContextManager:
    """自适应上下文预算管理。

    预算模型::

        fixed_cost   = tokens(system_prompt) + tokens(tool_definitions)
        hard_limit   = context_limit - fixed_cost - reserved_tokens   # 历史硬上限
        soft_limit   = hard_limit * trigger_ratio                     # 压缩触发线
        触发条件      = history_tokens + 本轮 > soft_limit
                       或 history_bytes + 本轮 > trigger_bytes        # 任一满足

    压缩策略：把最老的若干轮批量摘要进 ``summary``（保护最近 ``max_recent_turns`` 轮），
    ``summary`` 超限时再做二级压缩（先再摘要，仍超则 FIFO 截断保留尾部）。
    """

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
        self.tools_tokens = self._count_text_tokens(
            json.dumps(tool_definitions, ensure_ascii=False)
        )
        self.fixed_cost = self.system_tokens + self.tools_tokens

        self.reserved_tokens = self._cfg("reserved_tokens", 4096)
        self.usable_tokens = max(
            0, context_config.context_limit - self.fixed_cost - self.reserved_tokens
        )
        self.trigger_ratio = self._cfg("trigger_ratio", 0.7)
        self.soft_limit_tokens = int(self.usable_tokens * self.trigger_ratio)
        self.trigger_bytes = self._cfg("trigger_bytes", 204800)

        logger.info(
            f"上下文预算: total={context_config.context_limit}, fixed={self.fixed_cost}, "
            f"reserved={self.reserved_tokens}, hard={self.usable_tokens}, "
            f"soft={self.soft_limit_tokens} ({self.trigger_ratio:.0%}), "
            f"bytes_trigger={self.trigger_bytes}"
        )

    def _cfg(self, name, default):
        value = getattr(self.config, name, default)
        return default if value is None else value

    # ------------------------------------------------------------------ 计数

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
        return summary_tokens + self._count_messages_tokens(self.recent)

    def _history_bytes(self, include_summary: bool = True) -> int:
        total = 0
        if include_summary and self.summary:
            total += len(self.summary.encode("utf-8"))
        for msg in self.recent:
            total += len((msg.get("content") or "").encode("utf-8"))
        return total

    # -------------------------------------------------------------- 摘要生成

    def _simple_summarize(self, text: str, ratio: Optional[float] = None) -> str:
        ratio = self._cfg("summary_ratio", 0.3) if ratio is None else ratio
        max_chars = max(1, int(len(text) * ratio))
        if len(text) <= max_chars:
            return text
        return text[:max_chars] + "…"

    def _llm_summarize(self, text: str, ratio: Optional[float] = None) -> str:
        ratio = self._cfg("summary_ratio", 0.3) if ratio is None else ratio
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
            return self._simple_summarize(text, ratio)

    def _turns_to_text(self, messages: list[dict]) -> str:
        parts = []
        for msg in messages:
            role = "User" if msg.get("role") == "user" else "Assistant"
            parts.append(f"{role}: {msg.get('content', '')}")
        return "\n".join(parts)

    def _compress_turns(self, messages: list[dict]) -> str:
        text = self._turns_to_text(messages)
        if self.llm_client:
            return self._llm_summarize(text)
        return self._simple_summarize(text)

    def _append_summary(self, text: str):
        if not text:
            return
        self.summary = f"{self.summary}\n{text}" if self.summary else text

    # ---------------------------------------------------------- 二级压缩

    def _summary_cap_tokens(self) -> int:
        explicit = self._cfg("summary_max_tokens", 0)
        if explicit and explicit > 0:
            return explicit
        return max(1024, int(self.usable_tokens * self._cfg("summary_max_ratio", 0.15)))

    def _shrink_summary(self) -> bool:
        """summary 超上限时收缩：先再摘要（仅严重超限时），仍超则 FIFO 截断保留尾部。"""
        cap = self._summary_cap_tokens()
        if cap <= 0 or self.summary_tokens_count <= cap:
            return False

        current = self.summary_tokens_count
        if self.llm_client and current > cap * 2:
            self.summary = self._llm_summarize(self.summary, ratio=cap / current)
        if self.summary_tokens_count > cap:
            ellipsis = "…"
            keep = max(1, cap - self._count_text_tokens(ellipsis))
            tokens = self.encoding.encode(self.summary)
            self.summary = ellipsis + self.encoding.decode(tokens[-keep:])
            if self.summary_tokens_count > cap:  # tokenizer 漂移兜底
                self.summary = self.encoding.decode(
                    self.encoding.encode(self.summary)[-cap:]
                )

        logger.debug(
            f"二级压缩: summary {current} -> {self.summary_tokens_count} tokens (cap={cap})"
        )
        return True

    # ------------------------------------------------------------ 压缩调度

    def _protected_messages(self) -> int:
        return max(1, int(self._cfg("max_recent_turns", 4))) * 2

    def _pop_oldest_turn(self) -> list[dict]:
        moved = []
        if self.recent:
            moved.append(self.recent.pop(0))
        if self.recent and self.recent[0].get("role") == "assistant":
            moved.append(self.recent.pop(0))
        return moved

    def _need_compress(self, extra_tokens: int, extra_bytes: int) -> bool:
        tokens = self.summary_tokens_count + self.recent_tokens_count + extra_tokens
        if tokens > self.soft_limit_tokens:
            return True
        return (self._history_bytes() + extra_bytes) > self.trigger_bytes

    def _over_target(self, extra_tokens: int, extra_bytes: int) -> bool:
        tokens = self.summary_tokens_count + self.recent_tokens_count + extra_tokens
        if tokens > self.target_tokens:
            return True
        return (self._history_bytes() + extra_bytes) > self.target_bytes

    def _compress_to_fit(self, extra_tokens: int, extra_bytes: int):
        protected = self._protected_messages()
        for _ in range(_MAX_COMPRESS_ROUNDS):
            if not self._need_compress(extra_tokens, extra_bytes):
                return
            # 批量弹出最老的若干轮，压到目标水位以下，只调用一次摘要，降低压缩开销
            if len(self.recent) > protected and self.summary_tokens_count < self.soft_limit_tokens:
                moved = []
                while len(self.recent) > protected and self._over_target(
                    extra_tokens, extra_bytes
                ):
                    moved.extend(self._pop_oldest_turn())
                if moved:
                    self._append_summary(self._compress_turns(moved))
                    self._shrink_summary()
                    continue
            if self._shrink_summary():
                continue
            # 底线保护：仅当清空摘要确实能回到阈值内时才清，避免无谓信息损失
            if self.summary and self._fits_without_summary(extra_tokens, extra_bytes):
                logger.warning("极端超限：清空摘要以保护最近对话")
                self.summary = ""
                continue
            break  # 无法继续压缩，交由上层做硬顶处理

    def _fits_without_summary(self, extra_tokens: int, extra_bytes: int) -> bool:
        tokens = self.recent_tokens_count + extra_tokens
        bytes_ = self._history_bytes(include_summary=False) + extra_bytes
        return tokens <= self.soft_limit_tokens and bytes_ <= self.trigger_bytes

    # -------------------------------------------------------------- 对外接口

    def build_messages(self, user_input: str) -> list[dict]:
        user_tokens = self._count_text_tokens(user_input)
        user_bytes = len(user_input.encode("utf-8"))
        self._compress_to_fit(user_tokens, user_bytes)

        messages = [{"role": "system", "content": self.system_prompt}]
        if self.summary:
            messages.append({
                "role": "system",
                "content": f"[对话摘要]\n{self.summary}",
            })
        messages.extend(self.recent)
        messages.append({"role": "user", "content": user_input})

        current_total = self._count_messages_tokens(messages)
        prompt_total = current_total + self.tools_tokens
        if prompt_total > self.config.context_limit:
            logger.warning(
                f"上下文仍超出窗口: {prompt_total} > {self.config.context_limit}"
            )
        logger.debug(
            f"构建上下文: system={self.system_tokens}, "
            f"summary={self.summary_tokens_count}, "
            f"recent={self.recent_tokens_count}, "
            f"user={user_tokens}, total={current_total}/{self.usable_tokens}"
        )

        return messages

    def add_turn(self, user_content: str, assistant_content: str):
        """追加一轮完整对话。不做硬性裁剪，由 build_messages 按阈值压缩。"""
        self.recent.append({"role": "user", "content": user_content})
        self.recent.append({"role": "assistant", "content": assistant_content})

    def reset(self):
        self.summary = ""
        self.recent = []

    @property
    def current_tokens(self):
        return self._count_history_tokens()

    @property
    def current_bytes(self):
        return self._history_bytes()

    @property
    def summary_tokens_count(self):
        return self._count_text_tokens(self.summary) if self.summary else 0

    @property
    def recent_tokens_count(self):
        return self._count_messages_tokens(self.recent)

    @property
    def summary_cap_tokens(self):
        return self._summary_cap_tokens()

    @property
    def target_tokens(self):
        return int(self.soft_limit_tokens * self._cfg("compress_target_ratio", 0.8))

    @property
    def target_bytes(self):
        return int(self.trigger_bytes * self._cfg("compress_target_ratio", 0.8))

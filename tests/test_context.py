from unittest.mock import Mock

from chyan_agent.context import ContextManager
from chyan_agent.config import ContextConfig


def _make_ctx(context_limit=10000, reserved_tokens=3500, max_recent_turns=2,
              trigger_ratio=0.7, trigger_bytes=204800, llm_client=None, **kwargs):
    cfg = ContextConfig(
        context_limit=context_limit,
        reserved_tokens=reserved_tokens,
        max_recent_turns=max_recent_turns,
        trigger_ratio=trigger_ratio,
        trigger_bytes=trigger_bytes,
        **kwargs,
    )
    return ContextManager(
        context_config=cfg,
        system_prompt="You are a test agent.",
        tool_definitions=[],
        llm_client=llm_client,
    )


class TestContextManager:
    def test_build_messages_empty(self):
        ctx = _make_ctx()
        msgs = ctx.build_messages("hello")
        assert len(msgs) == 2
        assert msgs[0]["role"] == "system"
        assert msgs[-1]["role"] == "user"
        assert msgs[-1]["content"] == "hello"

    def test_add_turn(self):
        ctx = _make_ctx()
        ctx.add_turn("hello", "hi there")
        assert len(ctx.recent) == 2
        assert ctx.recent[0]["role"] == "user"
        assert ctx.recent[1]["role"] == "assistant"

    def test_add_turn_does_not_hard_trim(self):
        # 新语义：add_turn 不再硬性裁剪，由 build_messages 按阈值压缩
        ctx = _make_ctx(max_recent_turns=2)
        for i in range(5):
            ctx.add_turn(f"q{i}", f"a{i}")
        assert len(ctx.recent) == 10

    def test_build_with_history(self):
        ctx = _make_ctx()
        ctx.add_turn("hello", "hi")
        ctx.add_turn("how are you", "I'm fine")
        msgs = ctx.build_messages("good")
        assert len(msgs) == 2 + 4  # system + user + 4 history
        roles = [m["role"] for m in msgs]
        assert roles == ["system", "user", "assistant", "user", "assistant", "user"]

    def test_budget_properties(self):
        ctx = _make_ctx(context_limit=10000, reserved_tokens=3500, trigger_ratio=0.7)
        assert ctx.usable_tokens == 10000 - ctx.fixed_cost - 3500
        assert ctx.soft_limit_tokens == int(ctx.usable_tokens * 0.7)
        assert ctx.trigger_bytes == 204800

    def test_token_trigger_compresses(self):
        ctx = _make_ctx(max_recent_turns=2)
        ctx.soft_limit_tokens = 1
        ctx.trigger_bytes = 10 ** 9
        for i in range(4):
            ctx.add_turn("long question " * 20, "long answer " * 20)
        ctx.build_messages("new input")
        assert ctx.summary != ""
        assert len(ctx.recent) <= 4  # 保护最近 max_recent_turns 轮

    def test_byte_trigger_compresses(self):
        ctx = _make_ctx(max_recent_turns=2)
        ctx.soft_limit_tokens = 10 ** 9
        ctx.trigger_bytes = 50
        for i in range(4):
            ctx.add_turn("question-" + "x" * 40, "answer-" + "y" * 40)
        ctx.build_messages("z" * 40)
        assert ctx.summary != ""

    def test_protected_recent_turns(self):
        ctx = _make_ctx(max_recent_turns=3)
        ctx.soft_limit_tokens = 1
        ctx.trigger_bytes = 10 ** 9
        for i in range(10):
            ctx.add_turn(f"question {i} " * 10, f"answer {i} " * 10)
        ctx.build_messages("final")
        # 至少保留保护轮数，且最后一轮仍在 recent
        assert len(ctx.recent) >= 6
        assert ctx.recent[-1]["content"].startswith("answer 9")

    def test_compression_batches_into_single_llm_call(self):
        llm = Mock()
        llm.chat.return_value = Mock(content="摘要")
        ctx = _make_ctx(max_recent_turns=1, llm_client=llm)
        ctx.soft_limit_tokens = 1
        ctx.trigger_bytes = 10 ** 9
        for i in range(5):
            ctx.add_turn("q" * 100, "a" * 100)
        ctx.build_messages("new")
        assert llm.chat.call_count == 1
        assert ctx.summary == "摘要"

    def test_summary_secondary_compression(self):
        ctx = _make_ctx(summary_max_tokens=50)
        ctx.summary = " ".join(f"fact-{i}" for i in range(400))
        assert ctx.summary_tokens_count > 50
        changed = ctx._shrink_summary()
        assert changed is True
        assert ctx.summary_tokens_count <= 50
        assert ctx.summary.startswith("…")

    def test_summary_cap_auto_ratio(self):
        ctx = _make_ctx(context_limit=10000, reserved_tokens=3500, summary_max_ratio=0.1)
        assert ctx.summary_cap_tokens == max(1024, int(ctx.usable_tokens * 0.1))

    def test_target_watermark_derives_from_trigger(self):
        ctx = _make_ctx(
            context_limit=10000, reserved_tokens=3500, trigger_ratio=0.7,
            trigger_bytes=204800, compress_target_ratio=0.5,
        )
        assert ctx.target_tokens == int(ctx.soft_limit_tokens * 0.5)
        assert ctx.target_bytes == int(ctx.trigger_bytes * 0.5)

    def test_compress_lands_below_trigger(self):
        llm = Mock()
        llm.chat.return_value = Mock(content="摘要")
        ctx = _make_ctx(max_recent_turns=1, llm_client=llm)
        ctx.soft_limit_tokens = 30
        ctx.trigger_bytes = 10 ** 9
        for i in range(6):
            ctx.add_turn(f"question {i}", f"answer {i}")
        ctx.build_messages("final")
        assert ctx.summary != ""
        # 压到目标水位后，短期内不应再次触发压缩
        assert not ctx._need_compress(0, 0)

    def test_summary_tokens_count(self):
        ctx = _make_ctx()
        assert ctx.summary_tokens_count == 0
        ctx.summary = "test summary content"
        assert ctx.summary_tokens_count > 0

    def test_reset(self):
        ctx = _make_ctx()
        ctx.add_turn("hello", "hi")
        ctx.summary = "some summary"
        ctx.reset()
        assert ctx.summary == ""
        assert ctx.recent == []

    def test_token_counting_consistency(self):
        ctx = _make_ctx()
        ctx.add_turn("hello world", "hi there")
        recent_tokens = ctx.recent_tokens_count
        assert recent_tokens > 0
        assert isinstance(recent_tokens, int)

    def test_build_messages_after_compress(self):
        ctx = _make_ctx()
        for i in range(6):
            ctx.add_turn(f"test input {i}", f"test response {i}")
        msgs = ctx.build_messages("final input")
        assert msgs[-1]["content"] == "final input"

    def test_history_bytes(self):
        ctx = _make_ctx()
        ctx.add_turn("abc", "defg")
        assert ctx.current_bytes == len("abcdefg")
        ctx.summary = "hh"
        assert ctx.current_bytes == len("abcdefghh")
        assert ctx._history_bytes(include_summary=False) == len("abcdefg")

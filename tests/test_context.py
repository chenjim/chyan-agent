from chyan_agent.context import ContextManager
from chyan_agent.config import ContextConfig


def _make_ctx(max_recent_turns=4):
    cfg = ContextConfig(
        context_limit=10000,
        reserved_tokens=3500,
        max_recent_turns=2,
        summary_ratio=0.3,
    )
    return ContextManager(
        context_config=cfg,
        system_prompt="You are a test agent.",
        tool_definitions=[],
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

    def test_build_with_history(self):
        ctx = _make_ctx()
        ctx.add_turn("hello", "hi")
        ctx.add_turn("how are you", "I'm fine")
        msgs = ctx.build_messages("good")
        assert len(msgs) == 2 + 4  # system + user + 4 history
        roles = [m["role"] for m in msgs]
        assert roles == ["system", "user", "assistant", "user", "assistant", "user"]

    def test_recent_trimming(self):
        ctx = _make_ctx(max_recent_turns=2)
        for i in range(5):
            ctx.add_turn(f"q{i}", f"a{i}")
        assert len(ctx.recent) <= 4  # max_recent_turns * 2

    def test_compress_simple(self):
        ctx = _make_ctx()
        # Fill enough turns to trigger compression
        ctx.usable_tokens = 1  # Force compression immediately
        ctx.add_turn("long question " * 50, "long answer " * 50)
        ctx.build_messages("new input")
        assert len(ctx.recent) < 2 or ctx.summary != ""

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
        before = ctx.current_tokens
        original_recent_len = len(ctx.recent)
        msgs = ctx.build_messages("final input")
        # Should not crash, messages have correct structure
        assert msgs[-1]["content"] == "final input"

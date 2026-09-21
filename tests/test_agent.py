import tempfile
from dataclasses import dataclass
from unittest.mock import Mock, patch

from chyan_agent.context import ContextManager
from chyan_agent.memory import MemoryManager
from chyan_agent.tools import ToolExecutor
from chyan_agent.agent import Agent


@dataclass
class FakeContextConfig:
    context_limit: int = 10000
    default_limit: int = 32768
    reserved_tokens: int = 3500
    trigger_ratio: float = 0.7
    trigger_bytes: int = 204800
    max_recent_turns: int = 2
    summary_ratio: float = 0.3
    summary_max_tokens: int = 0
    summary_max_ratio: float = 0.15


@dataclass
class FakeLLMConfig:
    base_url: str = "https://test.com/v1"
    api_key: str = "sk-test"
    model: str = "gpt-4o-mini"
    temperature: float = 0.3
    max_tokens: int = 1024


class TestAgent:
    def setup_method(self):
        self.tmpdir = tempfile.mkdtemp()
        self.memory = MemoryManager(self.tmpdir)
        self.tool_executor = ToolExecutor(self.memory)
        self.tool_executor.update_config({"read_max_lines": 500, "read_truncate_mode": "auto"})
        self.context = ContextManager(
            context_config=FakeContextConfig(),
            system_prompt="You are a test agent.",
            tool_definitions=self.tool_executor.get_schemas(),
        )
        self.llm = Mock()

    def teardown_method(self):
        import shutil
        shutil.rmtree(self.tmpdir)

    def _make_agent(self):
        return Agent(
            llm_client=self.llm,
            context_manager=self.context,
            memory_manager=self.memory,
            tool_executor=self.tool_executor,
        )

    def test_run_turn_simple(self):
        self.llm.chat.return_value = Mock(
            content="Hello!",
            tool_calls=[],
            usage={"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
        )
        agent = self._make_agent()
        response = agent.run_turn("hi")
        assert response == "Hello!"
        assert agent.current_turn == 1

    def test_run_turn_with_tool_call(self):
        from chyan_agent.llm import ToolCallInfo

        # First call returns tool call
        tool_call = ToolCallInfo(
            id="call_1",
            type="function",
            function_name="read",
            function_arguments='{"path": "test.txt"}',
        )
        first_response = Mock(
            content=None,
            tool_calls=[tool_call],
            usage={"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
        )
        # Second call returns final response
        second_response = Mock(
            content="File read successfully.",
            tool_calls=[],
            usage={"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30},
        )
        self.llm.chat.side_effect = [first_response, second_response]

        agent = self._make_agent()
        response = agent.run_turn("read test.txt")
        assert response == "File read successfully."
        assert self.llm.chat.call_count == 2

    def test_run_turn_tool_iteration_limit(self):
        from chyan_agent.llm import ToolCallInfo

        tool_call = ToolCallInfo(
            id="call_loop",
            type="function",
            function_name="search",
            function_arguments='{"path": ".", "pattern": "*"}',
        )
        looping_response = Mock(
            content=None,
            tool_calls=[tool_call],
            usage={"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
        )
        self.llm.chat.return_value = looping_response

        agent = self._make_agent()
        response = agent.run_turn("search everything")
        assert "工具执行结果过大" in response or "超过限制" in response

    def test_get_status(self):
        self.llm.chat.return_value = Mock(
            content="ok",
            tool_calls=[],
            usage={"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
        )
        agent = self._make_agent()
        agent.run_turn("hello")
        agent.run_turn("world")
        status = agent.get_status()
        assert status["current_turn"] == 2
        assert "system_tokens" in status
        assert "total_history" in status
        assert "memories_count" in status

    def test_reset(self):
        self.llm.chat.return_value = Mock(
            content="ok",
            tool_calls=[],
            usage={"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
        )
        agent = self._make_agent()
        agent.run_turn("hello")
        agent.run_turn("world")
        assert agent.current_turn == 2
        agent.reset()
        assert agent.current_turn == 0
        assert self.context.recent == []

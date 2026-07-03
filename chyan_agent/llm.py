import time
import logging
from dataclasses import dataclass
from typing import Optional

from openai import OpenAI, APITimeoutError, RateLimitError, APIStatusError, OpenAIError

from .config import LLMConfig

logger = logging.getLogger("chyan-agent")


@dataclass
class ToolCallInfo:
    id: str
    type: str
    function_name: str
    function_arguments: str


@dataclass
class LLMResponse:
    content: str
    tool_calls: list[ToolCallInfo]
    usage: dict


class LLMClient:
    def __init__(self, config: LLMConfig):
        self.config = config
        self.client = OpenAI(
            base_url=config.base_url,
            api_key=config.api_key,
        )

    def chat(self, messages: list, tools: Optional[list] = None) -> LLMResponse:
        last_error = None
        for attempt in range(3):
            try:
                kwargs = dict(
                    model=self.config.model,
                    messages=messages,
                    temperature=self.config.temperature,
                    max_tokens=self.config.max_tokens,
                    timeout=30,
                )
                if tools:
                    kwargs["tools"] = tools

                response = self.client.chat.completions.create(**kwargs)
                return self._parse_response(response)

            except APITimeoutError as e:
                if attempt < 2:
                    wait = 2 ** attempt
                    logger.warning(f"API 超时，{wait}s 后重试 ({attempt+1}/2)")
                    time.sleep(wait)
                else:
                    logger.error("API 超时，已达重试上限")
                    raise
                last_error = e

            except RateLimitError as e:
                if attempt < 3:
                    wait = 2 ** attempt
                    logger.warning(f"速率限制，{wait}s 后重试 ({attempt+1}/3)")
                    time.sleep(wait)
                else:
                    logger.error("速率限制，已达重试上限")
                    raise
                last_error = e

            except APIStatusError as e:
                if e.status_code in (500, 502, 503, 504) and attempt < 2:
                    wait = 2 ** attempt
                    logger.warning(f"服务端错误 {e.status_code}，{wait}s 后重试")
                    time.sleep(wait)
                else:
                    logger.error(f"API 错误 {e.status_code}: {e.message}")
                    raise
                last_error = e

            except OpenAIError as e:
                logger.error(f"API 错误: {type(e).__name__}: {e}")
                raise

        raise last_error if last_error else Exception("API 调用失败")

    def _parse_response(self, response) -> LLMResponse:
        choice = response.choices[0]
        message = choice.message

        tool_calls = []
        if message.tool_calls:
            for tc in message.tool_calls:
                tool_calls.append(ToolCallInfo(
                    id=tc.id,
                    type=tc.type,
                    function_name=tc.function.name,
                    function_arguments=tc.function.arguments,
                ))

        usage = {}
        if response.usage:
            usage = {
                "prompt_tokens": response.usage.prompt_tokens,
                "completion_tokens": response.usage.completion_tokens,
                "total_tokens": response.usage.total_tokens,
            }

        return LLMResponse(
            content=message.content or "",
            tool_calls=tool_calls,
            usage=usage,
        )

# chyan-agent 设计方案

> 状态：最终版（自适应上下文窗口）
> 目标：让 Agent 按所用模型自动适配上下文窗口，在用量达到阈值时自动压缩，
> 构建一个最小可用、可运行、可演示的轻量 Agent。

---

## 1. 项目定位

chyan-agent 不是一个通用 Agent 框架，而是一个**轻量、可解释、可验证**的 Agent。它要回答的核心问题是：

> 如何按所用模型的上下文窗口自动适配预算，并在用量达到默认阈值时自动压缩历史，
> 通过分层记忆 + 上下文预算 + 工具调用，支撑上百轮连贯对话？

窗口大小分层解析：**配置覆盖 > 可选联网目录 > 内置模型表 > 兜底默认**。
因此，所有设计选择都服务于「可解释、可验证、可复现」这三个目标。

---

## 2. 核心设计原则

| 原则 | 说明 |
|------|------|
| 轻量 | 无 SQLite、无向量数据库、无重型依赖，JSON 文件持久化 |
| 透明 | 上下文占用、工具调用、记忆召回都可打印、可审计 |
| 真实 | 工具操作真实文件系统，不是 Mock |
| 可验证 | 提供 100 轮压力测试脚本，输出上下文曲线 |

---

## 3. 文件结构

```
~/Project/chyan-agent/
├── chyan_agent/
│   ├── __init__.py
│   ├── config.py          # 加载并校验 config.toml
│   ├── model_windows.py   # 模型上下文窗口分层解析（JSON 窗口表 + 可选联网目录）
│   ├── model_windows.json # 具体模型名 -> 窗口 tokens（可自定义）
│   ├── context.py         # 上下文预算管理：token 计数、阈值压缩、摘要
│   ├── memory.py          # JSON 持久化：原始历史 + 长期记忆
│   ├── tools.py           # 工具定义与执行
│   ├── llm.py             # 统一 LLM 调用封装
│   └── agent.py           # 主编排逻辑
├── config.toml            # 用户配置文件（需手动创建或复制模板）
├── config.example.toml    # 配置模板
├── main.py                # 命令行交互入口
├── scripts/
│   └── run_100_turns.py   # 100 轮自动对话测试
├── requirements.txt
├── README.md
└── docs/
    ├── requirements.md    # 需求说明（与实现保持一致）
    └── design.md          # 本文件
```

---

## 4. 配置系统（config.toml）

`chyan_agent/config.py` 负责读取 `config.toml`，并提供类型安全的访问接口。

### 4.1 配置项

完整配置参见 [`config.toml`](../config.toml)。

### 4.2 关键参数说明

**LLM 配置**：
- `base_url`：OpenAI 兼容 API 地址。
- `api_key`：API 密钥。
- `model`：模型名称（如 gpt-4o-mini、gpt-4o）。
- `temperature`：生成随机性（0-1，推荐 0.3）。
- `max_tokens`：单次回复最大 token 数。

**上下文配置**：
- `context_limit`：`"auto"` 表示按 `llm.model` 自动解析窗口；也可写整数（如 `10000`）显式覆盖。
- `default_limit`：auto 解析不到模型窗口时的兜底值（默认 131072）。
- `reserved_tokens`：给工具调用结果、模型回复预留的空间。该值应设置为**最坏情况**下的安全边界。
- `trigger_ratio`：压缩软阈值，占可用预算的比例（默认 0.7）。
- `trigger_bytes`：压缩的第二道保险，原始历史文本字节数（默认 204800，即 200KB）；与 `trigger_ratio` 任一满足即压缩。
- `compress_target_ratio`：压缩目标水位，压到「触发阈值 × 该比例」（默认 0.8），形成滞回，避免临界抖动导致每轮都压缩。
- `max_recent_turns`：压缩时保护的最近完整对话轮数（默认 4），**不再作为硬性裁剪上限**。
- `summary_ratio`：对历史消息做一级摘要时的目标压缩比例。
- `summary_max_tokens` / `summary_max_ratio`：`summary` 二级压缩上限；前者为 0 时按后者（占可用预算比例）自动计算。
- `window_table`：内置窗口表 JSON 路径；留空使用随包的 `chyan_agent/model_windows.json`。
- `online_catalog` / `catalog_url` / `catalog_proxy` / `catalog_cache` / `catalog_ttl_hours`：可选联网模型目录（models.dev），默认关闭以保证离线确定性。
- `usable_tokens = context_limit - count(system_prompt) - count(tool_definitions) - reserved_tokens`，这是 context.py 实际能用的历史消息**硬上限**；`soft_limit = usable_tokens * trigger_ratio` 是压缩触发线。

**窗口解析（model_windows.py）**：分层策略，先命中先用——配置显式值 > 可选联网目录（带缓存与 TTL，失败降级）> 内置模型表（按**具体模型名**精确映射，对日期/版本后缀做最长前缀容错）> 兜底默认。解析结果与来源在启动时打印。

**记忆配置**：
- `data_dir`：数据持久化目录。
- `recall_mode`：记忆召回算法，支持 `"bm25"`（推荐）或 `"grep"`（简单关键词匹配）。
- `recall_top_k`：召回记忆的默认数量。

**工具配置**：
- `read_max_lines`：单次 `read` 工具返回的最大行数，防止大文件占满上下文。
- `read_truncate_mode`：截断模式
  - `"auto"`：智能截断（前 N 行 + ... + 后 M 行，N+M = read_max_lines）
  - `"head"`：只保留前 N 行
  - `"tail"`：只保留后 N 行

**日志配置**：
- `level`：日志级别（DEBUG/INFO/WARNING/ERROR），推荐 INFO。
- `log_file`：日志文件路径，相对于工作目录。
- `max_bytes`：单个日志文件最大字节数（默认 10MB），超出后自动轮转。
- `backup_count`：保留的历史日志文件数量（默认 3 个）。

---

## 5. 上下文管理系统（context.py）

### 5.1 核心职责

1. 精确计算每条消息 token 数（tiktoken），包括工具定义的 tokens。
2. 维护三条消息队列：
   - `system`：系统 prompt
   - `summary`：压缩后的历史摘要
   - `recent`：最近 N 轮完整对话
3. 当历史用量达到阈值（`soft_limit` 或 `trigger_bytes`）时，按策略压缩。
4. 对外提供 `build_messages(user_input)` → OpenAI messages 列表。
5. 动态预留空间给预期的工具输出和模型回复，防止单次调用突破 `context_limit`。

### 5.2 消息队列结构

```python
class ContextManager:
    system: str
    summary: str            # 一段长文本，概括更早的对话
    recent: list[Message]   # 最近 N 轮完整对话
```

### 5.3 压缩策略

当 `history_tokens + 本轮 > soft_limit` **或** `history_bytes + 本轮 > trigger_bytes` 时：

1. **一级压缩（批量）**：从 `recent` 中批量弹出最老的若干轮（直到回到阈值内，或只剩保护轮数），
   **合并为一次 LLM 摘要**追加到 `summary`。批量而非逐轮，可显著降低 LLM 调用次数。
2. **保护最近轮**：始终保留最近 `max_recent_turns` 轮完整原文，不参与摘要。
   `add_turn` 不再硬性裁剪 `recent`，裁剪统一由压缩负责。
3. **二级压缩**：`summary` 超过上限时，先对 `summary` 自身再摘要一次（仅当严重超限，避免信息损失雪崩），
   仍超则按 FIFO 截断，保留尾部并前缀 `…`。
4. **底线保护**：仅当清空 `summary` 确实能回到阈值内时才清空，优先保住最近对话。

**设计原则**：一级压缩允许批量、二级压缩克制递归（只在严重超限时再摘要），
避免信息损失雪崩，同时保证大窗口下 `summary` 不会无界增长。

### 5.4 摘要提示词

```
请把以下对话内容压缩为关键事实列表。保留：实体、决策、数值、待办。删除：寒暄、重复、语气词。
输出用简洁中文 bullet points，不超过原文的 {ratio}% 长度。
```

### 5.5 Token 预算公式

```
fixed_cost   = tokens(system_prompt) + tokens(tool_definitions)
hard_limit   = context_limit - fixed_cost - reserved_tokens   # 历史硬上限
soft_limit   = hard_limit * trigger_ratio                     # 压缩触发线
触发条件     = history_tokens + 本轮 > soft_limit
              或 history_bytes + 本轮 > trigger_bytes         # 任一满足

total_history = tokens(summary) + tokens(recent) ≤ hard_limit
```

每次构建 messages 前重新计算，达到任一阈值则触发压缩。`reserved_tokens` 用于预留给工具输出和模型回复的动态空间。

---

## 6. 记忆系统（memory.py）

### 6.1 持久化文件

所有数据存到 `memory.data_dir` 目录：

```
data/
├── conversation.jsonl   # 原始完整对话历史，每行一条 JSON
├── memories.json        # 长期记忆列表
└── meta.json            # 当前 summary 与 recent 指针等运行时状态
```

### 6.2 数据模型

**conversation.jsonl**
```json
{"turn": 1, "role": "user", "content": "...", "timestamp": "..."}
{"turn": 1, "role": "assistant", "content": "...", "tool_calls": []}
```

**memories.json**
```json
[
  {"id": "m1", "content": "用户偏好 Python", "source_turn": 3, "created_at": "..."},
  {"id": "m2", "content": "项目使用 PostgreSQL", "source_turn": 7, "created_at": "..."}
]
```

**meta.json**（可选快照文件）
```json
{
  "summary": "",
  "recent": [],
  "total_turns": 0,
  "last_updated": "...",
  "version": 1
}
```

### 6.3 生命周期

- 启动时优先从 `meta.json` 恢复 `summary` 和 `recent`（快速启动）。
- 如果 `meta.json` 不存在或版本不匹配，从 `conversation.jsonl` 重建上下文。
- 每轮对话结束追加到 `conversation.jsonl`，更新 `memories.json` 和 `meta.json`。
- 提供 `reset()` 清空所有数据。

### 6.4 BM25 记忆索引 + jieba 中文分词

- 启动时从 `memories.json` 加载所有记忆，用 `rank-bm25` 建立索引。
- 中文分词使用 `jieba`，索引构建和查询时均按 jieba 分词结果处理；未安装 jieba 时降级为逐字拆分。
- 每次调用 `remember` 工具新增记忆时，重建索引（记忆数量通常较少，重建开销可接受）。
- `recall` 工具使用 BM25 算法对查询进行相关性排序，返回 top_k 条最匹配的记忆。

实现参见 [`chyan_agent/memory.py`](../chyan_agent/memory.py)。

---

## 7. 工具系统（tools.py）

### 7.1 工具列表

| 工具 | 功能 | 是否操作真实文件 | 特殊说明 |
|------|------|------------------|----------|
| `read` | 读取文件内容 | 是 | 支持 `offset/limit` 行范围；整文件智能截断 |
| `write` | 写入/追加内容到文件 | 是 | 禁止覆盖敏感文件 |
| `edit` | 精确字符串替换 | 是 | 唯一匹配，或 `replace_all` |
| `list` | 列目录 | 是 | 跳过隐藏文件/EXCLUDE_DIRS，可设 depth |
| `search` | 按文件名 glob 查找 | 是 | - |
| `grep` | 按正则搜索内容 | 是 | - |
| `delete` | 删除文件或空目录 | 是 | 禁止删除敏感文件 |
| `remember` | 把事实写入长期记忆 | 是（memories.json） | 自动重建 BM25 索引 |
| `recall` | 从长期记忆中召回 | 是（BM25） | 返回 top_k 条 |
| `todo_write` / `todo_read` | 会话内任务清单 | 否（内存） | 整表覆盖 |
| `bash` | 受限 shell 执行命令 | 是（子进程） | 需 `bash_enabled`；危险命令拦截、超时、输出上限、目录限制 |
| `web_fetch` / `web_search` | 抓网页 / 搜索 | 否（网络） | 需 `web_enabled`；可配 `web_proxy` |

**统一约束（见 7.5）**：工具结果受 `tool_output_max_bytes` 统一截断；文件与命令受 `work_dir` 目录限制；
未启用的 `bash`/`web` 不注册 schema，不计入 fixed tokens。

### 7.1.1 read 工具截断机制

为防止单个文件占满上下文预算，`read` 工具实现智能截断：

**配置参数**（来自 config.toml）：
- `read_max_lines`：最多返回行数（默认 500）
- `read_truncate_mode`：截断模式（默认 "auto"）

**截断策略**：
```python
def read_file(path: str, max_lines: int, mode: str) -> str:
    lines = open(path).readlines()
    
    if len(lines) <= max_lines:
        return ''.join(lines)
    
    if mode == "head":
        return ''.join(lines[:max_lines]) + f"\n\n[截断：剩余 {len(lines) - max_lines} 行]"
    
    elif mode == "tail":
        return f"[截断：省略前 {len(lines) - max_lines} 行]\n\n" + ''.join(lines[-max_lines:])
    
    elif mode == "auto":
        # 前 70% + 后 30%
        head_lines = int(max_lines * 0.7)
        tail_lines = max_lines - head_lines
        return (
            ''.join(lines[:head_lines]) +
            f"\n\n[截断：省略中间 {len(lines) - max_lines} 行]\n\n" +
            ''.join(lines[-tail_lines:])
        )
```

**工具返回示例**：
```json
{
  "success": true,
  "path": "src/main.py",
  "total_lines": 1200,
  "returned_lines": 500,
  "truncated": true,
  "content": "import os\n...\n[截断：省略中间 700 行]\n...\nif __name__ == '__main__':\n    main()"
}
```

### 7.2 工具定义（OpenAI Function Calling Schema）

每个工具都要提供：
- `name`
- `description`
- `parameters`（JSON Schema）
- `execute(**kwargs)` 函数

**recall 工具示例**：
```json
{
  "name": "recall",
  "description": "从长期记忆中召回相关内容。使用自然语言描述你想找的信息（如'数据库配置'、'用户偏好语言'、'上次讨论的端口号'），系统会自动用 BM25 算法匹配最相关的记忆。",
  "parameters": {
    "type": "object",
    "properties": {
      "query": {
        "type": "string",
        "description": "要查找的内容关键词或短语，支持多词组合"
      },
      "top_k": {
        "type": "integer",
        "default": 5,
        "description": "返回最相关的 N 条记忆"
      }
    },
    "required": ["query"]
  }
}
```

### 7.3 工具执行流程

1. LLM 决定调用工具时，返回 `tool_calls`。
2. `agent.py` 按顺序执行每个工具，收集结果。
3. 把工具结果作为 `tool` 消息重新喂给 LLM。
4. LLM 基于结果生成最终回复。

### 7.4 安全边界

- `write`/`edit`/`delete` 默认禁止操作 `config.toml`、`config.example.toml`、`.env` 等敏感文件（黑名单机制）。
- 文件操作与 `bash` 受 `work_dir` 限制：路径/工作目录超出该根目录即拒绝（`work_dir` 为空则不限制）。
- `bash` 默认关闭；启用后拦截危险命令模式（`rm -rf /`、`mkfs`、`dd of=/dev/...`、`sudo`、`curl|sh` 等），
  并有超时与输出字节上限。
- 工具执行失败时返回结构化错误信息，包含建议的修复步骤，不抛异常中断对话。

**错误信息示例**：
```json
{
  "success": false,
  "error": "文件不存在: src/main.py",
  "suggestion": "使用 search 工具查找文件路径，或用 write 工具创建新文件"
}
```

### 7.5 输出预算与工具门控

- **统一输出预算**：`execute()` 返回前对结果 JSON 做 `tool_output_max_bytes` 检查，超出则截断为
  `{"truncated_output": true, "note": ..., "preview": ...}`，避免单个工具结果挤爆上下文。
- **工具门控**：`bash_enabled` / `web_enabled` 为假时，对应 schema 不注册，节省 fixed tokens，
  且在未启用时调用返回明确错误而非静默失败。

---

## 8. LLM 调用层（llm.py）

### 8.1 职责

- 统一封装 OpenAI 兼容接口。
- 支持 `chat.completions.create`。
- 支持 tool calling。
- 处理超时、重试、错误降级。

### 8.2 接口

```python
class LLMClient:
    def __init__(self, config: LLMConfig): ...
    def chat(self, messages: list, tools: list | None = None) -> LLMResponse: ...
```

### 8.3 错误处理

**超时与重试策略**：

| 错误类型 | HTTP 状态码 | 重试策略 | 重试间隔 |
|---------|-------------|---------|---------|
| 请求超时 | Timeout (>30s) | 重试 2 次 | 1s, 2s |
| 速率限制 | 429 | 重试 3 次 | 指数退避：1s, 2s, 4s |
| 服务端错误 | 500, 502, 503, 504 | 重试 2 次 | 1s, 2s |
| 客户端错误 | 400, 401, 403, 404 | 不重试 | - |
| 无可用 key | 401 | 不重试，提示配置 | - |
| 返回格式异常 | JSON 解析失败 | 不重试，记录日志 | - |

实现参见 [`chyan_agent/llm.py`](../chyan_agent/llm.py)。

### 8.4 日志系统

使用 Python `logging` 模块，配合 `RotatingFileHandler` 实现日志轮转。

**日志级别与内容**：

| 级别 | 记录内容 | 示例 |
|------|---------|------|
| DEBUG | token 计数、压缩触发、BM25 打分详情 | `[DEBUG] 压缩触发: 批量弹出 3 轮, summary 12345 -> 8200 tokens` |
| INFO | 每轮对话摘要、工具调用 | `[INFO] Turn 5: user=读文件, tools=[read], tokens=4810/124000` |
| WARNING | 接近预算、召回结果为空、文件不存在 | `[WARNING] 上下文仍超出窗口: 130200 > 131072` |
| ERROR | API 失败、文件操作异常、配置错误 | `[ERROR] API 调用失败: 429 Rate Limit` |

初始化代码参见 [`main.py`](../main.py)。

**日志输出示例**：
```
[2026-07-03 10:23:45] [INFO] Turn 1: user=帮我读 config.json, tokens=390/124000
[2026-07-03 10:23:46] [INFO] 工具调用: read(path=config.json)
[2026-07-03 10:23:47] [INFO] Turn 1 完成, assistant tokens=156
[2026-07-03 10:24:12] [DEBUG] 压缩触发: 批量弹出 3 轮, summary 12345 -> 8200 tokens
[2026-07-03 10:25:30] [ERROR] API 超时，2s 后重试 (1/2)
[2026-07-03 10:25:32] [INFO] API 重试成功
```

---

## 9. Agent 编排（agent.py）

### 9.1 主循环

实现参见 [`chyan_agent/agent.py`](../chyan_agent/agent.py)。

### 9.2 与上下文管理的协作

- 调用 `build_messages` 时，context.py 自动完成摘要和裁剪。
- `memory.save_turn` 保存完整原始内容到 `conversation.jsonl`，不受窗口限制影响。

---

## 10. 命令行入口（main.py）

### 10.1 交互模式

```bash
python main.py --config config.toml
```

交互界面：
```
chyan-agent > 帮我看看 src/main.py 的内容
[read] src/main.py
[回复内容]
上下文：system=234 + summary=1456 + recent=3120 = 4810 / 124000 hard
```

### 10.2 支持命令

| 命令 | 说明 | 输出示例 |
|------|------|---------|
| `/reset` | 清空当前对话上下文与记忆 | `已清空 45 轮对话和 12 条记忆` |
| `/status` | 打印当前上下文占用情况 | `window=131072, soft=87000, hard=124000`<br>`system=234, summary=1456(cap=18000), recent=3120, total=4810/124000 (4%), bytes=18234/204800` |
| `/memories` | 列出所有长期记忆 | `[m1] 用户偏好 Python (Turn 3)`<br>`[m2] 项目使用 PostgreSQL (Turn 7)` |
| `/export` | 导出对话历史为 JSON | `已导出到 data/export_20260703_102345.json` |
| `/exit` | 退出 | - |

### 10.3 故障恢复机制

**自动恢复**：
- 每轮对话结束后，`conversation.jsonl` 和 `meta.json` 立即落盘（无缓存）。
- 崩溃后重启，自动从 `meta.json` 恢复 `summary` 和 `recent` 状态。
- 如果 `meta.json` 损坏或 `version` 不匹配，自动从 `conversation.jsonl` 重建（耗时但保证数据完整性）。

**手动恢复**：
```bash
# 从指定轮次继续（跳过前 N 轮）
python main.py --config config.toml --resume-from-turn 50

# 强制重建 meta.json（忽略现有快照）
python main.py --config config.toml --rebuild-context
```

**恢复日志示例**：
```
[2026-07-03 10:30:15] [INFO] 检测到 meta.json (version=1, turns=47)
[2026-07-03 10:30:15] [INFO] 恢复上下文: summary=2340 tokens, recent=4 轮
[2026-07-03 10:30:15] [INFO] 从 Turn 48 继续
```

---

## 11. 100 轮压力测试（scripts/run_100_turns.py）

### 11.1 目标

验证在自适应窗口（默认按模型解析）下，Agent 能否稳定完成 100 轮对话，并观察：
- 总 token 占用是否始终低于硬上限，且压缩后回落到软阈值内
- summary 长度随轮次的变化，是否受二级压缩上限约束
- 记忆召回是否有效

### 11.2 测试方式

设计三种测试场景，覆盖不同的上下文压力模式：

#### **场景 1：线性任务**（30 轮）
**目标**：测试基本上下文管理和摘要压缩。

**特点**：每轮独立，无依赖关系，模拟知识问答场景。

**示例 prompts**：
```
1. 介绍一下 Python 语言的特点
2. 什么是 Docker 容器技术
3. 解释 REST API 的设计原则
4. 说说微服务架构的优缺点
5. 什么是 CI/CD
...（共 30 条）
```

**预期结果**：
- 前 10 轮：`recent` 持续增长，`summary` 为空
- 第 11-30 轮：触发压缩，`summary` 逐渐增长，`recent` 保持在 `max_recent_turns` 以内
- 总 token 占用稳定在 40%-60% 之间

---

#### **场景 2：依赖任务**（40 轮）
**目标**：测试 `recent` 保留效果和跨轮依赖理解。

**特点**：每条输入依赖前几轮结果（如"刚才的文件"、"上一步的结果"）。

**示例 prompts**：
```
1. 创建一个名为 test.txt 的文件
2. 往 test.txt 里写入 'Hello World'
3. 读取 test.txt 的内容
4. 在 test.txt 末尾追加一行 'Goodbye'
5. 再读一次 test.txt，确认内容
6. 创建 data.json 文件
7. 往 data.json 写入 {"name": "test"}
8. 读取 data.json
9. 修改 data.json，增加字段 "version": 1
10. 确认 data.json 的最终内容
...（共 40 条，形成 4-5 条操作链）
```

**预期结果**：
- `recent` 必须保留足够轮数（至少 4 轮），否则"刚才的文件"等指代会失效
- 第 20 轮后开始频繁触发压缩
- 总 token 占用在 50%-75% 之间波动

---

#### **场景 3：记忆召回压力**（30 轮）
**目标**：测试 `remember`/`recall` 工具和 BM25 索引效果。

**特点**：前期密集存储事实，中期无关对话，后期批量召回。

**示例 prompts**：
```
1. 记住：我最喜欢的编程语言是 Python
2. 记住：项目数据库使用 PostgreSQL 15
3. 记住：API 服务运行在 8080 端口
4. 记住：日志存储在 /var/log/app
5. 记住：Redis 缓存过期时间是 3600 秒
6-20. （15 轮无关对话，如"介绍 Rust"、"什么是 GraphQL"）
21. 我最喜欢的编程语言是什么？
22. 项目用的什么数据库？
23. API 服务的端口号是多少？
24. 日志文件在哪里？
25. Redis 缓存多久过期？
26-30. （5 轮混合查询，如"数据库和 Redis 的配置信息"）
```

**预期结果**：
- 前 5 轮：每次调用 `remember`，`memories.json` 增加 1 条
- 第 21-25 轮：每次调用 `recall`，BM25 精确召回对应记忆（Precision@5 > 90%）
- 第 26-30 轮：多关键词查询，返回多条相关记忆并按相关性排序
- `summary` 应包含"用户存储了 5 条记忆"等元信息

---

每轮记录 `system / summary / recent / total` token 数，生成 JSONL 日志和 matplotlib 曲线图。

### 11.3 输出

```
output/
├── turns_log.jsonl
├── context_usage.png
└── test_scenarios.json  # 测试用例定义
```

测试场景定义参见 [`scripts/run_100_turns.py`](../scripts/run_100_turns.py)。

**输出 JSONL 格式**（每行一个 JSON 对象）：
```jsonl
{"turn": 1, "scenario": "linear", "system_tokens": 234, "summary_tokens": 0, "recent_tokens": 156, "total_tokens": 390, "usable_limit": 13500, "compression_triggered": false, "tool_calls": [], "timestamp": "2026-07-03T10:23:45"}
{"turn": 2, "scenario": "linear", "system_tokens": 234, "summary_tokens": 0, "recent_tokens": 312, "total_tokens": 546, "usable_limit": 13500, "compression_triggered": false, "tool_calls": ["read"], "timestamp": "2026-07-03T10:23:48"}
{"turn": 5, "scenario": "dependency", "system_tokens": 234, "summary_tokens": 450, "recent_tokens": 1200, "total_tokens": 1884, "usable_limit": 13500, "compression_triggered": true, "tool_calls": ["write", "recall"], "timestamp": "2026-07-03T10:24:01"}
```

**JSONL 格式优势**：
- 流式追加：每轮测试结束立即写入一行，无需缓存全部数据
- 结构化：支持复杂类型（数组、对象），可记录每轮的工具调用详情
- 与项目一致：与 `conversation.jsonl` 格式统一
- 易于分析：pandas 可直接读取 `pd.read_json("turns_log.jsonl", lines=True)`

### 11.4 可视化输出

**context_usage.png 图表内容**：

使用 matplotlib 生成双子图，展示上下文占用趋势，代码详见 [`scripts/run_100_turns.py`](../scripts/run_100_turns.py)。

**预期图表**：
- 图1 显示：
  - 蓝色区域（System）保持恒定
  - 橙色区域（Summary）阶梯式增长（每次压缩增加一块）
  - 绿色区域（Recent）锯齿状波动（增长后突然下降）
  - 总占用始终低于红色虚线（usable_limit）
  
- 图2 显示：
  - 红点标记每次压缩触发的轮次
  - 线性任务：稀疏分布（每 5-8 轮一次）
  - 依赖任务：密集分布（每 3-4 轮一次）
  - 记忆召回：前期密集，后期稀疏

---

## 12. 与博文的结合点

| 博文章节 | 对应代码/演示 |
|----------|---------------|
| 为什么固定窗口不够 | `model_windows.py` 窗口解析 + `context.py` 预算计算 |
| 分层记忆设计 | `summary` + `recent` + `memories.json` |
| 阈值压缩策略 | `context.py` 的 `_compress_to_fit` / `_shrink_summary` |
| 工具调用与记忆协同 | `tools.py` 的 `remember/recall` |
| 100 轮验证 | `scripts/run_100_turns.py` |

---

## 13. 依赖清单

```txt
# requirements.txt
openai>=1.0.0,<2.0.0
tiktoken>=0.7.0,<1.0.0
matplotlib>=3.8.0,<4.0.0
rank-bm25>=0.2.2,<1.0.0
jieba>=0.42.1,<1.0.0
pandas>=2.0.0,<3.0.0
```

**依赖说明**：
- `openai`：LLM API 调用，兼容 OpenAI 和其他 API。
- `tiktoken`：精确 token 计数，支持 GPT-4/GPT-3.5 tokenizer。
- `matplotlib`：压力测试可视化，生成上下文占用曲线图。
- `rank-bm25`：长期记忆 BM25 相关性检索，配合 `jieba` 做中文分词。
- `jieba`：中文分词，提升 BM25 在中文记忆召回中的精度。
- `pandas`：JSONL 日志读取与数据分析。

**版本约束原则**：
- 使用 `>=X.Y.Z,<(X+1).0.0` 格式，允许小版本更新，禁止大版本跃迁。
- 生产环境建议用 `pip freeze > requirements.lock` 锁定精确版本。

**安装**：
```bash
pip install -r requirements.txt
```

---

## 14. 关键风险与验证建议

### 14.1 最高风险点

| 风险 | 影响 | 验证方式 | **预期指标** |
|------|------|---------|-------------|
| 动态 token 预算不准 | 实际 token 突破窗口限制 | 对比 tiktoken 与 API 返回的 `usage.total_tokens` | **误差 < 5%，100 轮无越界** |
| 摘要压缩信息丢失 | 早期重要信息在 50+ 轮后无法召回 | 人工检查 summary 内容 + 场景 2 依赖任务准确率 | **依赖任务成功率 > 95%** |
| BM25 召回精度不足 | `recall` 返回无关记忆 | 准备 10 组查询-记忆对，计算 Precision@5 | **Precision@5 > 90%** |
| 工具输出过大 | 单个 `read` 返回 5K tokens 文件 | 测试读取 2000 行文件，验证截断机制 | **单次工具输出 < 2000 tokens** |
| 工具调用死循环 | Agent 反复调用同一工具 | 场景 2 依赖任务中监控工具调用次数 | **单轮工具调用 ≤ 5 次** |

**性能基线**：
- 单轮平均响应时间：< 3 秒（不含 LLM API 延迟）
- 100 轮总耗时：< 10 分钟（gpt-4o-mini）
- 内存占用：< 100MB（不含模型加载）
- 日志文件大小：< 5MB（100 轮）

### 14.2 分阶段验证

**阶段 1：核心模块验证**
1. 实现 `context.py`，用 20 条固定消息测试压缩逻辑。
2. 实现 `memory.py`，测试 BM25 召回 10 条记忆的准确率。
3. 验证 token 计数准确性（与 OpenAI API 对比误差 < 5%）。

**阶段 2：小规模集成**
1. 实现 `agent.py` + `tools.py`，跑通 20 轮完整对话。
2. 观察上下文增长曲线，调整 `reserved_tokens` 和 `max_recent_turns`。
3. 测试工具调用链路（read → grep → write 连续操作）。

**阶段 3：100 轮压力测试**
1. 实现 `scripts/run_100_turns.py`，执行三种场景。
2. 生成上下文占用曲线图，确认始终低于窗口硬上限。
3. 人工抽查 10 轮对话，验证回复质量和记忆召回准确性。

---

## 15. 下一步工作

1. 创建 `chyan_agent/` 包结构与各模块代码。
2. 创建 `config.example.toml`。
3. 实现 `main.py` CLI。
4. 实现 `scripts/run_100_turns.py`。
5. 按 14.2 节分阶段验证。
6. 把运行结果写入博文 source.md。

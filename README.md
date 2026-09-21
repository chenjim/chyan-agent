# chyan-agent

> 自适应上下文窗口的轻量 Agent — 分层记忆 + 上下文预算 + 工具调用，支撑上百轮连贯对话。

## 项目定位

chyan-agent 是一个**轻量、可解释、可验证**的 Agent。它要回答的核心问题是：

> 如何让 Agent 按所用模型的上下文窗口自动适配预算，并在用量达到阈值时自动压缩历史，
> 用分层记忆支撑上百轮连贯对话？

窗口大小按 `llm.model` 分层解析（配置覆盖 > 可选联网目录 > 内置模型表 > 兜底默认），
不再写死固定窗口。所有设计选择都服务于**可解释、可验证、可复现**这三个目标。

## 核心设计原则

| 原则      | 说明                                                 |
| --------- | ---------------------------------------------------- |
| 🪶 轻量   | 无 SQLite、无向量数据库、无重型依赖，JSON 文件持久化 |
| 🔍 透明   | 上下文占用、工具调用、记忆召回都可打印、可审计       |
| 🛠️ 真实 | 工具操作真实文件系统，不是 Mock                      |
| 📊 可验证 | 提供 100 轮压力测试脚本，输出上下文曲线              |

## 架构概览

```
chyan-agent/
├── chyan_agent/
│   ├── __init__.py
│   ├── config.py         # 加载并校验 config.toml
│   ├── model_windows.py  # 模型上下文窗口分层解析（JSON 窗口表 + 可选联网目录）
│   ├── model_windows.json # 具体模型名 -> 窗口 tokens（可自定义）
│   ├── context.py        # 上下文预算管理：token 计数、阈值压缩、摘要
│   ├── memory.py         # JSON 持久化：原始历史 + 长期记忆
│   ├── tools.py          # 工具定义与执行
│   ├── llm.py            # 统一 LLM 调用封装
│   └── agent.py          # 主编排逻辑
├── config.toml           # 用户配置文件
├── main.py               # 命令行交互入口
├── scripts/
│   └── run_100_turns.py  # 100 轮自动对话测试
├── requirements.txt
├── README.md
└── docs/
    ├── requirements.md   # 需求说明（与实现保持一致）
    └── design.md         # 完整设计方案
```

## 快速开始

### 1. 安装依赖

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
```

### 2. 配置

编辑 `config.toml`，填入你的 LLM API 信息：

```toml
[llm]
base_url = "https://api.openai.com/v1"
api_key = "sk-xxx"
model = "gpt-4o-mini"
temperature = 0.1
max_tokens = 4096

[context]
context_limit = "auto"   # 按模型自动解析窗口；也可写整数覆盖
trigger_ratio = 0.7      # 可用预算达 70% 触发压缩
trigger_bytes = 204800   # 或原始历史达 200KB 触发压缩
```

详见 `docs/design.md` 完整配置说明。

### 3. 运行

```bash
python main.py
```

CLI 交互模式，支持以下命令：

| 命令          | 功能                     |
| ------------- | ------------------------ |
| `/reset`    | 清空当前对话上下文与记忆 |
| `/status`   | 打印当前上下文占用情况   |
| `/memories` | 列出所有长期记忆         |
| `/export`   | 导出对话历史为 JSON      |
| `/help`     | 显示命令列表             |
| `/exit`     | 退出                     |

## 核心机制

### 上下文预算管理

窗口按 `llm.model` 自动解析（配置覆盖 > 可选联网目录 > 内置模型表 > 兜底默认）：

```python
fixed_cost = tokens(system_prompt) + tokens(tool_definitions)
hard_limit = context_limit - fixed_cost - reserved_tokens     # 历史硬上限
soft_limit = hard_limit * trigger_ratio                       # 压缩触发线
触发条件   = history_tokens + 本轮 > soft_limit
            或 history_bytes + 本轮 > trigger_bytes           # 任一满足
```

达到阈值即自动压缩：

1. **一级压缩**：批量弹出最老的若干轮，合并为一次摘要写入 `summary`，保护最近 `max_recent_turns` 轮
2. **二级压缩**：`summary` 超过上限时先再摘要，仍超则 FIFO 截断保留尾部
3. **底线保护**：仅当清空摘要确实能回到阈值内时才清空，避免无谓信息损失

### 记忆系统

- **短期记忆**：保留最近 N 轮完整对话（`recent`）
- **中长期摘要**：压缩后的对话概要（`summary`）
- **长期记忆**：用户告知的关键事实，使用 BM25 相关性召回

### 工具列表

| 工具         | 功能                         |
| ------------ | ---------------------------- |
| `read`     | 读取文件内容（`offset/limit` 行范围 + 智能截断） |
| `write`    | 写入/追加内容到文件          |
| `edit`     | 精确字符串替换（改文件首选） |
| `list`     | 列出目录内容                 |
| `search`   | 按文件名 glob 查找           |
| `grep`     | 按正则搜索文件内容           |
| `delete`   | 删除文件或空目录             |
| `remember` | 把事实写入长期记忆           |
| `recall`   | 从长期记忆中召回归档信息     |
| `todo_write` / `todo_read` | 会话内任务清单 |
| `bash` | 执行命令（需 `bash_enabled`；危险命令拦截/超时/输出上限） |
| `web_fetch` / `web_search` | 抓网页 / 联网搜索（需 `web_enabled`） |

所有工具结果受 `tool_output_max_bytes` 统一截断；文件与命令受 `work_dir` 目录限制。

## 测试

运行单元测试：

```bash
.venv/bin/python -m pytest tests/ -v --tb=short
```

运行 100 轮压力测试：

```bash
python scripts/run_100_turns.py
```

脚本会生成上下文占用趋势图 `output/context_usage.png` 和详细日志 `output/turns_log.jsonl`。

## 技术栈

- Python 3.10+
- [OpenAI Python SDK](https://github.com/openai/openai-python) — LLM API 调用
- [tiktoken](https://github.com/openai/tiktoken) — 精确 token 计数
- [rank-bm25](https://github.com/dorianbrown/rank_bm25) — 长期记忆相关性检索
- [prompt_toolkit](https://github.com/prompt-toolkit/python-prompt-toolkit) — 交互式 CLI

## 许可

MIT © [chyan.tech](https://chyan.tech)

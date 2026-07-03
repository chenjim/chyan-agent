# chyan-agent

> 10K 上下文窗口下的轻量 Agent — 分层记忆 + 上下文预算 + 工具调用，支撑上百轮连贯对话。

## 项目定位

chyan-agent **不是**一个通用 Agent 框架，而是一个**工程演示项目**。它要回答的核心问题是：

> 当 LLM 上下文窗口只有 10K tokens 时，如何通过分层记忆 + 上下文预算 + 工具调用，支撑上百轮连贯对话？

所有设计选择都服务于**可解释、可验证、可复现**这三个目标。

## 核心设计原则

| 原则 | 说明 |
|------|------|
| 🪶 轻量 | 无 SQLite、无向量数据库、无重型依赖，JSON 文件持久化 |
| 🔍 透明 | 上下文占用、工具调用、记忆召回都可打印、可审计 |
| 🛠️ 真实 | 工具操作真实文件系统，不是 Mock |
| 📊 可验证 | 提供 100 轮压力测试脚本，输出上下文曲线 |

## 架构概览

```
chyan-agent/
├── chyan_agent/
│   ├── __init__.py
│   ├── config.py         # 加载并校验 config.toml
│   ├── context.py        # 上下文预算管理：token 计数、摘要、裁剪
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
temperature = 0.3
max_tokens = 1024
```

详见 `docs/design.md` 完整配置说明。

### 3. 运行

```bash
python main.py
```

CLI 交互模式，支持以下命令：

| 命令 | 功能 |
|------|------|
| `/reset` | 清空当前对话上下文与记忆 |
| `/status` | 打印当前上下文占用情况 |
| `/memories` | 列出所有长期记忆 |
| `/export` | 导出对话历史为 JSON |
| `/help` | 显示命令列表 |
| `/exit` | 退出 |

## 核心机制

### 上下文预算管理

```python
fixed_cost = tokens(system_prompt) + tokens(tool_definitions)
usable_for_history = context_limit - fixed_cost - reserved_tokens
```

当总 token 超过预算时自动触发压缩：
1. **初级压缩**：把最老的一轮对话交给 LLM 摘要，纳入 `summary`
2. **次级压缩**：`summary` 超过上限时 FIFO 丢弃早期内容
3. **底线保护**：确保最近 1 轮对话始终完整保留

### 记忆系统

- **短期记忆**：保留最近 N 轮完整对话（`recent`）
- **中长期摘要**：压缩后的对话概要（`summary`）
- **长期记忆**：用户告知的关键事实，使用 BM25 相关性召回

### 工具列表

| 工具 | 功能 |
|------|------|
| `read` | 读取文件内容（支持智能截断） |
| `write` | 写入/追加内容到文件 |
| `search` | 按文件名 glob 查找 |
| `grep` | 按正则搜索文件内容 |
| `remember` | 把事实写入长期记忆 |
| `recall` | 从长期记忆中召回归档信息 |

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

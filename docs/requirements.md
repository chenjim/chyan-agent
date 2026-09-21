# chyan-agent 需求说明

> 状态：与当前实现保持一致（自适应上下文窗口版）
> 定位：可解释、可验证、可复现的轻量 Agent。上下文窗口按所用模型自动适配，
> 历史用量达到阈值时自动压缩，用分层记忆支撑上百轮连贯对话。

## 1. 目标

- 不再绑定固定 10K 窗口；窗口大小按 `llm.model` 自动解析，也允许配置显式覆盖。
- 历史用量达到默认阈值（**可用预算的 70%** 或 **原始历史文本 200KB**，任一满足）即自动压缩，
  而不是等撞到硬顶才压，为工具结果与模型回复留出安全余量。
- 保留分层记忆：短期（`recent`）/ 中长期（`summary`）/ 长期（`memories`），
  支撑上百轮对话仍能召回早期信息。

## 2. 功能需求

### FR-1 窗口解析（分层，先命中先用）
1. 配置显式指定：`context.context_limit` 为整数或数字字符串。
2. 可选联网目录：`models.dev/api.json`，带本地缓存与 TTL，失败时降级到缓存/内置表。
3. 内置模型窗口表：JSON 文件 `chyan_agent/model_windows.json`（可用 `context.window_table` 指向自定义文件），
   按**具体模型名**精确映射（如 `deepseek-v4.1-flash`、`kimi-k2.7-code`），对日期/版本后缀做最长前缀容错。
4. 兜底默认：`context.default_limit`。

解析结果与来源（config/online/builtin/fallback）在启动日志与 CLI 头部打印。

### FR-2 上下文预算
```
fixed_cost = tokens(system_prompt) + tokens(tool_definitions)
hard_limit = context_limit - fixed_cost - reserved_tokens     # 历史硬上限
soft_limit = hard_limit * trigger_ratio                       # 压缩触发线
触发条件   = history_tokens + 本轮 > soft_limit
            或 history_bytes + 本轮 > trigger_bytes           # 任一满足
```
- `history_bytes` 为 `summary` 与 `recent` 文本的 UTF-8 字节数之和。
- token 计数使用 tiktoken（默认 `o200k_base`），作为跨模型近似。

### FR-3 压缩（一级）
- 触发后，批量弹出最老的若干轮（`recent` 中超出保护轮数的部分），
  **合并为一次摘要**追加到 `summary`，降低 LLM 调用次数。
- 保护最近 `max_recent_turns` 轮完整原文，不参与摘要。
- `add_turn` 不再硬性裁剪 `recent`，裁剪统一由压缩负责。

### FR-4 二级压缩（summary 上限）
- `summary` 超过上限（`summary_max_tokens`，或按 `summary_max_ratio` 自动）时：
  - 严重超限（> 2×上限）且有 LLM：对 `summary` 自身再摘要一次；
  - 仍超限：FIFO 截断，保留尾部，前缀 `…`。
- 底线保护：仅当清空 `summary` 确实能回到阈值内时才清空，避免无谓信息损失。

### FR-5 记忆系统
- 短期：`recent` 最近若干轮完整对话。
- 中长期：`summary` 压缩摘要。
- 长期：`memories.json`，`remember`/`recall` 工具，BM25 + jieba 中文分词召回。
- 持久化：`conversation.jsonl`（原始全量）、`memories.json`、`meta.json`（运行时快照）。

### FR-6 工具
操作真实文件系统，敏感文件（`config.toml`、`.env` 等）写入/编辑/删除受保护。

| 工具 | 说明 | 默认 |
|------|------|------|
| `read` | 读文件；支持 `offset/limit` 读指定行范围与整文件智能截断 | 开 |
| `write` | 整文件写入/追加 | 开 |
| `edit` | 精确字符串替换（唯一匹配，或 `replace_all`） | 开 |
| `list` | 列目录（名称/类型/大小，可设 depth） | 开 |
| `search` | 按文件名 glob 查找 | 开 |
| `grep` | 按正则搜索内容 | 开 |
| `delete` | 删除文件或空目录 | 开 |
| `remember` / `recall` | 长期记忆写入 / BM25 召回 | 开 |
| `todo_write` / `todo_read` | 会话内任务清单 | 开 |
| `bash` | 受限 shell 执行命令（危险命令拦截、超时、输出上限、目录限制） | 关（需 `bash_enabled`） |
| `web_fetch` / `web_search` | 抓取网页 / 联网搜索 | 关（需 `web_enabled`） |

`web_search` 支持 SearXNG/通用 JSON 接口（`web_search_url`，`{query}` 占位）；默认 DuckDuckGo HTML 常被反爬拦截，被拦截时返回明确错误与配置建议。

**统一约束**：
- `tool_output_max_bytes`：所有工具结果统一字节上限，超出截断为 `preview`，保护上下文。
- `work_dir`：文件操作与命令的工作目录根，超出即拒绝；留空不限制。
- 未启用的 `bash`/`web` 不注册 schema，不占用 fixed tokens。

### FR-7 CLI
`/reset`、`/status`（含窗口、软/硬阈值、summary 上限、字节占用）、`/memories`、`/export`、`/help`、`/exit`。

## 3. 非功能需求

- 轻量：无 SQLite / 向量库，JSON 持久化；新增依赖为零（联网目录用标准库 urllib）。
- 透明：窗口来源、预算、压缩触发均可打印、可审计。
- 可复现：联网目录默认关闭，保证离线确定性。
- 安全：不泄露密钥；敏感文件受保护；联网目录支持代理配置。

## 4. 关键配置项（`[context]`）

| 配置 | 默认 | 说明 |
|------|------|------|
| `context_limit` | `"auto"` | `auto` 或显式整数 |
| `default_limit` | 131072 | 兜底窗口 |
| `reserved_tokens` | 4096 | 预留给工具结果与回复 |
| `trigger_ratio` | 0.7 | 软阈值比例 |
| `trigger_bytes` | 204800 | 字节触发阈值（200KB） |
| `compress_target_ratio` | 0.8 | 压缩目标水位（滞回，避免临界抖动） |
| `max_recent_turns` | 4 | 压缩时保护的最近轮数 |
| `summary_ratio` | 0.3 | 一级摘要目标比例 |
| `summary_max_tokens` | 0 | 二级压缩上限，0=自动 |
| `summary_max_ratio` | 0.15 | 自动上限占可用预算比例 |
| `online_catalog` | false | 是否启用联网模型目录 |
| `window_table` | `""` | 内置窗口表 JSON 路径，空=随包 `chyan_agent/model_windows.json` |

## 4.1 工具配置（`[tools]`）

| 配置 | 默认 | 说明 |
|------|------|------|
| `read_max_lines` / `read_truncate_mode` | 500 / `auto` | 整文件读取的行数与截断模式 |
| `max_tool_iterations` | 10 | 单轮工具调用上限 |
| `tool_output_max_bytes` | 20000 | 单个工具结果字节上限 |
| `work_dir` | `""` | 工作目录根，空=不限制 |
| `bash_enabled` / `bash_timeout` / `bash_workdir` / `bash_max_output_bytes` | false / 30 / `""` / 20000 | bash 工具 |
| `web_enabled` / `web_proxy` / `web_max_bytes` / `web_search_url` | false / `""` / 20000 / DDG | 联网工具 |
| `catalog_url` / `catalog_proxy` / `catalog_cache` / `catalog_ttl_hours` | — | 联网目录参数 |

## 5. 验收标准

- `pytest tests/ -v` 全绿。
- 启动时打印窗口解析结果与来源。
- 达到软阈值或字节阈值时触发压缩，压缩后历史占用回落到阈值内（受保护轮除外）。
- `summary` 不超过其上限；超限时可再摘要或 FIFO 截断。
- 100 轮压力测试脚本可运行并输出上下文占用曲线。

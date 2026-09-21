# CLAUDE.md

轻量 Agent：按模型自动适配上下文窗口，用量达阈值（可用预算 70% 或原始历史 200KB，任一满足）自动压缩，用分层记忆 + 工具调用支撑上百轮连贯对话。目标可解释、可验证、可复现。

## 常用命令
```bash
.venv/bin/python -m pytest tests/ -v --tb=short   # 单元测试
.venv/bin/python scripts/run_100_turns.py         # 100 轮压力测试
.venv/bin/python main.py                          # CLI 交互入口
```

## 架构
```
chyan_agent/{config,model_windows,context,memory,tools,llm,agent}.py
chyan_agent/model_windows.json  # 具体模型名->窗口表（可自定义路径 context.window_table）
main.py        # CLI 交互入口
config.toml    # 用户配置（已 gitignore）
config.example.toml  # 配置模板（含参数注释）
scripts/run_100_turns.py   docs/{requirements,design}.md
```

## 核心机制
- 窗口解析：分层（配置覆盖 > 可选联网目录 > 内置模型表 > 兜底默认），`context_limit="auto"`
- 上下文预算：`hard = context_limit - fixed_cost - reserved_tokens`；`soft = hard * trigger_ratio`；达 soft 或 `trigger_bytes` 自动压缩（批量摘要/保护最近N轮/summary 二级压缩）
- 记忆：短期(最近N轮)→中长期摘要→长期事实(BM25召回)
- 工具：read/write/search/grep/remember/recall/delete

## 约定
- 轻量：无 SQLite/向量库，JSON 持久化；可打印、可审计；操作真实文件系统
- 配置参考 config.example.toml，勿提交 config.toml（含真实密钥）

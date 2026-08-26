# CLAUDE.md

工程演示项目：10K 上下文窗口下，用分层记忆 + 上下文预算 + 工具调用支撑上百轮连贯对话。目标可解释、可验证、可复现。

## 常用命令
```bash
.venv/bin/python -m pytest tests/ -v --tb=short   # 单元测试
.venv/bin/python scripts/run_100_turns.py         # 100 轮压力测试
.venv/bin/python main.py                          # CLI 交互入口
```

## 架构
```
chyan_agent/{config,context,memory,tools,llm,agent,cli}.py
main.py        # 入口，委托给 chyan_agent.cli.main
config.toml    # 用户配置（已 gitignore）
config.example.toml  # 配置模板（含参数注释）
scripts/run_100_turns.py   docs/design.md
```

## 核心机制
- 上下文预算：`usable = context_limit - fixed_cost - reserved_tokens`，超限自动压缩（摘要/FIFO/保护最近1轮）
- 记忆：短期(最近N轮)→中长期摘要→长期事实(BM25召回)
- 工具：read/write/search/grep/remember/recall

## 约定
- 轻量：无 SQLite/向量库，JSON 持久化；可打印、可审计；操作真实文件系统
- 配置参考 config.example.toml，勿提交 config.toml（含真实密钥）

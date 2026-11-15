# chyan-agent

呈言科技出品的开源、本地优先「个人 Agent 工作台」（Go 实现），以 Web/PWA 为主交互入口；设计参考 opencode 与 Hermes 的实践。

## 文档

- `docs/requirements.md` — 产品需求（分期 FR 与验收标准）
- `docs/research.md` — P0 调研与决策记录（ADR）
- `docs/architecture.md` — P1 架构设计（分层、目录、里程碑）
- `docs/development-plan.md` — 开发计划（P2-P5 任务分解与质量门）

## 约定

- 实现语言 Go（`chyan/` 目录，module `chyan.tech/chyan`），PWA 为 TS 构建后 go:embed 嵌入。
- **代码注释用英文，docs/ 文档用中文**。
- **测试覆盖全面**：变更逻辑须有自动化测试，覆盖率目标 ≥ 80%；边界/错误路径必测。
- **技术不清晰先调研**：方案拿不准时先 search 探索业界最优实践再决策，不臆测。
- 技能兼容 `~/.agents/skills/<name>/SKILL.md`（frontmatter 仅 name+description）。
- 需求变更同步 `docs/requirements.md`；架构决策记入 `docs/research.md` ADR 表。
- 旧 Python 实现已移除；迁移背景与算法来源见 `docs/research.md`。

# 子 Agent 执行约定（P2 并行实现，已归档）

> 历史协作记录：P2 并行实现期间供子 Agent 读取的约定。当前开发以 `AGENTS.md` 为准。

## 共同约定（所有子 Agent）

1. 只改自己被分配的目录/文件；不动 `internal/core/core.go`、`internal/store/store.go` 契约
   （如契约确有缺陷，写进自己模块的实现文档并在总结中上报，勿自行改契约）。
2. 代码注释**英文**、简洁；文件头一行包注释；复杂逻辑才加行内注释。
3. 测试覆盖全面：边界/错误路径必测；`go test ./...` 全绿、目标包覆盖率 ≥ 80%；
   `gofmt`/`go vet` 零告警。
4. 可读性优先，不滥用抽象；不写超出任务范围的功能（YAGNI）。
5. 测试内**不得依赖网络**；需要外部服务一律 mock。
6. 参考设计：`docs/architecture.md`（4.x 节）、`docs/requirements.md`（FR-x）、
   `docs/development-plan.md`（任务号）；默认参数见 architecture.md 附录 A。
7. 完成标准 = 实现 + 测试全绿 + 该包 gofmt 干净；总结里报：新增文件、关键决策、
   覆盖率数字、与契约/文档的偏差（如有）。

## 模块契约速查

- 消息/工具/上下文/权限类型：`internal/core/core.go`
- 持久化接口：`internal/store/store.go`（SQLite 落 `~/.chyan/chyan.db`）
- 目录约定：私有数据 `~/.chyan/`；跨工具知识 `.agents/`
- 旧算法参考：早期 Python 实现的 `context.py`（迁移背景见 `docs/research.md`）

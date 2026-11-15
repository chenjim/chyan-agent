# chyan 开发计划

> 状态：v4（2026-11-01，P1–P5 已交付，P6 需求补口进行中）
> 依据：[requirements.md](requirements.md)、[architecture.md](architecture.md)、[research.md](research.md)。
> 约定：接口先行；每任务带测试（覆盖率 ≥ 80%，边界/错误必测）；代码注释英文；每模块独立提交。

## 0. 总览

```
P2 内核 MVP ──► P3 多端与安全 ──► P4 助理能力 ──► P5 打磨发布
（CLI 可用）    （手机可派发）    （hermes 可替换）  （产品可发布）
```

每期出口条件见 [architecture.md §6](architecture.md)。当前进度：P1–P5 交付完成
（发布准入项 T1 许可证已关闭；真实 LLM 配置 T9 已解决），P6 需求补口见 §5。实时台账见 [TODO.md](TODO.md)。

## 1. P2 内核 MVP（已完成）

三条实现线可并行（契约已冻结于 `chyan/internal/core/core.go` 与 `internal/store/store.go`）：

### 线 A：core + llm + 上下文预算
| # | 任务 | 产出 | 测试 |
|---|------|------|------|
| A1 | llm 包：OpenAI 兼容 client + **provider-model adapter（能力协商：流式优先 `Streamer`、不支持回退 `Chat`）** + 多 provider 路由 + fallback 链 | `internal/llm/` | mock server 单测（超时/429/格式异常/流式回退） |
| A2 | token 计数 + 模型窗口表（移植 model_windows.json 分层解析；联网目录更新走内部 HTTP client，非 web 工具） | `internal/llm/windows.go` | 分层解析用例（config>online>builtin>fallback） |
| A3 | ContextManager：窗口感知阈值（软/硬、token/bytes 双触发）、turn 边界压缩约束、handoff 交接（含 `/compact`） | `internal/core/context.go` | 阈值计算用例由早期 Python 版本的 tests/test_context.py 移植适配；handoff/turn 边界用例新增 |
| A4 | agent loop：消息组装、工具循环、max_tool_iterations、输出截断 | `internal/core/agent.go` | 假 LLM 驱动的循环用例 |
| A5 | 子 Agent：匿名/命名派发、嵌套 ≤ 2、并发限额、结果摘要回传、Interrupt | `internal/core/subagent.go` | 嵌套越界/权限收敛/中断用例 |
| A6 | 内置工具 read_session / remember / recall（/compact 走 ContextManager.Compact） | `internal/core/tools_builtin.go` | 摘要回传/记忆读写用例（todo 不做，见 D16） |

### 线 B：store SQLite
| # | 任务 | 产出 | 测试 |
|---|------|------|------|
| B1 | SQLite 实现（session/message/task/memory/audit/kv + 迁移框架） | `internal/store/sqlite.go` | 全接口 CRUD 单测（内存实现同用例复跑） |
| B2 | cost/tokens 汇总（子→父）；session JSON 导出/导入后移 P4-6 | `internal/store/` | 汇总用例 |
| B3 | memory md 真相源最小实现：文件命名 `<id>-<slug>.md`（base36 时间有序 id）、frontmatter 解析（含 triggers）、**全局+项目两域读写**（handoff 双保险依赖项目域，Memory.ProjectPath 标归属）、关键词检索（`SearchMemories(query, kind, projectPath, topK)`，projectPath 非空时项目域+全局、项目优先；空时全局）。P2 助理链路载体；FTS5/梦境/完整分域/hindsight 迁移留 P4-3 | `internal/memory/store.go` | frontmatter 边界/坏文件跳过/两域读写/命名规则/项目归属检索用例 |

> FTS5 索引（应用层分词 + BM25 + 域合并索引）整体移 P4-3（方案见 architecture.md 4.3）；
> P2 用关键词检索实现 `SearchMemories`，P4-3 仅换实现不动签名。

### 线 C：tools + skills + CLI
| # | 任务 | 产出 | 测试 |
|---|------|------|------|
| C1 | 文件工具集（read/write/edit/list/search/grep/delete）+ work_dir 限制 + 敏感文件黑名单 + edit 写前 mtime/hash 冲突校验 | `internal/tools/fs.go` | 越界拒绝/截断/黑名单/冲突校验用例 |
| C2 | bash 工具（危险命令拦截/超时/输出上限） | `internal/tools/bash.go` | 拦截模式表驱动测试 |
| C4 | skills 加载器（~/.agents/skills SKILL.md frontmatter） | `internal/skills/` | 123 个真实技能抽样解析测试 |
| C5 | CLI：`chyan run "任务"`（--model/--agent/--session/--continue/--fork/--format/--work-dir）；`/status`、`/compact` 以独立子命令触发（`chyan status`、`chyan compact --session`），供非交互态查询/压缩 | `cmd/chyan/` | 集成测试（假 LLM，含 --fork 分叉用例、work_dir 默认 cwd 用例） |
| C6 | MCP client（官方 go-sdk）：local command + remote url 双形态、headers 引用 secret 凭据（不明文）、工具注册 adapter（`<server>_<tool>` 防冲突）、输出统一截断、过三态审批（P3 起）。**web 工具后移 P4，P2 外部能力（搜索/抓取）经 MCP server 补位** | `internal/mcp/` | 双形态连接/凭据引用/工具名冲突/超时用例（mock MCP server） |
| C7 | command 加载器（`.agents/commands/*.md`，斜杠命令 prompt 模板，对齐 opencode 格式 description/template/$ARGUMENTS） | `internal/commands/` | frontmatter 解析/占位符渲染用例 |
| C8 | `~/.chyan/` 目录初始化（含旧 UserConfigDir/UserCacheDir 首启迁移）+ config.toml 加载（分段见 architecture 4.9）+ secret/ 凭据存储（明文 + 0600，D20；配置引用 `secret:<name>` 解析）。A1 路由配置与 C6 凭据引用的硬前置，**排 C1 之前** | `internal/secret/` + `internal/config/` | 目录初始化/旧路径迁移/配置解析/缺省合并/凭据引用解析/权限位/坏文件用例 |

> C3（三态权限 + Intersect）整体移 P3（与审批流同期交付，见 P3-2）；P2 工具安全 =
> work_dir 限制 + 黑名单 + 危险命令拦截（硬编码，无权限规则引擎）。

**P2 出口**：编码任务（改文件走工具链）+ 助理任务（记忆读写：B3 md 真相源 + remember/recall）
各 1 条真实链路跑通（人工 dogfood + 关键路径 e2e 假 LLM 自动化）；
`go test ./...` 全绿，覆盖率 ≥ 80%（`go test -coverprofile`，`internal/` 各包合并统计，入口壳层不计入）；压缩/handoff 按新策略验证
（窗口感知触发、turn 边界约束、数据层不压缩）。

## 2. P3 多端与安全

| # | 任务 | 产出 |
|---|------|------|
| P3-1 | daemon：HTTP REST + SSE + 双监听面（本地 unix socket / 远程 TCP `127.0.0.1:31415` + token 鉴权） | `internal/daemon/` |
| P3-2 | 三态权限规则引擎 + Intersect（自 P2 线 C 迁入，含通配符匹配/收紧矩阵用例）+ 审批流：ask → SSE/IM 推送 → 批准/拒绝 → audit；审批单附改动 diff 预览；`--auto` 模式 | `internal/core/perm.go` 等 |
| P3-3 | PWA：任务列表/发起任务/审批面板（diff 预览）/只读文件树/父子任务树（Vite+TS，go:embed） | `chyan/web/` |
| P3-4 | TUI（bubbletea）：会话列表、上下文占用、审批面板（自 P2 迁入，依赖 daemon/审批流） | `internal/tui/` |
| P3-5 | 远程接入：Tailscale 首选 + 反代+token 可选（远程面端口见 P3-1；通道说明见 architecture 4.5） | 部署文档 |

**出口**：手机发起任务全链路走通；危险操作被审批拦截；audit 可查。

## 3. P4 助理能力（hermes 替换关键期）

| # | 任务 | 产出 |
|---|------|------|
| P4-1 | cron 调度（JSON 任务定义，借鉴 hermes jobs.json） | `internal/cron/` |
| P4-2 | channels：飞书（官方 oapi-sdk-go）+ Telegram adapter | `internal/channels/` |
| P4-3 | memory 长期记忆完整版：FTS5 索引缓存（自 P2 线 B 迁入：应用层中文分词 CJK bigram、BM25×新近度×pinned×域优先级、三域合并索引；modernc FTS5 支持开工首日验证，不可用降级纯 Go 倒排）+ 夜间梦境任务（抽取/查重/supersede 链/梦境报告/项目归属判定）+ 完整分域（项目/本机/全局三级 + workspace trust）+ hindsight 迁移脚本（md 读写最小版已在 P2-B3） | `internal/memory/` |
| P4-4 | 浏览器自动化（playwright 子进程桥先行） | `internal/tools/browser.go` |
| P4-5 | web_fetch / web_search 工具（代理支持） | |
| P4-6 | session JSON 导出/导入（自 B2 后移；兼容 opencode session export 格式的导入器，可选） | `internal/store/export.go` |

**出口**：hermes 常用 cron 任务与 IM 通知迁移可用；记忆无损迁移验证。

## 4. P5 打磨发布

| # | 任务 | 产出 |
|---|------|------|
| P5-1 | TTS/语音转写（对接本地 kokoro 等） | |
| P5-2 | 性能：daemon 常驻 < 80MB、CLI 冷启动 < 300ms 压测达标 | |
| P5-3 | 三平台交叉编译打包（linux/win/darwin × amd64/arm64）+ 自动更新检查 | CI |
| P5-4 | 用户文档 + 诊断包 + 数据备份恢复 | |
| P5-5 | dogfood 两周 → 退役 opencode/hermes → 对外发布 | |

**出口**：三平台安装包；dogfood 无阻断问题。

## 5. P6 需求补口（2026-10-29，进行中）

需求 v2 补齐项（[requirements.md](requirements.md)）对应任务，按依赖排序：

| # | 任务 | 覆盖需求 | 测试/验收 |
|---|------|----------|-----------|
| R1 ✅ | 取消运行全链路：core `Interrupt` 覆盖工具/子 Agent → daemon `POST /api/v1/tasks/{id}/cancel` → CLI Ctrl+C（REPL 只中断 turn）→ TUI/PWA 取消按钮 → session `interrupted` 落库 | FR-1/4/6、D29 | 三阶段中断用例 + daemon e2e（发起→取消→状态） |
| R2 ✅ | 图像输入：llm `image_url`（base64）与视觉能力协商 → `read` 读图 → CLI `--image`/REPL 粘贴 → PWA 上传 → IM 入站图片 → browser 截图回灌 | FR-10、D31 | 假 LLM 断言 image part + 非视觉模型报错 + 超限拒绝 |
| R3 ✅ | 指令源加载：全局 `~/.agents/AGENTS.md` + 项目 `AGENTS.md`/`CLAUDE.md`/`.cursor/rules` 注入 directive 段，`[context] load_project_instructions` 可关 | FR-7、D32 | 加载优先级/缺失文件/开关用例 |
| R4 ✅ | IM 入站触发（文本 + 飞书/Telegram 图片）：白名单、**thread 粒度**会话映射（`channel:chat[:t:thread]`，`/new` 清空）、防回环（忽略 bot 自身、同 thread 串行、限速）、图片附件 | FR-7、D35 | channels/daemon e2e（假飞书 server） |
| R5 ✅ | `chyan update` 命令：检查 + 版本比较 + 平台产物下载地址与 SHA256 输出（不提供安装动作）；网络失败给离线路径；release 元数据补 SHA256SUMS | FR-8、D33 | 假 release 端点的新旧版本/无资产/网络失败用例 |
| R6 ✅ | 运维缺口：`chyan prune`（session/audit/artifacts，dry-run 默认）+ 日志落盘按天轮转（90 天保留、3 天后压缩为 zip、脱敏）+ `status` 输出 db/artifacts/cache 体积与运行任务数 + 迁移前自动备份 + `restore --force`（覆盖前自动备份）+ artifact 写入/回收用例 | FR-11、D34 | prune 往返/dry-run、日志轮转、迁移失败拒绝启动、restore 往返 |
| R7 ✅ | daemon 稳定性：并发任务上限（默认 4，排队）、单任务超时（30min 可配）、优雅停机（有界排空）、重启后 `running` 任务标 `interrupted`、SSE 断线自动重连/轮询恢复、TUI 平台感知默认地址（Windows 走 TCP）、**watchdog 自愈扫描（卡死任务/拒绝激增/陈旧会话）与 `/healthz` 探活** | FR-6 | 并发/超时/停机/重启/断线重连/watchdog 用例 |
| R8 ✅ | 文档口径同步：Windows 本地面非目标、`chyan` 默认 REPL、成本/自更新非目标、架构附录 B 黑名单清单 | §3/§5 | 文档 diff 核对 |
| R9 ✅ | 体验与安全加固（2026-11-01）：IM **全双工队列**（每 thread FIFO 上限 8、忙时回执、turn 边界合并、共享 task slot）+ **回复裁剪 800 字 + PWA 链接** + **意图过滤**（默认关）；**订阅直投**（`im:sub`/`im:owner`）；**Inline & Spill / `expand`** 链式续读（≤20KB 窗口、越界拒绝）；**触发式记忆注入**（确定性 triggers 匹配 top 3/400 字，走 **user 通道**，不进 system prompt）；**常驻 Router**（`CHYAN_ROUTER=1`，默认关）；**调度指标**（`im.turn`/`task.turn` SSE 事件） | FR-1/2/6/7、D37–D45 | channels/daemon/core/app 测试 + 全量回归 |

**出口**：R1–R9 全部有测试证据；`requirements.md` §4 验收表 A1–A12 逐行可点开证据；
`go test ./... -race` 全绿、覆盖率维持 ≥80%。

## 6. 质量门（每期通用）

1. 任务完成 = 实现 + 测试 + 文档同步 + 独立提交。
2. `go test ./...` 全绿，覆盖率 ≥ 80%（`go test -coverprofile`，`internal/` 各包合并统计，入口壳层不计入）。
3. `gofmt`/`go vet` 零告警。
4. 需求变更同步 `requirements.md`；架构决策追加 `research.md` ADR 表。
5. 技术不清晰先调研（search/对标），不臆测。

## 7. 待决事项（不阻塞 P6）

| 事项 | 影响 | 计划 |
|------|------|------|
| 开源许可证选型（D8 盈利未定） | P5 发布前定 | 预研 MIT/Apache-2.0/BSL |
| 远程接入默认通道（Tailscale vs 反代） | P3-5 | 按 §2 架构先做双监听面抽象，通道可后定 |
| 飞书之外的 IM 渠道优先级 | P4-2 后 | dogfood 反馈定 |
| TUI 深度（对标 opencode TUI 程度） | P3-4 | 先命令面板+审批，够用即止 |

## 8. 里程碑时间参照

不设绝对日期，以出口条件驱动；单人开发按「线并行、任务串行」推进，
P2 → P3 → P4 → P5 → P6 顺序交付，期间本人全程 dogfood。

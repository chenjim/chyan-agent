# chyan 架构设计

> 状态：P1 定稿（2026-10-15；2026-11-02 同步产品定位）
> 产品：chyan（呈言智）—— 开源、本地优先的个人 Agent 工作台（呈言科技产品），以 Web/PWA 为主交互入口。
> 调研与决策见 [research.md](research.md)，需求见 [requirements.md](requirements.md)。

## 1. 定位

一个常驻后台的个人 Agent 工作台：统一承接编码、文件处理、浏览器操作与定时自动化；
Web/PWA 是主交互入口，CLI/TUI 服务高级用户与脚本，IM 服务移动触发、通知与审批；
技能与 MCP 生态直接兼容现有资产；设计参考 opencode 与 Hermes 的实践。

## 2. 分层架构

```
┌─────────────────── 接入层 ───────────────────┐
│ chyan CLI │ chyan TUI │ Web 工作台/PWA │ IM bot │
└────────────────────┬────────────────────────┘
                chyan daemon（Go 单二进制，常驻）
┌────────────────────┴────────────────────────┐
│ API 层：HTTP REST + SSE 事件流 + 本地鉴权     │
│ ┌─────────────── 内核 core ────────────────┐ │
│ │ agent loop │ 模型路由 │ 上下文预算+handoff │ │
│ │ 工具循环   │ 子 Agent │ 权限三态审批      │ │
│ ├────────────── 能力层 capability ─────────┤ │
│ │ memory(md真相源+梦境) │ skills 兼容层    │ │
│ │ cron 调度 │ channels(IM) │ tools 工具箱  │ │
│ ├────────────── 基础层 platform ───────────┤ │
│ │ SQLite │ MCP client │ 凭据存储 │ 日志    │ │
│ └─────────────────────────────────────────┘ │
└─────────────────────────────────────────────┘
```

单二进制交付：CLI/TUI/daemon 是同一构建产物的不同入口（`chyan`、`chyan serve`），
PWA 构建产物 `go:embed` 嵌入二进制。零外部运行时依赖。

## 3. 目录结构（Go workspace）

```
chyan-agent/
├── chyan/                  # Go 工程（module）
│   ├── cmd/                # CLI 入口（cobra）：chyan / chyan serve / chyan run
│   ├── internal/
│   │   ├── daemon/         # HTTP server + SSE + 本地鉴权
│   │   ├── core/           # agent loop、上下文预算/handoff、工具循环、子 agent、审批
│   │   ├── llm/            # 多 provider 路由（OpenAI 兼容 + anthropic 等）
│   │   ├── memory/         # 长期记忆（md 真相源 + FTS5 索引 + 梦境任务）+ hindsight 迁移
│   │   ├── skills/         # ~/.agents/skills 兼容加载器
│   │   ├── commands/       # .agents/commands 斜杠命令加载器
│   │   ├── mcp/            # MCP client（官方 go-sdk，只做 client 不做 server）
│   │   ├── tools/          # 文件/bash/web/浏览器/TTS（审批门控 + 输出截断）
│   │   ├── cron/           # 定时任务调度
│   │   ├── channels/       # im: feishu / telegram / ...（adapter 接口）
│   │   ├── tui/            # bubbletea TUI
│   │   ├── store/          # SQLite（session/task/audit/trace_span），JSONL 导出
│   │   ├── trace/          # 本地 span 模型、错误分类与耗时/成本归因汇总（FR-13/D47）
│   │   └── secret/         # 凭据独立存储（明文+0600，不加密，D20；替换明文 config.toml）
│   ├── web/                # PWA 源码（TS），构建产物 embed 进二进制
│   ├── configs/            # 配置模板（TOML）
│   └── docs/               # Go 侧设计补充
├── docs/                   # 本目录：requirements / research / architecture
└── scripts/                # 压测等脚本
```

## 4. 核心模块设计

### 4.1 Agent 内核（core）

移植 chyan-agent 已验证的算法，Go 重写：

- **上下文预算与压缩**：窗口分层解析（配置 > 联网目录 > 内置表 > 兜底）沿用旧版
  （沿用早期 Python 实现的 `context.py` 算法）；联网目录更新走内部 HTTP client（模型窗口表
  是 llm 包内部数据，非 web 工具，不受 web 工具 P4-5 后移影响），不可达时降级内置表。
  **压缩策略修订**——现代模型 1M 窗口 + prompt
  cache 常态下压缩不是好方案，压缩降级为**小窗口（如本地 200K 模型）的兜底机制**：
  - **数据层不被压缩改写**：压缩/handoff 只影响发给模型的窗口，永不改写落库数据。
    **落库口径**：工具输出按 `tool_output_max_bytes` 截断后写入 message 表（truncated
    标记）；超限时先把**未截断原文**落 `~/.chyan/artifacts/<session>/<tool>/<ts>-<sha12>.txt`
    （0600，不进 cache TTL 清理），message 内保留 `[artifact: 路径]` + `[sha256: …]` 指针
    ——真相源 = 完整 artifact + 哈希，模型可见部分是截断视图；
    工具截断（防膨胀）与上下文压缩（管窗口）是两回事，`read_session` 回查同口径；
    **链式续读**：内置 `expand` 工具按 `artifact:<path>` / `handoff:<session>` 引用按窗口
    （默认 ≤20KB）取回完整证据，带 `offset` 续读提示与嵌套引用（≤5 条），不必重跑工具；
    路径必须落在 artifacts 根内（越界拒绝），`expand` 属只读工具，仍受 deny 规则约束；
  - **窗口感知触发**：阈值绑定模型窗口表（4.7），软/硬阈值按窗口动态计算——1M 模型可全程零压缩；
  - **时机约束**：只在 turn 边界或工具循环迭代间隙触发，且要求 tool-call/result 成对完整；
    不打断工具执行、不打断 research 中途（压缩前先把进行中计划/已得结论固化进 handoff）；
  - **缓存友好**：上下文只做尾部追加不动前缀；压缩击穿 prompt cache，能不压就不压。
- **handoff 交接**（压缩产物，合并旧二级 summary 机制）：接近硬阈值或用户 `/compact` 手动触发时，
  生成结构化 handoff note——Intent & Corrections / Decisions / Done / Active / Next / References
  （+ Summary 段），清空 recent 只带 note 继续；note 存 session 附属（SQLite KV）随会话走，
  Decisions/Corrections 同步固化到项目 memory（md 真相源）双保险。工作现场（任务清单）在
  note 的 Active/Next 段维护，不做独立 TodoList UI。
- **agent loop**：借鉴 Hermes turn loop（run_agent.py），消息类型与 tool 调用格式对齐
  opencode 的 session 消息 schema（reasoning / tool / text part），保证导出互操作。
- **子 Agent**：详见 4.1.1。
- **工具循环**：`max_tool_iterations` 限制、工具输出统一字节截断（`tool_output_max_bytes`），
  防 session 数据膨胀（实际使用中 opencode 本地库可达 GB 级的教训）；edit 写前冲突校验（基于上次 read 的
  mtime/hash，被外部编辑器改动则拒绝写入并重读提示，防人工/agent 互相覆盖）。
- **运行取消**（D29）：`core.Agent.Interrupt` 协作式中断 LLM 请求/工具执行/子 Agent；
  daemon 暴露 `POST /api/v1/tasks/{id}/cancel`，CLI Ctrl+C（`run` 退出码 130）、TUI `ctrl+x`、PWA 取消按钮
  走同一语义。取消后已完成消息与工具结果照常落库、task 与 session 双写 `interrupted`
  （`session.status` 列，下一次成功 turn 复位 `active`）；重复取消幂等；取消不触发 fallback 重试。
  实现要点：daemon 用 `Agent.Interrupt` 而非取消 run ctx，避免撞掉取消前的落库写。
- **已证伪硬约束**：单轮内同一 `tool+参数` 签名失败达 `max_repeated_tool_failures`（默认 3）
  后，代码层直接拦截（执行前返回 synthetic error），不再交给模型自行判断是否重试；
  成功一次即清零。约束计数只在 turn 内有效，跨 turn 环境可能已变化，不盲目封禁。
- **证据锚点**：handoff note 新增 Evidence 段，由代码从折叠窗口抽取 artifact 指针与
  `[tool error]` 记录（不经 LLM），保证结论可回溯原件。

#### 4.1.1 子 Agent

主 Agent 派发子 Agent 执行独立子任务（调研、并行实现、只读探索），对齐 opencode `agents/*.md`。

**定义**：`~/.agents/agents/<name>.md`（全局）+ 项目 `.agents/agents/`（可覆盖），frontmatter 含
`description / model / permission`（permission 为三态规则子集），正文为系统提示。

**派发与生命周期**：
- 主 Agent 通过内置 `task` 工具派发：`task(prompt)` 或 `task(agent, prompt)` → 子 Agent 创建独立 session
  （`parent_id` 指向父 session）、独立上下文预算与工具循环。
- **匿名派发**：`agent` 可不传——使用默认模型、继承父权限（不放大）、通用系统提示，
  无需预定义 agents/*.md；命名 agent 只是预置了角色卡（系统提示/权限/模型）的可复用形态。
- **上下文隔离**：子 Agent 中间过程不回灌父上下文，仅返回最终结果摘要（含产出物路径）；
  完整过程落 session 表，父 Agent 可用 `read_session` 回查细节。防上下文爆炸。
- 多子 Agent 可并行（goroutine），结果按完成顺序聚合。

**权限收敛**：子 Agent 有效权限 = 父权限 ∩ agent 声明权限，只收紧不放大
（如 explorer 声明 `edit: deny, bash: deny` 即强制只读）。子 Agent 触发 ask 时走同一审批流（4.2），
audit 记录父子链路。

**资源限额**：嵌套深度 ≤ 2 层（子 Agent 不得再派子 Agent）、并发子 Agent 上限、
单子 Agent 工具迭代上限与超时；子 Agent cost/tokens 汇总进父 session 统计。

**可观测**：TUI/PWA 展示父子任务树（进行中/完成/失败），支持中断子 Agent。

### 4.2 权限审批（三态规则 + 三种审核模式）

- 规则：`allow / ask / deny` × action（**工具名**或 `*` 通配）× 资源通配符。
  优先级：**字面量模式优先于通配符**，同级命中 deny > ask > allow；无命中时 read=allow、其余=ask。
  action 即工具名（`read/write/edit/list/search/grep/delete/bash/browser/web_fetch/web_search/
  tts/transcribe/task/remember/recall/read_session/expand`，MCP 为 `<server>_<tool>`）；
  资源匹配见 requirements FR-5（文件类=工作区相对路径、bash=命令行，其余取入参或回落工具名；
  暂无 web/mcp 类级规则）。
- **审核模式**（决定 `ask` 由谁裁决，CLI flag / 配置 / TUI 可切换）：
  | 模式 | ask 处理 | 场景 |
  |------|----------|------|
  | `ai`（默认） | AI 审核员判风险：low 放行 / high 拒绝 / uncertain 升级人工 | 日常（无人值守友好） |
  | `human` | SSE/IM 推送人工批准 | 需要全程人工把关时 |
  | `full` | 自动放行（等价 opencode `--auto`） | 信任环境 |
- **硬底线**：显式 `deny` 在任何模式下都硬拦，`full` 也不例外。
- **AI 审核偏保守**：拿不准一律升级人工，不静默放行；判定与理由落 audit。
  **实现策略（D23）**：规则启发式先行 + LLM 兜底——命令/路径特征表直接判 low/high，
  规则拿不准才调 LLM 审核员（`review.model` 指定廉价模型，默认规则-only），LLM 仍
  uncertain 则升级人工。每 ask 最多一次额外 LLM 调用，成本可控。
- 审批流：ask 触发 → 按模式裁决 →（人工时）SSE 推送 PWA/IM → 批准/拒绝 → 落 audit 表。
- **审批超时**：human 模式下 ask **1 分钟无人批准/拒绝 → 降级 ai 裁决**（`review.escalate_timeout`
  可配）；ai 裁决若仍 uncertain → **按拒绝处理**，不回环升级人工（防"升级→超时→再升级"死循环）。
  超时降级与最终判定均落 audit。
- **审批预览**：审批单附改动 diff/文件片段（只读），TUI/PWA 审批面板同享——手机上无桌面
  编辑器也能明智审批；diff 由 tools 层按「上次 read 内容 vs 本次写入内容」生成，不依赖 git。
- 独立 audit 表（谁/何时/什么操作/批准人/审核模式/判定理由），产品级可追溯。

### 4.3 记忆（memory）

设计原则（调研支撑见 research.md §7）：**md 文件为真相源 + FTS5 索引缓存 + 夜间梦境维护**，
对齐 Letta MemFS（目录即索引）/ Anthropic memory tool（just-in-time 加载）/ Letta dreaming
（后台固化）三模式。**注入通道精确口径**：事实类记忆（fact/kb/model 及一切检索结果）
不进 system prompt（防投毒提权），只走 tool/user 通道；directive 类（AGENTS.md/CLAUDE.md 等
指令源）本是指令，**经用户信任确认后作为 system prompt 的指令段注入**，外部仓库默认不信任。

**分层结构**：
| 层 | 内容 | 载体 |
|----|------|------|
| 工作记忆 | recent 近期原文 | 会话上下文 |
| 交接记忆 | handoff 结构化交接（Intent/Decisions/Active/Next…） | 压缩交接（见 4.1），存 session 附属 |
| 长期记忆 | facts / kb / models / directives 四类（踩坑教训走 fact + procedural） | `~/.chyan/memory/<kind>/*.md`（全局）+ `<repo>/.agents/memory/`（项目）+ FTS5 索引 |

**长期记忆文件格式**（frontmatter 对齐 `~/.agents/skills` 的 SKILL.md 规则）：

```markdown
---
name: 用户偏好Python
keywords: [python, 技术栈, 偏好]      # 文件头关键词 = 检索入口
triggers: [工具输出落库, 会话大字段持久化]  # 踩坑记忆用：触发场景 = 干这事前会被想起来
type: semantic                        # semantic | episodic | procedural（facts 用）
status: active                        # active | superseded（替代双时间戳）
supersedes: m12                       # 使哪条旧记忆失效
source: dream | explicit | migrated:hindsight
pinned: false
---
正文：事实内容（Markdown）
```

- **真相源 = 文件**：人可读可整理、git 可版本化（时间线可回溯）、与技能生态格式统一；
  过时事实标 `status: superseded + supersedes` 链，**不删除**。
- **文件命名**：`<id>-<slug>.md`。id 为创建时生成的时间有序短 ID（base36，如 `m1a2b3`，
  域内唯一），frontmatter 显式 `id:`（示例中 `m12` 即此 id），`supersedes` 引用同域 id
  （supersede 链限同域，跨域不引用）；slug 取 name 的可读化缩写（kebab-case）。
- **索引层 = SQLite FTS5**（keywords+正文，BM25+中文分词）：派生缓存，可随时删除重建，
  排序 = BM25 × 新近度 × pinned × 域优先级。向量检索（sqlite-vec）为可选插件不作硬依赖。
  **中文分词方案**：应用层分词后空格拼接写入（unicode61 切词），索引侧与查询侧用同一
  分词器——CJK 二元切分（bigram）+ 英文按词的极简规则（零词典依赖），词典分词库
  （纯 Go 的 gse）作为 P4-3 实测后的可选增强；不用 trigram（中文双字词无法命中）、
  不用 cgo 分词（破坏交叉编译）。**域合并索引**：全局/项目/项目本机三域统一进一张
  FTS5 虚表（每行带 domain 列），跨域检索免 UNION、按域过滤/加权在查询内完成；
  modernc.org/sqlite 的 FTS5 构建支持在 P4-3 开工首日验证，不可用则降级纯 Go 倒排。
  **分期**：P2-B3 用关键词检索（frontmatter keywords + name，项目>全局排序），
  P4-3 上 FTS5 后 `SearchMemories` 契约不变、仅换实现。
- **just-in-time 加载**：recall → 索引命中（路径+摘要）→ 必要时才读全文，不预载上下文。

**踩坑记忆（lesson，存/召回）**：踩坑 = 「触发条件 → 错误做法 → 正确做法」，比普通事实多一层
**触发条件**——关键不在存，而在干这件事之前能被想起来（事后召回无价值）：
- **格式**：kind 仍走 fact，`type: procedural`，frontmatter 加 `triggers` 字段（见上，区别于泛化
  keywords），正文写清症状/错误做法/正确做法。
- **存**（三通道）：① 显式 `remember` 工具（"记住这个坑"）；② 会话内踩坑信号
  （"踩坑/原来不行/浪费了 X 小时"）→ 提示用户确认落盘，不静默写；③ 夜间梦境抽取
  （识别"试了 A 失败、改 B 成功"模式）。
- **召回**（混合）：每轮输入先做**确定性触发器匹配**（`triggers` 子串命中，大小写不敏感，
  不依赖 FTS5 排序），命中最多 3 条、每条正文截 400 字，**作为本轮 user 消息的前置段注入**
  （`[关联经验 / lessons]`，本 turn 一次性、不写回 system prompt，保持"记忆是证据不是指令"
  的防投毒原则）；其余靠 `recall` 工具显式查；敏感操作前查相关 lesson/directive
  （如落库大字段前查"截断"）。未命中则本轮输入原样不改。

**梦境维护（dream）**：daemon 夜间定时任务（P4-1 cron 承载）：
1. 读当日会话 → LLM 抽取少量原子事实（ADD-only，累积不做激进合并）；
2. 查重合并：重复标 superseded 链而非删除 → 写入 md 文件（单写者串行化，防并发写冲突）；
3. 重建 FTS5 索引 → 生成梦境报告推送 PWA/IM（新增/失效/合并清单，git 可回滚）；
4. 可选「审阅后落盘」模式（默认自动落盘 + 报告）。

**写入通道**：显式 `remember/recall` 工具（语义对齐 hindsight retain/recall）、
梦境自动抽取、hindsight 迁移脚本（bank→目录、memory/observation→fact md、
mental model/directive/KB 树→同名目录，`~/.hermes/backups` 为迁移源）。

**项目记忆（分域）**：全局个人记忆之外，按项目隔离——
| 域 | 位置 | 说明 |
|----|------|------|
| 项目域 | `<repo>/.agents/memory/<kind>/*.md` | 项目约定/架构决策/坑，默认进 git（团队共享、可回滚） |
| 项目本机 | `<repo>/.agents/local/memory/` | gitignore，敏感/个人备注 |
| 全局域 | `~/.chyan/memory/<kind>/*.md` | 个人偏好/跨项目事实（D12 修订：随 `~/.chyan/` 一并备份换机） |
- 格式与全局同构（同一 frontmatter），FTS5 合并索引标注域；加载/检索优先级：项目 > 全局。
- **P2 分期口径**：B3 先支持**全局 + 项目两域读写**（handoff 的 Decisions/Corrections
  双保险依赖项目域写入），检索按 项目 > 全局 简单排序；本机域、FTS5 域合并索引、
  信任分级随 P4-3 落地。
- 指令源：全局 `~/.agents/AGENTS.md` → 项目 `AGENTS.md` → `CLAUDE.md` → `.cursor/rules/*`（按文件名排序），
  后者覆盖前者的冲突；渲染为 system prompt 末尾的 `## Project instructions` 段（单文件 16KB、
  合计 40KB 上限，超出截断并注明）；`[context] load_project_instructions = false` 关闭。
- 梦境归属判定：含项目路径/模块/约定 → 项目域；个人偏好/跨项目 → 全局域；
  拿不准 → 全局 + 报告标「待确认归属」可改。
- 外部 clone 仓库记忆降信任：按"证据不是指令"处理，directive 首次加载提示信任确认（workspace trust 模式）。

**健壮性与安全**：
- 单写者：所有记忆写经 memory 包串行化；md frontmatter 校验，坏文件跳过并告警不崩。
- **记忆是证据不是指令**：检索结果只走 tool/user 通道，不进 system prompt（防记忆投毒提权）；
- 删除权：物理删除 + 全量导出，来源可追溯。

### 4.4 技能（skills 兼容层）

- 加载 `~/.agents/skills/<name>/SKILL.md`，frontmatter 仅 `name`+`description`（已核实格式），
  正文 Markdown 作为注入指令。项目级 `.agents/skills` 可覆盖。
- 现有技能库（按目录分组）零成本接入。技能内脚本经 tools/bash 执行，受审批门控。

### 4.5 多端（daemon 为唯一核心）

- **daemon 双监听面**：本地面 = unix domain socket（**仅 Linux/macOS**，零 TCP 端口占用）；
  Windows 无本地面，daemon 走 TCP `127.0.0.1` + token（D30），无 token 时拒绝启动（不静默空跑）；
  `chyan tui` 默认地址按平台选择（Windows 走 `http://127.0.0.1:31415` 并要求 token）。
  远程面 = TCP `127.0.0.1:31415`
  （默认，bind 可配；端口前缀 314 经核对本机未占用），远程暴露由用户自选通道（Tailscale / nginx 反代 + token）决定。
- **任务调度**：并发上限默认 4（`[daemon] max_concurrent_tasks` / `--max-tasks`），超限任务保持
  `pending` 排队，拿到槽位才置 `running`；单任务超时默认 30min（`[daemon] task_timeout` /
  `--task-timeout`，从出队起算），超时置 `failed` 并写明超时值。优雅停机先停监听、再等在跑任务
  有界排空（5s），排队任务直接标 `interrupted`；启动时把上次进程遗留的 `running`/`pending`
  dispatch 标 `interrupted`（cron 行由调度器接管，不动），避免僵尸状态。
- **自愈与健康**：`GET /healthz` 返回存活状态与任务槽位占用（不走鉴权，供探活）；
  watchdog 周期扫描（默认 10 分钟，`watchdog_interval`，负值关闭）做三件事：① 中断超过
  `task_timeout + 5min` 仍无进展的 running 任务（`task_stalled`）；② 单会话 10 分钟内被拒
  操作 ≥5 次告警（`audit_deny_surge`）；③ 24h 无更新的活动会话标 `interrupted`（`session_stale`）。
  告警走 SSE `notice` 事件，并优先投回任务来源 thread（`im:owner`），无来源则投审批 IM chat。
- **调度指标**：`daemon.metric` SSE 事件两条——`im.turn`（排队时长/轮次时长/合并条数/成败，
  channel/chat/thread 标识）与 `task.turn`（排队时长/轮次时长/成败，task/session 标识），
  只观测不改调度，经 SSE 推给 TUI/PWA，不落业务表。
- **任务进度**：`task.progress` SSE 事件转发 thinking/tool_run/tool_done/reflexion 里程碑
  （工具、摘要、耗时、失败原因），供工作台执行时间线实时展示；reasoning token 与工具正文不进事件。
- **断线恢复**：SSE 断开后 TUI 每秒重连、PWA 指数退避重连（1s→30s）并重挂当前视图；
  两侧都有轮询兜底（TUI 3s / PWA 重连后刷新），状态不必依赖单条事件。
- **CLI**：`chyan`（交互 REPL，CLI 默认入口：多轮/markdown/`!cmd`/斜杠命令/中断当前 turn）、
  `chyan run "任务"`（非交互，供脚本/cron）、`chyan tui`（bubbletea TUI）全部是 **daemon 客户端**：
  任务经 `POST /tasks` + SSE 流式接收，审批票经 `/approvals` 回传，`status`/`compact`/`/reload`
  走对应 API。本机 socket 未监听时自动 detached 启动 `chyan serve` 并等 `/healthz`
  （`--no-autostart` / `CHYAN_NO_AUTOSTART=1` 关闭；Windows 走 TCP，需 `CHYAN_TOKEN`，
  token 不落盘，见 FR-11）。
  离线运维命令（export/import/backup/restore/prune/vacuum/diagnose/update/memory/trace/cron）
  仍本地直连数据目录——restore 必须在 daemon 停止时执行，不能让 daemon 代理。
- **TUI**：bubbletea，会话列表/审批面板（含 diff 预览）/上下文占用（继承 `/status` 可观测传统）。
- **Web 工作台/PWA**：桌面与移动端共用的主交互入口；已有任务发起/查看/取消、危险操作审批
  （含 diff 预览）、会话历史、按任务打开的统一执行时间线（会话消息 + 实时输出 + 工具进度 +
  审批/错误/产物链接）与**只读项目文件树**（list/read/查看 diff，不在线编辑——改代码用
  桌面编辑器），以及会话搜索/重命名/删除/导出、产物 artifacts 预览下载、**cron 与记忆管理**
  （见 4.6 的 Web 管理接口）；页头汇总全局运行任务数与待审批徽标，任务结束弹出提示并可在
  **显式授权**后发浏览器通知，daemon 断连时页头显示离线并置灰任务发起入口（不伪装可执行）；
  主工作台产品化增量按 FR-12 补齐。纯浏览器 SPA（Vite+TS），
  daemon 用 `go:embed` 嵌入静态资源经 HTTP 提供，首版无 Electron/Tauri 壳；
  "安装"= 浏览器添加到主屏幕/桌面独立窗口。远程访问走用户自选通道
  （Tailscale / nginx 反代 + token），离线不执行 Agent。
- **IM**（P4）：飞书/Telegram adapter 先行，复用 Hermes channel_directory.json 迁移配置。
  入站触发（D35）：适配层带 `InboundPolicy`（白名单 chat/user、限速、长度上限、
  `MaxReplyChars`、可选 `IntentFilter`），daemon 的 bridge 在转发 SSE 后起 agent run，回复经
  同一 channel 回投。
  - **身份粒度 = thread**（`channel + chat + provider thread id`）：飞书取 `root_id`、Telegram 取
    `message_thread_id`；每个 thread 映射一个会话，同 thread 串行、不同 thread 可并行。
  - **全双工队列**：每 thread 一条 FIFO 队列（上限 8，超出丢弃并回执"队列已满"）；有 turn 在跑
    时入队并立即回执（排队位置/处理中），不阻塞入站；队首任务在**下一 turn 边界**把排队消息
    合并成一批（合并后共享一次 task slot）。
  - **输出裁剪**：回复超过 `MaxReplyChars`（默认 800）截断，并附 Web/PWA 链接指向完整内容；
    `/new` 清空映射并开新会话。
  - **意图过滤**：`IntentFilter` 非空时只处理匹配的消息（默认关闭，适配层可开）。
  - **常驻 Router（实验性，默认关）**：`CHYAN_ROUTER=1` 开启；每 channel 一个无工具常驻会话，
    仅在 thread 冷启动（无既有会话映射）时把请求改写为任务指令交给 worker，失败回落原文；
    指令上限 4000 字。
  - **防回环**：`FromBot` 与重复 message_id 直接丢弃。
  - **KV 键**（`<thread>` = `channel:chat` 或 `channel:chat:t:<thread id>`）：
    `im:session:<thread>`（thread→会话）、`im:sub:<thread>`（订阅直投目标）、
    `im:owner:<sessionID>`（会话→发起 thread，用于把告警/子任务结果投回来源）、
    `im:router:<channel>`（Router 会话）。

### 4.6 数据与存储

- **work_dir（工具操作边界）**：CLI 启动时的当前目录（cwd），可用参数显式指定（--work-dir），
  进程生命周期内固定；TUI 为启动目录 + 会话内可切换。文件工具的 work_dir 限制、敏感文件
  黑名单、相对路径解析均以它为基准（见 FR-2）。
- **目录约定**：跨工具可共享的知识/约定统一放 `.agents/`（skills/memory/agents/AGENTS.md）；
  chyan 私有数据集中放 `~/.chyan/`（`os.UserHomeDir()` 拼接，三平台统一为主目录下，不再用
  UserConfigDir/UserCacheDir 分散）：
  ```
  ~/.chyan/
  ├── config.toml        # 配置（自 ~/.config/chyan 迁入，D12 修订）
  ├── chyan.db           # SQLite 单文件
  ├── secret/<name>      # 凭据（明文 + 0600）
  ├── artifacts/         # 超限工具输出的完整原件（不随 cache TTL 清理）
  ├── state/cron/        # cron 任务跨轮状态（phase/attempts/falsified）
  ├── logs/              # 按天日志 chyan-YYYYMMDD.log（0600，保留 90 天，3 天后压缩为 .zip，写入前脱敏）
  └── cache/             # 工具中间产物（24h TTL）
  ```
  私有数据不进 `.agents/`（避免被 git 提交或被其他工具读取）；`.agents/` 不进 `~/.chyan/`
  （跨工具共享知识是公开资产，边界各管各的）。**迁移**：首启检测旧路径
  （`{UserConfigDir}/chyan`、`{UserCacheDir}/chyan`）有数据则搬入 `~/.chyan/` 后留一次性提示。
- **cache 目录**（`~/.chyan/cache/`，工具中间产物）：布局 `cache/<sha256>/<action>/<时间戳>-<文件>`
  （按内容寻址分桶，同产物不同次生成不互相覆盖）；**只收异步工具的显式产物**（下载/截图/TTS
  音频/转写稿等有实体文件的输出），同步查询类结果走摘要机制不落 cache。按 mtime 判龄，
  **默认 24h 后自动清理**（`cache_ttl_hours` 可配）；清理三触发点：启动时惰性清理（主）+
  夜间 dream 任务顺带 + 写入时抽查（防 daemon 长期不重启）。工具返回路径时附提示
  "24h 后清理，重要产出请 move 到 work_dir"——cache 只放中间产物，要留的产出落 work_dir。
  cache 随 `~/.chyan/` 一体备份/删除，不依赖 OS cache 清理工具。
- SQLite 单文件（`~/.chyan/chyan.db`）：session / message / task / memory / audit / kv / trace_span。
  message 内容大字段 JSON，超限截断（truncated 标记，落库口径见 4.1）。
- trace_span 表：每 turn 一个 `trace_id`，分 `turn / llm_request / hook / tool_execution /
  tool_wait_approval / memory_recall / memory_write / subagent` span；只存脱敏、限长元数据
  （模型、attempt、ttft、token/cache、成本、错误分类、结果字节数），不存 prompt、工具完整
  输入输出或记忆正文。审批等待单独计时，`chyan trace <id>` 输出模型/工具/等人工/编排四类耗时。
- task 表：异步任务，`kind: cron`（周期调度）/ `dispatch`（**一次性后台派发**——经 daemon
  API 由 PWA/IM/CLI 发起，FR-6「发起任务」落库即此）；子 Agent 为同步 session（parent_id，
  见 4.1.1），不入 task 表。
- 工作区选择（FR-12 W1）：`CreateTaskRequest.work_dir` 指定 daemon 机器上的已存在目录；
  `kv` 记录 `workspace:session:<id>`（续聊沿用原目录）与 `workspace:recent`（最近路径）；
  工具、AGENTS.md/项目记忆按该根组装，文件接口用 `?workspace=` 指定同一根。
- **Web 管理接口（FR-12）**：
  - 会话：`PATCH/DELETE /api/v1/sessions/{id}`、`GET .../export`（版本化导出信封，可直接
    `chyan import` 回放）；运行中任务占用会话时删除返回 409。
  - 文件：`GET /api/v1/files/download` 走同一沙箱；`source=artifacts` 切到
    `~/.chyan/artifacts` 根（绝对路径也必须落在根内），时间线中的 `[artifact: …]` 因此可直达。
  - cron：`GET/POST /api/v1/cron`、`PATCH/DELETE /api/v1/cron/{id}`、`POST .../run`；
    jobs.json 仍是真相源，调度器在线时由它持有运行时状态并串行落盘；每次运行后发
    `cron.run` SSE（含 `last_session_id`，Web 可跳回会话）。
  - 记忆：`GET /api/v1/memory`（按 q/domain/kind/project 检索或浏览）、
    `POST /api/v1/memory/{id}/supersede`（标记失效，不删文件）、
    `GET/POST /api/v1/memory/dream`（查看/立即运行梦境整理，报告落
    `~/.chyan/state/dream-report.json`）。
- **cron 任务状态**（`~/.chyan/state/cron/<job>.json`）：每次触发开新会话，状态文件是唯一
  续跑事实源——`phase / attempts / last_error / last_output(头 2KB) / falsified / evidence`；
  起跑前注入 `## Job State` 段并写 `running`（崩溃后可看出中断），结束后写回 idle/failed；
  任务 ID、文件 mtime 等易过期字段每轮重读，禁止复用状态里的旧值。
- session 表关键字段：`parent_id`（子 Agent 父链）、`agent`（agent 定义名）、
  `cost / tokens_*`（子 Agent 汇总进父）、`time_*`。
- audit 表关键字段：`action / resource / effect / review_mode / verdict / approved / actor`。
- 会话导入/导出：JSON（**P4 交付**，P2 仅 cost/tokens 汇总；兼容 opencode session export
  格式做导入器，可选）。
- 配置：TOML（沿用 chyan-agent config.toml 风格），密钥进 `secret/` 独立存储（不加密，D20），不进配置文件。

### 4.7 模型路由（llm）

- OpenAI 兼容 API 为主（覆盖大多数网关），anthropic 原生协议可选。
- **provider-model adapter**：每个 provider × 模型的能力差异（是否流式、reasoning 暴露、
  tool-call 增量格式、窗口、计价）封装在 adapter 层，模型窗口表是 adapter 能力矩阵的一
  部分。agent loop 能力协商：backend 实现 `Streamer`（流式契约，core.go）就走流式
  （TUI/PWA 实时输出、可中断体验），不支持则回退 `Chat` 非流式——流式是能用即用，
  不作为硬依赖。
- 多 provider 配置 + fallback 链 + 模型窗口表（移植 model_windows.json 机制）。
- **多模态**（D31）：消息 part 支持 `image`（OpenAI 兼容 `image_url`，base64 data URL 内联）；
  adapter 声明视觉能力，不支持时明确报错而非静默丢弃；图片 token 计入 usage。
  输入通道：CLI `--image`、REPL 粘贴、`read` 工具读图、PWA 上传、IM 入站图片、browser 截图回灌；
  单图 ≤5MB、单 turn ≤4 张、发送前长边压至 ≤2048，原图落 artifacts。
- **前缀稳定性**：工具定义按名排序、系统提示拼装稳定，保证 provider 前缀缓存可命中；
  reasoning 内容原样回传（`reasoning_content` 双向映射），缺失会触发 400 与重复计费。
- cost/tokens 累计落 session（对齐 opencode 统计字段）；只统计不控制（D36，预算拦截为非目标）。

### 4.8 MCP client（能力扩展主通道）

只做 client 不做 server（D17）——外部 MCP 服务（search、OSS 上传、playwright 等）是
chyan 的能力扩展生态，接入对齐 opencode 实测配置格式：

```toml
[mcp.servers.search]
type = "remote"                       # remote（HTTP url）| local（command 子进程）
url = "http://127.0.0.1:8080/mcp"
[mcp.servers.search.headers]
Authorization = "secret:search_token"       # 引用 secret/ 凭据名，不落明文

[mcp.servers.playwright]
type = "local"
command = ["playwright-mcp", "--headless"]
catalog_timeout = 30000               # 工具目录获取超时
execution_timeout = 30000             # 工具执行超时
disabled = false
```

- **工具注册**：MCP tool 经 adapter 实现 core.Tool，命名 `<server>_<tool>` 防跨服务冲突
  （下划线连接，对齐 opencode 实测与 function calling 惯例；前缀同时与内置工具名
  read/bash/task 等天然隔离，内置名保留不复用）；注册进 agent loop 与内置工具同权。
- **安全**：MCP 工具调用同样过三态审批（action=mcp）+ 输出统一截断；凭据经 secret/ 解析，
  修复 opencode.json 明文 Bearer token 的坑（MCP 运维经验）。
- **生命周期**：daemon 启动时连接 + 拉取工具目录；失败降级（该 server 工具不可用，
  不阻断其他）；重连退避。

### 4.9 配置与密钥

- **config.toml**（`~/.chyan/config.toml`，沿用旧版分段风格，路径见 4.6）：
  `[llm]`（provider/模型路由/fallback）、`[context]`（阈值覆盖）、`[permission]`（三态规则）、
  `[review]`（审核模式 `mode`、LLM 兜底审核模型 `model`、审批超时 `escalate_timeout`）、
  `[mcp.servers]`（见 4.8）、`[tools]`（截断/超时/循环上限）、`[cache]`（TTL）。
- **三级 markdown 扩展**（对齐 opencode 及格线）：agent（`.agents/agents`，见 4.1.1）、
  command（`.agents/commands/*.md`，斜杠命令 prompt 模板：frontmatter description +
  正文 $ARGUMENTS 模板）、skill（`.agents/skills`，见 4.4）；均为 全局 `~/.agents/` +
  项目 `.agents/` 两级，项目覆盖全局。
- **密钥**（`~/.chyan/secret/`，**不加密**，D20 修订）：凭据明文文件存 `~/.chyan/secret/<name>`，
  `0600` 权限 + 不放同步目录，防误提交/误同步，不防本机同用户进程。LLM API key、
  MCP headers token 等配置文件只引用凭据名（`secret:<name>`），凭据本体不进配置。
  **免恢复设计**：无 master key，同机重装/换机直接拷 `~/.chyan/` 目录即可；若 `~/`
  会被网盘同步，用户自行排除 `~/.chyan/`（威胁模型与取舍见 research.md D20）。
- **A2A 预留**（D18）：channels adapter 接口抽象为通用消息通道（IM 与 Agent 间通讯共用
  收发原语），daemon API 层留 AgentCard/task 语义位；不实现。

## 附录 A：默认参数（「实测」= 早期 Python 版本实测；「建议」= 新设计默认值。均可配置覆盖）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `reserved_tokens` | 4096 | 实测。硬上限预留（回复空间） |
| `trigger_ratio` | 0.7 | 实测。软阈值（历史占硬上限比例） |
| `trigger_bytes` | 204800 | 实测。字节双触发线（200KB） |
| `compress_target_ratio` | 0.8 | 实测。压缩目标 = 软阈值×0.8；0.7→0.56 的落差即滞回防抖 |
| `max_recent_turns` | 4 | 实测。最近轮保护（压缩不碰最近 4 轮） |
| `summary_ratio` / `summary_max_ratio` | 0.3 / 0.15 | 实测。摘要压缩比 / 摘要上限 |
| `tool_output_max_bytes` | 20000 | 实测。工具输出统一截断（20KB） |
| `bash_timeout` | 30s | 实测。bash 执行超时 |
| `web_max_bytes` | 20000 | 实测。web 抓取截断 |
| 文件读写上限 | 10MB | 实测。read/write 单文件上限 |
| `max_tool_iterations` | 25 | **建议**（弃旧版 5）。单段工具循环上限，覆盖典型编码链路 15–20 次调用；触顶不再直接失败，先 Reflexion 反思再接力（D28） |
| `max_tool_relays` | 3 | 建议。触顶后反思接力次数（0=不接力即失败；负值=不限，仅受中断/超时约束）。25×(1+3) 封顶单 turn 成本 |
| `max_repeated_tool_failures` | 3 | 建议。同一 `tool+参数` 签名在同一 turn 内失败 N 次后代码层拦截（0=默认 3，负值=关闭）；成功一次即清零 |
| 子 Agent 并发上限 | 4 | 建议。单机 goroutine + LLM 限速下够用 |
| 子 Agent 工具迭代上限 | 15 | 建议。子任务短平快，比主循环更紧 |
| `sub_agent_timeout` | 10min | 建议。覆盖调研/单文件实现，超时应拆任务 |
| `review.escalate_timeout` | 60s | 建议。human 模式 ask 超时降级 ai 裁决（D24） |
| `review.model` | 未设 = 纯规则 | 建议。LLM 兜底审核模型（D23），不配则规则拿不准直接升级人工 |
| daemon 并发任务上限 | 4 | 已实现（`[daemon] max_concurrent_tasks`）。超限排队并回执；单任务硬超时 30min（`[daemon] task_timeout`，从出队起算） |
| 图像输入上限 | 单图 5MB / 单 turn 4 张 / 长边 2048 | 已实现（D31）：超限拒绝；发送前按长边 2048 缩放（jpeg→jpeg，其余→png），原图落 artifacts |
| 日志保留 | 90 天（按天轮转，3 天后压缩为 zip） | 已实现（D34/FR-11）。0600，只记元信息，secret 与正文写入前脱敏；`CHYAN_LOG_LEVEL=off` 关闭 |
| `chyan prune` | 默认 dry-run | 已实现（D34/FR-13）。`--kind session\|audit\|trace\|artifacts` + `--older-than`，`--yes` 才实删 |
| `expand` 窗口 | 20000 字节 | 已实现。单次续读上限（与工具输出截断同量级，避免再次触发 spill）；`offset` 续读，路径限 artifacts 根内 |
| 经验自动注入 | top 3 / 每条 400 字 | 已实现。每轮输入 triggers 确定性匹配，命中作为 user 消息前置段；未命中不注入（D43） |
| IM 每 thread 队列 | 8 条 | 已实现。超限丢弃并回执；忙时回执排队位置；turn 边界合并成批 |
| IM 回复裁剪 | 800 字 | 已实现（`InboundPolicy.MaxReplyChars`）。超出截断并附 PWA/Web 链接；`DefaultMaxReplyChars` |
| IM 意图过滤 | 默认关 | 已实现（`InboundPolicy.IntentFilter`）。开启后仅 @/斜杠/疑问句/请求措辞触发 agent |
| IM 去重窗口 | 10 分钟 | 已实现。同 message_id 在此窗口内直接丢弃（防重投回环） |
| daemon watchdog | 10 分钟（负值关） | 已实现。卡死任务（timeout+5min）/ 会话 10min 内拒绝 ≥5 / 会话 24h 无更新 |
| 常驻 Router | 默认关（`CHYAN_ROUTER=1` 开） | 已实现（实验性）。每 channel 一个无工具会话改写冷启动请求，上限 4000 字，失败回落原文 |
| `bash` 默认权限 | ask | 三态权限 P3 上线后生效；P2 期 bash 仅受危险命令拦截 + work_dir 约束（单人 dogfood 场景）。chyan 默认审核模式 ai，bash 的 ask 由 AI 审核员自动裁决（低危放行/高危拒绝/拿不准升级人工） |

## 5. 非功能约束

| 项 | 目标 |
|----|------|
| 常驻内存 | daemon 空闲 < 80MB（GOMEMLIMIT 可调） |
| 跨平台 | Linux / Windows / macOS 单二进制（GOOS/GOARCH 交叉编译） |
| 启动 | CLI 冷启动 < 300ms（TUI 可用） |
| 安全 | 凭据独立存储（0600+gitignore）、工具审批门控、audit 审计、敏感文件保护（继承现有黑名单机制） |
| 可观测 | 上下文占用/工具调用/记忆召回可打印可审计（继承可解释传统） |

## 6. 分期里程碑

| 期 | 内容 | 验收 |
|----|------|------|
| P2 内核 MVP | core + llm（流式/adapter）+ tools（文件/bash）+ MCP client + skills 兼容 + CLI run + SQLite | 编码类任务与助理类任务各跑通 1 条真实链路；阈值计算用例自 git 历史 test_context.py 移植适配 |
| P3 多端与安全 | daemon HTTP/SSE + PWA（查看/发起/审批）+ TUI + 三态权限/审批 + audit | 手机发起任务全链路走通；危险操作有审批拦截 |
| P4 助理能力 | cron + channels（飞书/TG）+ memory(hindsight 迁移) + 浏览器 | hermes 常用 cron 任务与 IM 通知迁移可用 |
| P5 打磨发布 | TTS/语音、性能打磨、文档、打包发布、旧工具退役 | 三平台安装包；dogfood 两周无阻断问题 |

替换策略：P2 起用户本人日常切换（dogfood），P5 验收后退役 opencode/hermes；
对外发布以 P5 为准入。

## 7. 风险

| 风险 | 缓解 |
|------|------|
| 全量继承范围失控 | 严格分期；P2-P3 首发面收敛；重模块（浏览器/TTS）P4-P5 |
| 开源盈利未定（D8） | 架构预留技能包/企业版/支持订阅扩展点，不实现；商业模式后置决策 |
| session 数据膨胀 | 工具输出截断 + JSONL 归档 + DB 真空策略 |
| IM 平台 API 变更 | channels adapter 接口隔离，单渠道故障不影响内核 |
| 单人维护压力 | Go 生态胶水优先（SDK 成熟度已调研）；Python 保留为算法参考 |
| 成本无拦截（D36） | 只统计不控制；靠 cron 频率约束、每轮元信息与人工巡检；必要时后续加预算（需需求变更） |
| 图像输入成本/兼容 | 视觉能力协商 + 张数/体积上限 + 长边压缩；非视觉模型明确报错 |

## 附录 B：文件敏感清单与危险命令规则（与代码同步）

代码为准：`chyan/internal/tools/fs.go`（`sensitiveFiles` / `sensitiveRes` / `excludeDirs`）与
`chyan/internal/tools/bash.go`（`dangerRules`）。需求 §FR-2 引用本清单，改动规则须同步本节与测试。

**敏感文件名（拒绝 read/write/edit/delete/grep）**：`config.toml`、`config.example.toml`、
`.env`、`.env.local`。

**敏感路径模式（大小写不敏感，按绝对路径匹配）**：`.ssh/`、`.gnupg/`、`.git/config`、
`.git/credentials`、`id_rsa|id_dsa|id_ecdsa|id_ed25519`、`*.pem|*.key|*.p12|*.pfx|*.keystore`、
`credentials`、`.netrc`、`.npmrc`、`.pypirc`、`.env.<suffix>`、路径中任意段含 `secret` 的文件。

**遍历排除目录**：`.venv`、`venv`、`.git`、`__pycache__`、`node_modules`、`.idea`、`.vscode`
（以及点开头目录）。

**危险命令规则（bash 执行前拦截，命中即拒绝并回错误）**：

| 规则 | 匹配 |
|------|------|
| rm-root / rm-home | `rm <flags> /…`、`rm <flags> ~…` |
| mkfs | 命令位 `mkfs*` |
| dd-to-device | `dd … of=/dev/` |
| fork-bomb | `:(){…};:` |
| system-power | 命令位 `shutdown|reboot|halt|poweroff` |
| sudo | 命令位 `sudo` |
| redirect-blockdev | `> /dev/sd*|nvme*|hd*|xvd*|vd*` |
| chmod-root | `chmod -R 777 /` |
| pipe-to-shell | `curl|wget … \| sh|bash|zsh|dash|ksh` |
| git-force-push | `git push … -f|--force` |

命中即拒绝执行并回错误，**不进入审批流程**（无法被批准放行），模型需改用合规方式。

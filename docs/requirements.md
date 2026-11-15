# chyan 需求说明

> 状态：v4（2026-11-02 修订；v3 = 2026-11-01；v2 = 2026-10-29；v1 = P1 定稿 2026-10-15）
> v2 修订：新增 FR-10 图像输入、FR-11 数据治理与运维；补「取消运行」「AGENTS.md 指令源」「IM 入站触发」
> 「更新语义」「Windows 平台口径」「隐私外发」「升级兼容」；每条 FR 加状态与验收要点。
> v3 修订：IM 入站细化到 thread（队列/忙时回执/合并/回复裁剪/意图过滤）、daemon 自愈（watchdog +
> `/healthz`）与调度指标、工具输出 `expand` 链式续读、常驻 Router（实验性、默认关）；
> 触发式经验注入恢复，但**只走 user 通道**（不进 system prompt），修订 D32（见 D43）。
> v4 修订：对外定位从「替换 opencode + Hermes」调整为「开源、本地优先的个人 Agent 工作台」；
> 新增 FR-12，明确 Web/PWA 为主交互入口及任务发起、执行时间线、工作区、自动化与记忆管理需求。
> 定位：呈言科技出品的开源、本地优先「个人 Agent 工作台」（Go 实现）：以 Web 工作台为主入口，
> 统一承接编码、文件处理、浏览器操作与定时自动化；可接 OpenAI 兼容模型，数据与会话由用户自己掌握。
> 设计参考 opencode 与 Hermes 的实践，不绑定其运行时，也不作为对外价值主张。
> 调研/决策见 [research.md](research.md)，架构见 [architecture.md](architecture.md)，
> 计划见 [development-plan.md](development-plan.md)，阻塞与实测台账见 [TODO.md](TODO.md)。

## 0. 口径约定

- 需求只写「做什么 + 怎么算做完」，实现方案与参数默认值归 architecture.md，二者冲突以本文件为准并同步修订。
- 每条 FR 带状态标签：`已交付`（代码 + 测试证据齐）、`部分`（列出缺口）、`未开始`。
  状态只是导航，准入以 §4 验收为准。
- 编号只追加不重排：新增需求接续编号，废弃需求标「废弃」并保留条目。

## 1. 产品目标

- **统一完成工作**：一个 Agent 同时覆盖编码、文件处理、浏览器操作与个人自动化，统一工作区、会话、权限、记忆与产物，不要求用户在编码工具和助理工具之间搬运上下文。
- **Web 工作台是主交互入口**：桌面浏览器与可安装 PWA 提供完整任务闭环；移动浏览器复用同一工作台。CLI / REPL / TUI 是高级用户与脚本入口，IM 承担移动触发、通知与审批，不再以终端界面代表产品的主要形象。
- **本地优先、用户自持**：单机私有部署，会话、记忆、产物与自动化数据由用户掌握；可接通用模型（OpenAI 兼容 API），不绑定特定模型厂商或 IDE。
- **持续执行而非单次问答**：前台会话、后台任务、cron 与 IM 触发共享同一 Agent 内核，运行状态、审批和结果可跨端恢复。
- 输入面：文本 + 图片（FR-10）；输出面：Web 工作台 / CLI / REPL / TUI / IM。
- 常驻内存低、跨 Linux/Windows/macOS，按工具软件定位（不做 SaaS、不做生成式 AI 备案）。

## 2. 功能需求（按能力域）

### FR-1 Agent 内核 — 已交付

- agent loop：多轮对话、工具循环（默认上限 25 次）；触顶先 Reflexion 自我批判再接力，接力次数可配（默认 3）；
  同一 `工具+参数` 在单 turn 内失败 3 次后代码层直接拦截（成功后清零，跨 turn 不封禁）。
- 子 Agent：`task` 工具派发（独立 session/上下文/工具循环）；**支持匿名派发**（不传 agent 名即用默认
  模型+父权限+通用提示，无需预定义文件），命名 agent 定义在 `~/.agents/agents/*.md`（角色卡可复用）；
  上下文隔离（只回传结果摘要，完整过程可 `read_session` 回查）；权限只收紧不放大；
  嵌套上限见 architecture 4.1.1（子 Agent 不得再派子 Agent）；并发/超时有限额；父子任务树可观测、可中断。
- 上下文预算：窗口分层解析（配置 > 联网目录 > 内置表 > 兜底）；**压缩降级为小窗口兜底**
  ——1M 窗口 + prompt cache 的现代模型常态零压缩，小窗口模型超软阈值才压：
  数据层永不压缩（SQLite 全量真相源，`read_session` 可回查）；只在 turn 边界/工具循环间隙
  且 tool-call 对完整时压缩；**handoff 交接**（Intent & Corrections / Decisions / Done / Active / Next /
  Evidence / References）接近硬阈值或 `/compact` 手动触发，note 存 session 附属，决策与用户纠正双写项目 memory。
  任务清单在 handoff 的 Active/Next 段维护，**不做独立 TodoList UI**。
- 工具输出统一按字节截断（`tool_output_max_bytes`）；超限时**完整原文**落
  `~/.chyan/artifacts/<session>/…`（0600，不随 cache TTL 清理），message 内保留路径 + sha256 指针，
  保证结论可回溯且 session 不膨胀；模型可经 `expand` 工具按 offset/limit 窗口续读
  （单次 ≤20k，越界路径拒绝），handoff 引用同口径。
- 模型适配：OpenAI 兼容 API；多 provider 路由 + fallback 链（按序重试、按 spec 换 model）；
  **推理模型的 reasoning 内容原样回传**（缺失即 400，禁止把思考文本混进可见正文）；
  **工具定义与系统提示前缀必须稳定**（顺序确定，保证 provider 前缀缓存可命中）；
  流式优先、不支持回退非流式；流式请求不得被整请求超时截断。cost/tokens 按 session 累计。
- **运行取消（新增，全端）**：`Interrupt` 覆盖 LLM 请求、工具执行、子 Agent，协作式中断（秒级返回）；
  取消后已完成的消息与工具结果照常落库，session 记录 `interrupted` 状态；重复取消幂等；
  取消不触发 fallback 重试。端侧入口见 FR-4（CLI/REPL/TUI）与 FR-6（API/PWA）。

### FR-2 工具 — 已交付

- 文件工具（read/write/edit/list/search/grep/delete）、bash（受限执行）、browser（playwright 桥）、
  `expand`（超限输出与 handoff 窗口续读，只读）、TTS/转写（FR-8）、web fetch/search（P4 交付，代理支持、SSRF 拒绝、去标签）。
- 工具安全：`work_dir` 限制（文件操作不出工作区）、敏感文件黑名单、危险命令拦截。
  黑名单与命令规则清单固化在 architecture.md 附录 B（与代码同步维护，不再引用已删除的旧 Python 实现）。
- edit/write 写前冲突校验：基于上次 read 的 mtime/hash，文件被外部改动则拒绝并提示重读。
- 工具中间产物落 `~/.chyan/cache/`，按 mtime 默认 24h 自动清理（`cache_ttl_hours` 可配），
  清理三触发点（启动惰性 / 梦境顺带 / 写入抽查）；重要产出提示 move 到 work_dir。
- 图片读取能力见 FR-10。

### FR-3 扩展机制 — 已交付

- 三级 markdown 扩展，均为 全局 `~/.agents/` + 项目 `.agents/` 两级、项目覆盖全局：
  - agent：`agents/*.md`（frontmatter `description/model/permission` + 正文系统提示）；
  - command：`commands/*.md`（frontmatter description + 正文 prompt 模板，`$ARGUMENTS` 占位，对齐 opencode）；
  - skill：`skills/<name>/SKILL.md`（frontmatter 仅 name+description），兼容现有技能库，无需改造。
- 技能内脚本经 bash 工具执行，受审批门控。

### FR-4 CLI / REPL / TUI — 已交付

- **统一 daemon 客户端**：`chyan` REPL、`chyan "任务"`、`chyan run`、`chyan status`、`chyan compact`
  均通过 REST+SSE 与 daemon 交互（任务流、审批票、会话读写），不再在进程内组装 app；
  本机 socket 未监听时自动 detached 启动 `chyan serve` 并等待 `/healthz`
  （`--no-autostart` / `CHYAN_NO_AUTOSTART=1` 关闭）。离线运维命令仍本地直连数据目录。
- **CLI 默认交互入口**：`chyan` 无参数进入 REPL（多轮对话、流式输出、markdown 渲染与代码高亮、
  主题切换、`!cmd` 本地 shell 逃逸并记入上下文、斜杠命令 `/status` `/compact` `/theme` 等、
  每轮元信息行 `model · 耗时 · 上下文 · 累计 token/成本 · session id`）。
- `chyan "任务"` 等价 `chyan run "任务"`：非交互一次性执行，供脚本与 cron；
  flags：`--model/--agent/--session/--continue/--fork/--format/--work-dir/--home/--auto/--no-autostart`。
- `chyan status`（上下文占用）、`chyan compact --session`（手动触发 handoff）、`chyan version`。
- **取消**：REPL 内 Ctrl+C 中断当前 turn（保留会话、不退出进程；审批答题也会立即释放），
  空输入时 Ctrl+C/Ctrl+D 退出；`chyan run` 收到 SIGINT 走同一中断语义，退出码区分「被中断」与「失败」。
- **TUI**（`chyan tui`，对接 daemon）：对话流 + 审批面板（含 diff 预览）+ 会话列表（切换/打开）
  + 上下文占用 + 发起任务 + **运行中任务取消（`ctrl+x`）** + 刷新/退出。
  命令面板（`ctrl+p`，可过滤动作）、多会话 tab（`ctrl+←/→` 切换、`ctrl+w` 关闭）、MCP 状态展示；
  会话列表内 `/` 过滤、`r` 重命名（≤20 字）、`x` 删除（y/n 二次确认）、`e` 导出。

### FR-5 权限审批 — 已交付

- 三态规则 allow/ask/deny；action = **工具名**（`read / write / edit / list / search / grep /
  delete / bash / browser / expand / web_fetch / web_search / tts / transcribe / task / remember /
  recall / read_session`，MCP 工具为 `<server>_<tool>`）或 `*` 通配。
- resource 为通配符，匹配对象：文件类工具 = 工作区相对路径；bash = 命令行；其余工具取入参
  `path / command / cmd / file / name`，都没有则回落为工具名。
  **限制**：暂无 web/mcp 类级规则（限制 web/MCP 只能按具体工具名或 `*` 配置），
  web 工具的 URL/query 当前也不参与资源匹配。
- 规则优先级：先比模式特异性（**字面量模式优先于通配符**），同级命中取 **deny > ask > allow**；
  无命中时 `read` 默认 allow，其余默认 ask。显式 deny 任何模式下硬拦（`full` 也不例外）。
- agent 级权限覆写（`~/.agents/agents/*.md` frontmatter）；子 Agent 有效权限 = 父权限 ∩ 声明权限，只收紧。
- **审核模式三选一**：`ai`（默认：规则启发式先行 + LLM 兜底，low 放行 / high 拒绝 / uncertain 升级人工）、
  `human`（人工审批）、`full`（自动放行，等价 opencode `--auto`）。
- **审批超时**：human 模式 ask 超时（默认 1 分钟）→ 降级 ai 裁决；ai 仍 uncertain → 按拒绝处理，不回环。
- ask → 按模式裁决 →（人工时）SSE 推送 → 批准/拒绝；audit 表记录全链路
  （action/resource/verdict/审核模式/判定理由/父子链路）。
- 审批单附**改动 diff/文件片段**（只读预览），TUI/PWA/IM 审批面板同享。

### FR-6 daemon 与 PWA 基础能力 — 已交付（主工作台产品化见 FR-12）

- 常驻 HTTP server + SSE 事件流 + bearer token 鉴权；双监听面（见 §3 平台矩阵）：
  Linux/macOS 本地面 = unix socket（零端口，信任本机用户），远程面 = TCP + token。
- 任务：发起、列表、详情、**取消（新增 `POST /api/v1/tasks/{id}/cancel`）**；SSE 推送
  任务状态/进度/审批事件；断线后可用轮询恢复状态。
- 并发：同时运行任务数上限默认 4（可配），超限排队并回执；单任务硬超时默认 30 分钟（可配）。
- 自愈与健康：`GET /healthz` 返回存活状态与任务槽位占用；watchdog 周期扫描（默认 10 分钟）
  停滞的 dispatch，中断并告警，告警优先投回任务来源 thread；周期可配、负值关闭。
- 调度观测：SSE `daemon.metric` 事件输出 IM 与 daemon 任务的排队时长、turn 时长、合并条数、
  成败，只观测不改变调度。
- PWA（纯浏览器 SPA，go:embed，无 Electron/Tauri 壳）：token 登录（本地保存，可退出清除）、
  **统一的会话工作台**（桌面左侧栏为按时间分组的会话列表，主区为新建任务/转录/执行时间线；
  任务与会话不再分设入口）、**对话内容搜索弹窗**（检索消息正文，展示会话标题/日期/命中片段并高亮）、
  **取消任务**、审批（含 diff 预览）、父子任务树、
  **只读项目文件树**（list/read 查看与 diff，不支持在线编辑）、**亮/深/跟随系统三态主题**
  （偏好持久化，首屏无闪烁）；远程访问由用户自选通道（Tailscale/反代）。
- 进程生命周期：SIGINT/SIGTERM 优雅退出（先停止接收、再等在跑任务有界排空、最后清理 socket）；
  daemon 崩溃或重启后在跑任务标记为 interrupted，不留「永远 running」的脏状态。

### FR-7 助理能力 — 已交付

- **cron 定时任务**（`~/.chyan/jobs.json`，首次使用显式 `chyan cron import` 迁移 hermes，不隐式执行）：
  - 调度：5 字段表达式、`@every <dur>`、`@at <RFC3339>`（一次性）；
  - 任务形态：prompt / `script` + prompt（唤醒门：脚本末行 `{"wakeAgent": false}` 则本轮零 LLM）/
    `no_agent`（脚本 stdout 直接投递）/ `after: [上游任务]` 事件链（上游成功即触发，含环防护）/
    `context_from: [上游任务]`（注入上游输出）；
  - 状态：`~/.chyan/state/cron/<job>.json`（phase/attempts/last_error/last_output/falsified/evidence），
    每次触发开新会话，崩溃后可从中断处续跑；易过期字段每轮重读；
  - 并发：同一任务不重叠执行；daemon 停止时取消在跑任务。
- **IM 渠道**（首发飞书 + Telegram，channels adapter 接口）：
  - 出站：cron 任务结果投递（`channel = "feishu:<chat_id>"`）；失败重试与单渠道独立降级；
  - 审批：审批单（id + diff 摘要）推送 + 文本裁决 `approve|deny <id> [reason]`；
  - **入站触发（新增，本期）**：白名单用户/chat 的消息触发 agent run，回复回投原 chat/thread；
    身份粒度 = **thread**（channel + chat + provider thread id）：每个 thread 映射一个会话
    （`/new` 开新会话），同 thread 串行、不同 thread 可并行；
    **全双工队列**：忙时立即回执（排队位置/处理中），下一 turn 边界合并排队消息，队列上限 8 防洪水，
    worker 共用 daemon 任务槽位；入站图片按 FR-10 处理（飞书 image_key 与 Telegram photo 均已接入）；
    **回复裁剪**：按 channel 限长（默认 800 字），超长截断并附整段 PWA 会话链接；
    **意图过滤（可选，默认关）**：确定性规则（@、问号、斜杠命令、中英文请求动词）命中才起模型 turn；
    防回环：忽略 bot 自身消息与重复 message_id、限速（默认 2s）与长度上限（默认 4000 字符）；
    未授权消息静默丢弃并记日志；白名单为空 = 全部拒绝（开关打开也不放行）；
    **常驻 Router（实验性，默认关）**：`CHYAN_ROUTER=1` 开启，每 channel 一个无工具常驻会话，
    仅在 thread 冷启动时把请求改写为任务指令交给 worker，失败回落原文。
- **记忆**：三层（工作/交接/长期）+ 四类长期记忆（facts/kb/心智模型/指令）；
  md 文件为真相源（frontmatter + keywords + `supersedes` 链）+ FTS5 索引缓存（可重建，中文 bigram 分词）；
  分域：项目 `<repo>/.agents/memory`（默认进 git）> 项目本机 `<repo>/.agents/local/memory` > 全局 `~/.chyan/memory`，
  检索按 项目 > 本机 > 全局加权；just-in-time 加载（recall 命中路径后才读全文）；
  **指令源（新增）**：加载全局 `~/.agents/AGENTS.md` + 项目 `AGENTS.md` / `CLAUDE.md` / `.cursor/rules`，
  作为 directive 指令段注入 system prompt（仅指令源走 system 通道，检索类记忆不进 system prompt，防投毒）；
  项目指令源默认加载，可配置关闭（`[context] load_project_instructions`）；
  **踩坑记忆**：`triggers` 字段随记忆存储；命中时把最多 3 条（每条 ≤400 字）经验作为
  **本轮 user 消息**注入（不进 system prompt，D43 修订 D32），其余召回走 `recall` 显式检索；
  **夜间梦境**：抽取/查重/supersede/梦境报告/项目归属判定，可回滚；
  `~/.hermes` hindsight 迁移（MEMORY/USER/mental model/directive，无损、可重跑幂等）。
- web_fetch / web_search 工具（代理支持；反爬受限站点继续经 MCP/浏览器通道）；
  浏览器自动化（playwright 桥，截图路径可直接作为 FR-10 的图像输入）。

### FR-8 语音与发布 — 已交付

- TTS（对接本地 kokoro 等服务）与语音转写（对接本地转写服务），产物落 cache 并在结果中回传路径。
- 三平台打包（linux/win/darwin × amd64/arm64）+ SHA256SUMS。
- **更新检查（新增语义）**：`chyan update` 查询最新版本并与当前版本比较，输出新版本号、发布说明链接、
  下载地址与 SHA256（供用户自行校验）；**只提示不自动替换，不提供安装动作**。
  网络不可达时静默失败并给出离线升级路径。
- 诊断包、备份恢复、数据保留见 FR-11；旧 opencode/hermes 退役按 P5 出口执行。

### FR-9 Plan-and-Execute 编排 — 已交付

- 两阶段模式，REPL 内 **Shift+Tab** 切换 Planner / Executor，默认 Executor：
  - **Planner**：只读调研（读类工具全开、bash 只读守卫、摘除写入/子 Agent/MCP 工具），产出「写入计划」
    （可列选项供用户挑选），经 `submit_plan` 定案并落盘（session KV）；**绝不写入**。
  - **Executor**：携带已定计划，用户发「执行意图」消息才按计划执行（意图判定内置提示词）；
    计划明显有误则执行后告知偏差而非擅自改写；无计划时为现状 ReAct 自由执行。
- Planner/Executor 提示词内置、不可覆盖，同主模型（不支持独立 plan.model）；输入层 bubbletea textinput
  （捕获 Shift+Tab，含行编辑与历史）。

### FR-10 图像输入（多模态）— 已交付

- 输入通道：
  - CLI/REPL：`--image <path>`（可重复）+ REPL `/image <path> [问题]`（终端粘贴路径后回车即可）；
  - 工具：`read` 对 png/jpg/webp/gif 返回可被模型直接看到的图像内容（与文本结果同一消息）；
  - PWA：任务发起时可附图片上传；
  - IM：入站消息中的图片附件（飞书 image_key、Telegram photo，自动下载后随 turn 发送）。
- 模型侧：OpenAI 兼容 `image_url`，图片以 base64 data URL 内联发送（不依赖外链可访问性）；
  `llm` adapter 做视觉能力协商——`[llm] vision="off"` 直接拒绝图片输入；provider 对图片返回 4xx 时
  在原错误上追加「模型可能不支持视觉，建议 OCR/转写或换模型」，不静默丢弃图片。
- 截图回灌：browser 工具截图路径可直接作为输入传给模型，无需人工转存。
- 边界：单图 ≤ 5MB、单 turn ≤ 4 张，超限拒绝并提示；原图落 artifacts 供审计（随 FR-11 prune 清理）；
  发送前长边压缩到 ≤ 2048（jpeg 保持 jpeg，其余转 png；超过 40MP 的解压炸弹直接拒绝），
  原图仍落 artifacts；图片 token 计入 usage 统计（provider 返回时）。
- 验收：假 LLM 断言 image part 序列化正确；真实视觉模型跑通「截图 → 提问 → 正确回答」；
  非视觉模型给明确错误；超限被拒。

### FR-11 数据治理与运维 — 已交付

- **备份**：`chyan backup -o <file>` 打包 `~/.chyan/` 全量（默认排除 cache，含
  config/db/secret/artifacts/state/memory；`--include-cache` 可一并打包），secret 保持 0600；
  `chyan restore -f <file>` 默认拒绝覆盖已有 `chyan.db`；显式 `--force` 时先在数据目录同级写
  `chyan-pre-restore-<时间戳>.tar.gz` 快照，再覆盖。
- **诊断**：`chyan diagnose -o <file>` 打包版本/配置（脱敏）/db 与 audit 摘要/环境信息；
  secret 永不入包，消息正文只出摘要。
- **保留与清理**：默认永久保留；`chyan prune --kind session|audit|artifacts --older-than <dur>`
  按类型清理（默认 dry-run，加 `--yes` 才实删；artifacts 按文件 mtime，删后回收空目录）；
  `cache` 仍按 24h TTL 自动清理。
- **DB 维护**：schema 版本化，迁移前自动 `VACUUM INTO` 快照（`chyan.db.bak-<时间戳>`），
  迁移失败拒绝启动并提示恢复路径；`chyan vacuum` 执行 VACUUM + REINDEX + ANALYZE。
- **日志**：daemon 与 CLI 写 `~/.chyan/logs/chyan-YYYYMMDD.log`（0600，按天轮转，默认保留 90 天，
  超过 3 天的日志自动压缩为 `chyan-YYYYMMDD.log.zip`；`CHYAN_LOG_LEVEL=off` 关闭文件日志）；
  日志只记元信息，secret 与消息正文脱敏（写入前过滤）。
- **可观测汇总**：`chyan status` 输出上下文占用 + db/artifacts/cache 体积 + 运行中任务数。

### FR-12 Web 工作台 — 已交付

> 已具备：统一会话工作台（新建任务 + 会话列表/转录 + 执行时间线）、图片上传、审批与 diff、
> 父子任务树、只读文件树、工作区选择与续聊、产物直达、cron 与记忆管理、亮/暗/跟随系统主题、
> token 登录、SSE 断线恢复与 PWA 安装（验收见 A13）。

- **主入口与首屏**：认证后直接进入可发起任务的工作台，而非运维仪表盘；参考聊天应用的信息架构，
  桌面左侧栏直接承载「新建任务 + 搜索 + 按 7 天内/30 天内/月份分组的会话列表 + 审批/设置」，
  并支持**展开 / 图标栏 / 完全隐藏**三态切换（偏好持久化，隐藏态由左上角菜单按钮恢复）；主区默认为
  新会话输入，点选侧栏会话即切换转录。移动端保留底部导航与页内会话列表；版本、存储体积等
  运维信息收进状态/设置，不抢占主任务流。
- **工作区与任务发起**：发起前可见当前 `work_dir`，可选择 daemon 所在机器的已有目录并记住最近工作区；
  未选择时使用 daemon 默认工作区。支持输入 prompt、附图片、可选 agent/model；支持新建任务与继续已有会话，
  继续会话沿用原工作区和权限上下文。提交期间防重复创建，成功后直接进入该任务/会话的执行视图。
- **统一执行时间线**：任务详情在同一时间线呈现用户消息、流式回答、工具调用及状态、关键输出摘要、
  子 Agent 进度、审批与 diff、取消/失败/完成状态和最终结果；不为看工具过程、审批和结果分别跳转到无关联页面。
  长输出默认折叠并可续读完整 artifact；流式刷新不得清空草稿、丢失滚动位置或重复追加消息。
- **任务与会话管理**：任务与会话是同一件事的两个视角，导航只保留单一「会话」入口；会话页统一承载
  新建任务、会话列表（含无任务的历史会话）、状态筛选、转录详情和执行时间线入口。可查看
  pending/running/succeeded/failed/interrupted 状态并取消运行；会话支持重命名、删除（二次确认）与导出；
  搜索以弹窗形式检索**消息正文**（非仅标题），结果显示会话标题、日期与命中片段并高亮关键词；
  可从历史会话继续任务，且重新打开页面或 SSE 断线重连后能恢复当前任务的真实状态，不依赖一次性事件。
- **工作区与产物**：保留只读文件树和 diff；时间线中的文件改动、工具产物与生成文件可直接定位到文件预览，
  文本/Markdown/代码/图片可查看，其他文件可下载或复制本机路径。工作台不做在线代码编辑，编辑交给用户现有编辑器。
- **自动化与记忆管理**：Web 端可查看、创建、编辑、启停和立即运行 cron，查看最近执行状态/错误并跳转关联会话；
  可按项目/全局域检索和查看长期记忆与梦境报告，并将错误记忆标记失效。写操作沿用现有权限与审计口径。
- **全局状态与桌面形态**：任何页面均能看到运行中任务数与待审批数并一键到达；任务完成、失败或待审批时
  在页面内明确提示，浏览器通知仅在用户显式授权后启用。PWA 可安装为独立窗口，但运行仍依赖 daemon，
  离线时显示断连状态，不伪装成本地离线 Agent。
- **安全边界**：工作区浏览、文件预览/下载、任务续聊和自动化管理均须鉴权；路径访问不得突破 `work_dir`
  与敏感文件规则。远程访问继续使用 token 与用户自选通道，Web 主入口不放宽 FR-2/FR-5 的工具与审批约束。

### FR-13 Agent 可观测与失败归因 — 已交付

> 目标是回答“慢在哪、贵在哪、错在哪、用了哪条记忆”，不是把 prompt 和大段工具输出无差别写进日志。
> 采用 chyan 自有、版本化的内部 trace 字段；本地记录不等于遥测，默认不向任何外部服务导出。

- **关联标识**：每次用户 turn 建立根 `trace_id`，所有事件携带适用的 `task_id`、`session_id`、`turn_id`、
  `agent_id`、`parent_agent_id`、`span_id`、`parent_span_id`；子 Agent 的模型、工具、成本必须能归并到父任务。
- **span 切分**：至少区分 `turn`、`llm_request`、`hook`、`tool_execution`、`tool_wait_approval`、
  `memory_recall`、`subagent`；记录开始/结束时间、时长、状态和低基数 `error_class`。等待人工审批与工具真实执行
  必须分开计时，重试不得隐藏在无法解释的总耗时中。
- **模型归因**：记录 provider/model、attempt、含重试总时长、TTFT（流式且可取得时）、input/output token、
  cache read/cache creation token、stop reason 与成本；provider 未返回的字段标为 unavailable，不得伪造为 0。
- **工具归因与错误契约**：记录 tool/call ID、脱敏参数摘要、执行时长、结果字节数/估算 token、是否截断、
  artifact 引用、重试次数和 `error_class`。错误至少分 `invalid_argument`、`permission_denied`、`not_found`、
  `timeout`、`unavailable`、`rate_limited`、`internal`；返回模型的错误须简短、指出可修正参数或下一步，
  traceback/堆栈只进本地受控诊断日志，不直接倾倒给模型。截断结果必须带缩小范围或续读 artifact 的指引。
- **记忆治理与归因**：召回/注入记录 memory ID、domain、status、source、supersedes、排序/命中原因、
  注入条数与字节/token 量、截断情况；写入、梦境合并、冲突检测、取代和人工失效均产生审计事件。
  已 supersede/失效记忆不得作为有效事实注入；评测必须覆盖“用户更新旧事实后仍引用旧值”的负例。
- **本地查询与汇总**：trace 元数据结构化落盘，单用户首版默认全量记录、不采样；提供
  `chyan trace <task|session|trace-id>` 的文本与 JSON 查询，汇总模型耗时、工具执行耗时、等人工耗时、
  编排空转、token/成本和错误分布，能够定位具体失败 span。同一份数据经 `GET /api/v1/traces`
  暴露给 Web 工作台（会话页“耗时归因”面板，按 turn 分组、可展开 span），端点与 CLI 共用
  `trace.Summarize`，不引入第二套聚合口径。
- **隐私与保留**：默认只记元数据和脱敏摘要，不记 prompt 正文、完整工具入参/出参、记忆正文与凭据；
  文件权限、轮转、保留和诊断包脱敏沿用 FR-11。远程遥测/OTLP 导出仍为明确的非默认扩展，不在首版实现。
- **验收**：A14；正常、重试、超时、审批等待、工具失败、子 Agent、记忆更新/失效和敏感信息脱敏均有自动化测试。

## 3. 非功能需求

- **交付形态**：单二进制（Web 工作台/PWA embed），零外部运行时依赖；首版不依赖 Electron/Tauri 等桌面壳；
  daemon 空闲内存 < 80MB；CLI 冷启动 < 300ms。
- **平台矩阵**：

  | 平台 | CLI/REPL/TUI | daemon 本地面 | daemon 远程面 |
  |------|--------------|----------------|----------------|
  | Linux / macOS | 全功能 | unix socket | TCP `127.0.0.1:31415` + token |
  | Windows | 全功能（TUI 需显式 `--url`，见下） | **不做**（非目标，走 TCP 本机回环） | TCP + token |

  Windows 上 `chyan tui` 的默认 local unix socket 不可用，必须显式
  `--url http://127.0.0.1:31415 --token <token>`（平台感知默认值列入 P6-R7）。

- **成本口径**：只做 cost/tokens 统计与展示（session 累计 + 每轮元信息）；**不做预算拦截、限流、超支拒绝**
  （明确非目标）。风险由 cron 频率约束与人工巡检兜底，见 §5。
- **隐私与数据外发**（默认值，均须可在配置中关闭）：

  | 外发点 | 默认 | 说明 |
  |--------|------|------|
  | LLM 端点 | 用户必配 | 唯一的强需求外发；本地模型可完全私有 |
  | models.dev 窗口目录 | 关（`online_catalog`） | 开启时只拉取公开模型表 |
  | 更新检查 | 关 | 仅手动 `chyan update` 触发 |
  | web_fetch / web_search | 无后台外发 | 仅当模型调用该工具时访问目标 URL / 提交查询词 |
  | MCP 远程 server | 关 | 用户显式配置才连接 |
  | 遥测 | 无 | 产品不含任何遥测/上报 |

- **升级与数据兼容**：升级不丢数据（schema 版本化 + 迁移前备份 + 失败拒绝启动）；
  session 导出 JSON 带格式版本，导入兼容旧版本与 opencode v2 导出（尽力兼容，失败给可读原因）。
- **安全**：凭据独立存储（`~/.chyan/secret/` 明文 + 0600，不加密，D20）；敏感文件保护；危险命令拦截；
  审批与 audit 全链路；daemon token 鉴权（TCP 面），token 由 `--token` 或 `CHYAN_TOKEN` 传入、
  不落盘；token 为空时远程 TCP 面不启动（仅本地面）；诊断包与日志脱敏。
- **可解释**：上下文占用、工具调用、记忆召回、审批记录、成本均可打印/查询/审计。
- **数据目录**：私有数据集中 `~/.chyan/`（config/db/secret/cache/artifacts/state/logs），
  与 `.agents/`（跨工具知识）分离。
- **开源**：许可证待定（D8/T1）；商业模式待定，仅预留扩展点不实现。

## 4. 验收标准

| # | 需求 | 验收条件 | 证据 |
|---|------|----------|------|
| A0 | 通用 | `go test ./...` 全绿（含 `-race`）；`internal/` 各包合并覆盖率 ≥ 80%（入口壳层不计）；Web 测试全绿且语句/分支覆盖率 ≥ 80%；`gofmt`/`go vet` 零告警 | CI + Go/Web coverage |
| A1 | FR-1 内核 | 编码任务与助理任务各 1 条真实链路（人工 dogfood + 假 LLM e2e 进 CI）；handoff 按新策略（窗口感知、turn 边界、数据层不压缩）验证；取消在 LLM 阶段/工具阶段/子 Agent 阶段均秒级生效且状态落库 | `app/e2e_test.go`、context/subagent 用例、`app/interrupt_test.go` |
| A2 | FR-2 工具 | 越界/黑名单/冲突校验/危险命令拦截用例全绿；截断与 artifact 指针（写入/回收）用例；`expand` 窗口续读与越界拒绝；cache 24h 清理实测 | `tools/*_test.go`、`core/guard_test.go`、`core/expand_test.go`、`app/refs_test.go`、`cmd/chyan/prune_test.go`（回收） |
| A3 | FR-3 扩展 | 123 个真实技能抽样解析通过；command `$ARGUMENTS` 渲染；项目级覆盖全局 | `skills/commands` 测试 |
| A4 | FR-4 端 | REPL 真实会话跑通（多轮/中断/`!cmd`/主题）；`run` 全 flags 与 `--format json`；TUI 取消运行（`ctrl+x`）实测 | `cmd/chyan` 测试 + 人工 dogfood |
| A5 | FR-5 权限 | 危险操作被审批拦截；deny 硬拦不产审批单；超时降级与 uncertain 拒绝有 audit；agent 级覆写与子 Agent 交集收敛 | `daemon/approval_e2e_test.go`、`app/permission_test.go` |
| A6 | FR-6 多端 | 手机发起任务全链路；发起→取消→状态 interrupted；5 个并发任务（含排队）各自独立 session 不串台；SSE 断线后状态可恢复 | `daemon/cancel_test.go`、daemon e2e |
| A7 | FR-7 助理 | hermes cron 任务迁移可用（After 链/wake gate/state 续跑有测试）；IM 出站 + 审批裁决 + 入站触发（白名单/防回环/会话映射）实测；hindsight 迁移 50 条无损；AGENTS.md/CLAUDE.md/.cursor 指令源按序注入且可关 | cron/channels/memory/app/instructions 测试 + 人工 |
| A8 | FR-8 发布 | 六目标打包 + SHA256SUMS；`chyan update` 在假 release 上正确判定新旧版本并输出下载信息与校验值；TTS/转写端到端 | `scripts/release.sh`、`update` 测试（`cmd/chyan/update_test.go`、`internal/update`） |
| A9 | FR-9 编排 | Planner 阶段写入类工具不可用且 `submit_plan` 可落盘回读；Executor 注入计划且仅在执行意图下动手 | `core/plan_test.go`、`app/plan_phase_test.go` |
| A10 | FR-10 图像 | 见 FR-10 验收四条；真实视觉模型 dogfood：本地 640×320「7392」图经 `--image` 由 mimo-v2.6-flash 正确识别 | 上述用例 + 2026-10-29 真机记录 |
| A11 | FR-11 运维 | backup→restore 往返（db 可开、secret 0600、cache 排除）；`--force` 先快照；prune dry-run 与实删；迁移前快照与失败拒绝启动；日志按天轮转/保留/脱敏；`chyan status` 体积与运行任务数 | `backup`、`store`、`logging`、`cmd` 测试 |
| A12 | NFR | daemon RSS < 80MB、CLI 冷启动 < 300ms 实测；三平台产物可运行（Windows 按 §3 口径）；无遥测外发 | `scripts/bench.sh` + [performance.md](performance.md)（2026-11-15：CLI 中位 17ms、daemon 37.7MB、六目标产物格式校验通过；Windows/macOS 真机运行待补） |
| A13 | FR-12 工作台 | 桌面与移动视口各跑通「选工作区 → 发起任务 → 流式时间线 → diff 审批 → 继续会话 → 完成并打开产物」；重复点击不重复建任务，刷新/SSE 断连后状态与草稿正确恢复；会话搜索/重命名/删除/导出可用；cron 与记忆管理读写生效且未授权/越界路径被拒绝；PWA 可安装独立窗口，daemon 离线有明确断连态 | Web 组件/状态测试 + daemon API e2e + 桌面浏览器 dogfood + [移动视口 dogfood](dogfood/a13-mobile-20261115.md)（2026-11-15，375×812 全链路 + 断连恢复；PWA 安装弹窗待真机） |
| A14 | FR-13 可观测 | 构造含 LLM 超时重试、工具参数错误、受控审批等待和子 Agent 的 turn：trace 树与父子 ID 完整，模型/工具/等人工/编排耗时可分账且不重复累计，attempt/token/error_class 可查询；工具错误对模型可行动且不含堆栈；旧记忆更新后不再按有效事实召回；`chyan trace` 文本/JSON 一致，日志与诊断包不泄露 prompt、secret、完整工具内容 | core/llm/tools/memory/daemon/cmd 测试（`daemon/traces_test.go` 覆盖分组/排序/参数校验）+ 故障注入 dogfood |

## 5. 明确不做

- SaaS、多租户、生成式 AI 服务备案、内容发布平台。
- 首版 Electron/Tauri 原生桌面壳（以桌面浏览器 + 可安装 PWA 交付；后续有明确系统整合需求再评估薄壳）。
- Web 在线代码编辑器、脱离 daemon 的离线 Agent 执行（工作台只读预览文件与 diff，运行始终由 daemon 承载）。
- MCP server（只做 client 接入外部 MCP 扩展能力，见 FR-2）。
- ACP 协议、PTY 内嵌终端、worktree 隔离等 opencode 高阶能力（暂缓，架构留位）。
- A2A 协议实现（预留不实现：channels adapter 不锁死 IM-only，daemon API 层留 AgentCard/task 语义位）。
- 独立 TodoList UI / todo 工具（任务清单内化在 handoff note 的 Active/Next 段）。
- **成本预算拦截/限流/超支拒绝**（只统计不控制，见 §3；风险已知：cron 频率与人工巡检兜底）。
- **Windows daemon 本地面（named pipe）**（Windows 走 TCP + token）。
- **二进制自动替换式自更新**（只检查 + 提示 + SHA256 校验，FR-8）。
- 向量检索（sqlite-vec 等）作硬依赖（可选插件，不作首版要求）。
- 检索类记忆（fact/kb/model）进入 system prompt（只走 user/tool 通道，D11）；`triggers` 之外的记忆不做后台自动注入。

## 6. 待决事项

| # | 事项 | 影响 | 现状 |
|---|------|------|------|
| T1 | 开源许可证（MIT / Apache-2.0 / BSL） | 发布准入 | 预研完成（research.md §8），待拍板 |
| — | 商业模式与分发渠道（GitHub Releases 之外） | 对外发布 | 未定，不阻塞开发 |
| — | 飞书之外的 IM 渠道优先级（钉钉/微信/Slack） | FR-7 扩展 | dogfood 反馈后定 |

> 已解决：T9 真实 `[llm]` 配置（2026-10-29 实测可用，见 [TODO.md](TODO.md)）——真实 dogfood 不再有配置阻塞。

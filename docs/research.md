# chyan P0 调研与选型

> 状态：持续修订（2026-11-02 更新产品定位；初稿 2026-10-15）
> 结论：Go 实现开源、本地优先的「个人 Agent 工作台」（chyan），以 Web/PWA 为主交互入口；
> opencode 与 Hermes 是设计参考，不绑定其运行时，也不作为对外产品定义。
> 架构设计见 [architecture.md](architecture.md)。

## 1. 决策记录（ADR 摘要）

| # | 决策 | 结论 | 关键依据 |
|---|------|------|----------|
| D1 | 产品形态 | 自研全能 Agent（参考 opencode + Hermes），不做编排 Hub | 维护者明确要求 |
| D2 | 技术栈 | **Go**，不用 Rust | 见 §2 |
| D3 | 交付形态 | A：单机产品（个人/小团队），**不做 SaaS**；架构预留企业版扩展点 | 维护者拍板 |
| D4 | 仓库策略 | 在 chyan-agent 仓库重写；旧 Python 实现已删除，算法参考来自早期版本 | 维护者拍板 |
| D5 | 能力范围 | 参考 opencode+Hermes 的能力面，**分期交付**（首发面收敛） | 维护者拍板 + 产品节奏 |
| D6 | 技能兼容 | 遵守 `~/.agents/skills` 规则（SKILL.md，frontmatter name+description） | 维护者拍板 |
| D7 | 合规 | 个人私有部署 + 接通用模型，按工具软件定位，不做生成式 AI 服务备案 | 维护者拍板 |
| D8 | 盈利模式 | 未定。开源为核心策略，架构预留扩展点（技能包/企业版/支持订阅）但不实现 | **风险项，待定** |
| D9 | 子 Agent | task 工具派发；支持匿名派发（默认模型+父权限+通用提示）；上下文隔离只回传摘要；权限只收紧不放大；嵌套 ≤ 2 | 维护者拍板（architecture 4.1.1） |
| D10 | 权限审核 | 三模式 human/ai/full，**默认 ai**（无人值守友好）；ai 保守（uncertain 升级人工）；显式 deny 任何模式硬拦 | 维护者拍板（architecture 4.2） |
| D11 | 记忆架构 | md 文件真相源（frontmatter + triggers）+ FTS5 缓存 + 夜间梦境维护；分域 .agents（项目/本机/全局）；记忆不进 system prompt；踩坑教训 = fact+procedural+triggers | 维护者提议梦境方案（§7、architecture 4.3） |
| D12 | 目录约定 | 跨工具知识统一 `.agents/`；chyan 私有数据（config/chyan.db/secret/cache）集中 `~/.chyan/`（修订：弃 UserConfigDir/UserCacheDir 分散布局，配置也移入） | 维护者拍板（2026-10-15 docs review 三轮：私有数据一个目录管完，备份/迁移/清理只动 `~/.chyan/`） |
| D13 | 上下文压缩 | 压缩降级为小窗口兜底（1M+prompt cache 常态零压缩）；数据层永不压缩；handoff 交接合并二级摘要；`/compact` 保留 | 维护者拍板（architecture 4.1） |
| D14 | 临时产物 | cache 放 `~/.chyan/cache/`（随私有目录一体管理），24h TTL（启动惰性/梦境顺带/写入抽查三触发） | 维护者拍板（architecture 4.6） |
| D15 | 多端查阅 | 审批单附 diff 预览 + PWA 只读文件树（不在线编辑）；PWA 纯浏览器 SPA 非套壳；edit 写前 mtime/hash 冲突校验 | 维护者拍板方案 B（architecture 4.2/4.5） |
| D16 | 交互面 | 不做 TodoList UI/工具（任务清单内化 handoff 的 Active/Next） | 维护者拍板 |
| D17 | MCP 范围 | **只做 client**（接入外部 MCP 扩展能力），不做 server。配置格式实测对齐 opencode（local command / remote url+headers），鉴权 header 引用 secret/ 凭据修复其明文 token 坑 | 维护者拍板 + opencode.json 实测 + 现有工具链以 MCP 服务形态存在 |
| D18 | A2A 协议 | 预留不实现：channels adapter 接口不锁死 IM-only，daemon API 层留 AgentCard/task 语义位 | 维护者拍板（后续可能需 Agent 间通讯） |
| D19 | 依赖选型 | modernc.org/sqlite（纯 Go 无 cgo，保三平台交叉编译）、pkoukk/tiktoken-go（对齐旧版 o200k_base）、spf13/cobra（CLI） | 单二进制硬约束 + go 1.24 兼容实测 |
| D20 | 凭据存储 | **不加密**（修订：弃硬件指纹派生 master key）：明文文件 + `0600`，防误提交/误同步，不防本机用户；换机拷 `~/.chyan/` 目录即可 | 维护者拍板（2026-10-15 docs review 二轮：加密无实际威胁收益，徒增换盘/迁移/恢复复杂度） |
| D21 | LLM 流式 | 契约加 `Streamer`/`Delta`（可选能力接口），agent loop 能力协商：支持流式即用（TUI/PWA 实时输出），否则回退 `Chat`；能力差异封装 provider-model adapter | 维护者拍板（2026-10-15 docs review） |
| D22 | P2 范围再收敛 | web 工具（fetch/search）因反爬可用性后移 P4，P2 外部能力经 MCP server 补位；三态权限 + TUI 随审批流整体移 P3；P2 = CLI only。FTS5 索引移 P4-3（P2 记忆用关键词检索） | 维护者拍板（2026-10-15 docs review） |
| D23 | AI 审核员实现 | **规则启发式先行 + LLM 兜底**：规则判 low/high，规则拿不准才调 LLM 审核（廉价模型可配），LLM 仍拿不准升级人工 | 维护者拍板（2026-10-15 docs review 二轮） |
| D24 | 审批超时 | human 模式下 ask **1 分钟无人批 → 降级 ai 裁决**；ai 判 uncertain → 按拒绝处理（防"升级→超时→再升级"循环）。超时时长可配 | 维护者拍板（2026-10-15 docs review 二轮） |
| D25 | work_dir 来源 | CLI：启动时的当前目录（cwd），可用参数显式指定；TUI：启动目录 + 会话内可切换 | 维护者拍板（2026-10-15 docs review 二轮） |
| D26 | 循环/并发上限 | `max_tool_iterations = 25`（弃旧版 5，覆盖真实编码链路 15–20 次调用）；子 Agent 并发 4、工具迭代 15、超时 10min。均为建议默认（新设计非旧版实测），可配置覆盖 | 维护者拍板（2026-10-15 docs review 二轮） |
| D27 | Plan-and-Execute 编排 | REPL 两阶段（**Shift+Tab** 切换，默认 Executor）：Planner 只读调研出「写入计划」（`submit_plan` 落盘 session KV），Executor 携带计划、发「执行意图」才动手、计划明显有误则告知偏差（无计划=现状 ReAct）；Planner/Executor 提示词内置不可覆盖、同主模型；Planner 工具白名单**默认拒绝**（摘除写入/子 Agent/MCP + bash 只读守卫）；输入层 liner→bubbletea textinput（捕获 Shift+Tab + 历史） | 维护者拍板（2026-10-17：两阶段切换、Planner 只读、Executor 意图判定） |
| D28 | 触顶接力 + Reflexion | 工具迭代触顶**不再直接失败**：先跑一次无工具的自我批判调用（已完成/剩余/是否无效重复，Reflexion 式），反思入历史后自动接力继续；`max_tool_relays = 3` 封顶（0=不接力即失败；负值=不限），接力耗尽仍报错保底防死循环。反思只进进度提示不进最终答案流（保 CLI 可解析） | 维护者拍板（2026-10-17：方案 A + Reflexion） |
| D29 | 取消运行 | **全端可取消**：内核协作式 `Interrupt`（LLM/工具/子 Agent），daemon 增 `POST /api/v1/tasks/{id}/cancel`，CLI Ctrl+C（REPL 只中断当前 turn，不退出）与 TUI/PWA 取消按钮同语义；取消后已完成消息与工具结果照常落库、session 记 `interrupted`，重复取消幂等，取消不触发 fallback 重试；cron 在跑任务随 daemon 停止取消 | 维护者拍板（2026-10-29 需求补口：失控任务只能杀进程是真实痛点） |
| D30 | Windows daemon 本地面 | **不做 named pipe**：Windows 无 unix socket 本地面，daemon 走 TCP `127.0.0.1` + token（PWA/TUI 同样经 TCP）；本地面零端口仅 Linux/macOS 享有。理由：跨平台行为一致的成本高于收益，Windows 上回环 TCP 已无外露风险 | 维护者拍板（2026-10-29） |
| D31 | 图像输入 | **首版支持多模态输入**：OpenAI 兼容 `image_url`（base64 内联），通道 = CLI `--image`/REPL 粘贴、`read` 工具读图、PWA 上传、IM 入站图片；browser 截图可直接回灌；视觉能力协商（不支持视觉明确报错，不静默丢弃）；单图 ≤5MB、单 turn ≤4 张、长边压缩 ≤2048，原图落 artifacts | 维护者拍板（2026-10-29：全能助理需要看图，不接受纯文本） |
| D32 | 指令源与 triggers | 加载全局 `~/.agents/AGENTS.md` + 项目 `AGENTS.md`/`CLAUDE.md`/`.cursor/rules` 作为 directive 指令段（仅指令源进 system prompt）；项目指令源默认加载、可配置关闭。~~删除「triggers 强命中自动注入」要求~~ → **2026-11-01 由 D43 修订恢复**：确定性触发匹配、走本轮 user 消息（不进 system prompt） | 维护者拍板（2026-10-29 初版；2026-11-01 D43 修订） |
| D33 | 更新语义 | **只检查不自动替换**：`chyan update` 输出最新版本、发布说明链接、下载地址与 SHA256（供用户自行校验）；不提供下载安装动作，自动替换列为非目标（回滚与签名成本高）。网络不可达静默失败并给离线升级路径 | 维护者拍板（2026-10-29） |
| D34 | 数据保留 | **默认永久保留 + 手动清理**：`chyan prune --kind session\|audit\|artifacts --older-than`（默认 dry-run，实删前导出/确认）；cache 保持 24h TTL 自动清理；DB 提供 VACUUM/索引重建入口。不做默认 TTL 自动删（审计与回溯价值高于磁盘） | 维护者拍板（2026-10-29） |
| D35 | IM 入站触发 | **本期做**入站：白名单用户/chat 消息触发 agent run 并回投；每 chat 映射一个会话（`/new` 开新会话）；防回环（忽略 bot 自身消息、同 chat 串行、限速与长度上限）；未授权消息静默丢弃并记日志。**会话粒度后由 D41 细化到 thread** | 维护者拍板（2026-10-29：手机直接对话是个人助理主路径，不再是可选增强 T11） |
| D36 | 成本口径 | **只统计不控制**：cost/tokens 按 session 累计并每轮展示；预算拦截、限流、超支拒绝列为明确非目标。已知风险：cron 频率与人工巡检是唯一兜底 | 维护者拍板（2026-10-29；此前发生过两起真实超额消费事故） |
| D37 | IM 全双工队列 | 每 thread 一条 FIFO + 单 worker：忙时立即回执（排队/处理中），下一 turn 边界合并排队消息；队列上限 8 防洪水；worker 共用 daemon task slot，不额外开并发；`/new` 重置会话映射 | 参考 SuperAgent 设计（2026-10-31） |
| D38 | IM 回复裁剪 | 按 channel policy 限长（默认 800 字），超长截断并附整段 PWA 会话链接；限长与 PWA 基址（`CHYAN_PUBLIC_URL`）均可配 | 同上 |
| D39 | 语义二级过滤 | channel 可开 `IntentFilter`：确定性规则（@提及、问号、斜杠命令、中英文请求动词）命中才起模型 turn，纯闲聊只桥接不花 token；默认关闭 | 同上 |
| D40 | 自愈与健康 | watchdog 周期性扫描停滞任务并中断，告警优先投回任务来源 thread；`GET /healthz` 暴露存活/任务槽位状态 | 同上 |
| D41 | Thread 身份与订阅直投 | 路由键 = channel+chat (+provider thread id)；thread→session、thread→投递目标、session→owner 均落 KV，后台结果/告警按订阅直投原 thread，不经过队列 | 同上（D35 的细化：会话粒度从 chat 细化到 thread） |
| D42 | Inline & Spill | 工具输出超限即落 `artifacts/` 并回 `[artifact: path]`；`expand` 工具按 offset/limit 窗口续读（artifact 与 handoff 引用），路径做根目录与符号链接越界防护，单窗口 ≤20k | 同上 |
| D43 | 触发式记忆注入（修订 D32） | 恢复确定性触发注入：按记忆 `triggers` 小写子串匹配（项目域优先，最多 3 条、每条 ≤400 字），命中内容作为**本轮 user 消息**注入（不进 system prompt，保持 D11 防投毒原则）；显式 `recall` 仍是主路径 | 同上（2026-10-31 设计探索；2026-11-01 改走 user 通道） |
| D44 | 常驻 Router（spike） | 每 channel 一个无工具常驻会话（`CHYAN_ROUTER=1` 开启，默认关），仅在 thread 冷启动时把用户请求改写为任务指令交给 worker；router 不挂工具/子 Agent，失败或空输出回落原文，成本只增一次冷启动调用 | 同上 |
| D45 | 调度指标 | 新增 `daemon.metric` SSE 事件：`im.turn`（排队时长/turn 时长/合并条数/成败）与 `task.turn`（排队时长/turn 时长/成败），只观测不改调度，供 PWA 与排障使用 | 同上 |
| D46 | 产品定位与主入口（修订 D1 对外口径） | 对外定位 = 开源、本地优先的个人 Agent 工作台；Web/PWA 为主交互入口，统一编码、文件处理、浏览器操作与定时自动化；CLI/TUI 为高级与脚本入口，IM 负责移动触发/通知/审批；首版不做原生桌面壳、在线 IDE 或脱离 daemon 的离线执行 | 维护者拍板（2026-11-02）：不绑定 opencode/Hermes 运行时；现有 PWA 基础能力与 daemon 架构保留，增量做 FR-12 主工作台产品化 |
| D47 | 本地 trace 与稳定归因口径 | 使用 chyan 自有、版本化的内部 span 字段；turn 下分模型/工具执行/等人工/hook/记忆/子 Agent，默认全量记录脱敏元数据，不提供默认远程导出；第三方 OTel GenAI 字段只作参考，不作持久化契约 | 维护者要求（2026-11-02）：避免记忆失效、工具错误不可行动和“只有日志但无法归因”问题；本地优先与无遥测口径不变 |

## 2. Go vs Rust

codex 用 Rust 是历史原因：初版 TypeScript/Node，为甩掉 Node 运行时、沙箱 FFI、GC 停顿而重写
（Discussion #1174），并有专职团队维护 ~70 crate。我们不具备这些前提。

| 维度 | Go | Rust |
|---|---|---|
| 负载特征 | LLM API 调用为主（IO 密集），CPU 闲置 | 性能优势无处兑现 |
| MCP SDK | 官方 go-sdk + mcp-go 9.2k★ | 官方 rmcp 4.0k★（打平） |
| 飞书/Telegram | 飞书官方 oapi-sdk-go；telebot | 飞书无官方 SDK，全自封装 |
| 跨平台单二进制 | GOOS/GOARCH 一行交叉编译 | 需 cargo-zigbuild 等外部工具链 |
| 长驻内存 | 几十~百余 MB（GOMEMLIMIT 可控） | 更低（-30~50%） |
| 开发速度 | 快 | 慢 2-3 倍（async/借用检查/编译慢） |

**结论**：选 Go。内存差距对 daemon 无感知影响，开发速度是「全量继承」范围压力下的成败关键。
兜底：若未来出现 CPU 密集组件（本地向量检索/沙箱），可 Rust 写 sidecar，Go 主体不变。

## 3. opencode 及格线（必须复制的能力）

来源：opencode v2.0.28 实测（`opencode --help`、`~/.config/opencode/`、`opencode.db` schema）。

**硬门槛（首发必做）**：
- `run` 非交互执行（--model/--agent/--format/--session/--continue/--fork）
- session 持久化（SQLite；含 cost/tokens 统计字段）
- 常驻 HTTP server + SSE 事件流 + prompt/message API（多端共享核心）
- 三态权限审批：allow / ask / deny，支持通配符资源匹配与 `--auto` 模式
- agent / command / skill 三级 markdown 扩展机制
- MCP client（local command + remote url）
- 多 provider 模型路由、凭据管理（独立 secret/ 存储，chyan 不加密见 D20）

**可缓（二期以后）**：ACP 协议、PTY 内嵌终端、git worktree 隔离、表单交互、快照回滚、分享链接。

**注意**：实际使用中 opencode 的本地库可达 GB 级——工具输出膨胀是真实问题，chyan 须内置工具输出截断
（复用 chyan-agent 的 `tool_output_max_bytes` 机制）。

## 4. Hermes 能力清单与继承方式

来源：Hermes 源码盘点（本地副本）。

| 能力 | 位置 | 复杂度 | 继承方式 | 分期 |
|------|------|--------|----------|------|
| Agent 内核（turn loop/上下文压缩/记忆） | `agent/`、`run_agent.py` | 高 | 重写，上下文算法移植 chyan-agent context.py；记忆改为 md 真相源新设计（见 §7） | P2 |
| session/配置体系 | `config.yaml`、`cli-config.yaml` | 中 | 重写，字段借鉴 | P2 |
| MCP 客户端 | — | 中 | Go 官方 SDK 直接实现 | P2 |
| 技能加载 | `~/.agents/skills/*/SKILL.md` | 低 | **兼容层直接吃**（格式已核实，兼容现有技能库） | P2 |
| 审批/安全 | `plugins/security` | 中 | 重写，采用 opencode 三态模型 | P3 |
| cron 定时任务 | `cron/jobs.json` | 中 | 重写，JSON 任务格式借鉴 | P4 |
| IM 渠道（飞书/钉钉/TG/微信/Slack/邮件） | `plugins/platforms/` | 高 | Go 原生重写（飞书用官方 SDK），**首发只做飞书+Telegram** | P4 |
| 记忆（hindsight） | `plugins/memory` | 中 | 一次性迁移脚本（实体 1:1，不做 REST 兼容），数据源 `~/.hermes/backups` | P4 |
| 浏览器自动化 | `plugins/browser` | 高 | 桥接 playwright 子进程，后原生 CDP | P4 |
| TTS/语音 | `audio_cache` | 中 | 重写，对接本地 kokoro 等服务 | P5 |

**重写难点**：IM 渠道适配层（各平台 API 差异大）、浏览器自动化（CDP/反爬）、
hindsight 记忆数据无损迁移。这三个是 P4 的主要工作量。

## 5. 产品边界（呈言科技产品定位）

- **做**：开源、本地优先的个人 Agent 工作台，以桌面/移动 Web（可安装 PWA）为主交互入口，统一编码、文件处理、浏览器操作与定时自动化；CLI/TUI 与 IM 共用 daemon 内核。
- **核心差异**：用户自持会话、记忆、产物和自动化数据；接通用模型（OpenAI 兼容 API），不绑定模型厂商或 IDE；同时支持前台任务与持续后台执行。
- **不做**：SaaS、多租户、生成式 AI 服务备案（私有部署+通用模型按工具软件定位）、内容平台；首版不做原生桌面壳、Web 在线代码编辑器或脱离 daemon 的离线执行。
- **设计来源**：参考 opencode 与 Hermes 的实践，不绑定其运行时。
- **预留不实现**：租户隔离接口、license 校验、遥测上报（可关）、技能包市场（盈利待定 D8）。
- **风险**：开源盈利模式未定；工作台范围须围绕 FR-12 主任务闭环收敛，避免把现有所有后台能力平铺成管理后台。

## 6. 调研方法与数据来源

- opencode：本机 v2.0.28 实测（CLI help、SQLite schema、OpenAPI）
- Hermes：源码 + `~/.hermes/` 数据目录（本地副本）
- Go/Rust：GitHub API（mcp-go/go-sdk/rmcp/rusqlite 等 star 与活跃度）、codex-rs 架构（Discussion #1174）、
  公开内存基准对比（rustvsgo.com 汇总，供参考非硬数据）
- 本机工具链：go 1.24.0、rustc/cargo 1.96.0（均齐备，不构成约束）

## 7. 记忆系统调研（2026-10-15）

来源：docs.mem0.ai、docs.letta.com、help.getzep.com、docs.claude.com、LangMem 文档 + hindsight 插件源码盘点。

**主流方案对比**：
| 方案 | 记忆分类 | 写入机制 | 检索 |
|------|----------|----------|------|
| mem0 | 事实条目 + metadata | 会话后 LLM 抽取，v3 改 ADD-only 累积 | 混合（语义+BM25+实体） |
| Letta(MemGPT) | Markdown 文件系统（MemFS） | 显式 + 后台 dreaming 固化 | 默认零向量，文件名+索引 |
| Zep/Graphiti | 时序知识图谱（双时间戳） | 摄入即抽取，新事实 invalidate 旧边 | 图谱 + 时点查询 |
| LangMem | semantic/episodic/procedural 三分 | 热路径抽取 + 后台 enrichment | 相似度 × importance × 衰减 |
| Anthropic memory tool | 文件树 /memories | agent 显式 CRUD 文件 | 无检索引擎，agent 读 |

**抄的 3 个模式**：① 目录即索引（小体量不上检索引擎）② 双时间戳事实失效（Zep）③ 后台反思固化（dreaming）。
**避的 2 个坑**：① 记忆进 system prompt = 污染特权通道（记忆是证据不是指令）② 过度抽取/激进合并致错。

**小规模结论**：SQLite FTS5（BM25）足够（1e2~1e4 条），向量（sqlite-vec）做可选插件。
**最终设计（2026-10-15 迭代）**：md 文件为真相源（SKILL.md 式 frontmatter，keywords 检索入口，
status: active|superseded + supersedes 链替代双时间戳）+ FTS5 索引缓存（可重建）+
夜间梦境任务维护（Letta dreaming 模式）+ just-in-time 加载。详细设计见 architecture.md 4.3。

## 8. 开源许可证预研（T1，2026-10-16 备决策）

背景：chyan 是呈言科技单机开源产品、不做 SaaS，盈利模式未定（D8 风险项）。
以下为三候选的结构性差异（基于许可证文本通用条款，非法律意见），供拍板：

| 维度 | MIT | Apache-2.0 | BSL（如 MariaDB BSL 1.1） |
|------|-----|-----------|--------------------------|
| 文本长度/门槛 | 极短（~170 词），最易读 | 较长（含专利/NOTICE/变更标注条款） | 需为每个版本声明「额外使用授权」（转换期、受限用途） |
| 专利授权 | 无明示授权（隐含） | 显式专利授权 + 专利报复条款 | 无（转换前不是 OSI 开源） |
| 可闭源商用/二次分发 | 允许 | 允许 | 转换期内禁止竞争性商用（按声明） |
| 对「本产品本身商业化」的保护 | 无（他人可闭源卖同款） | 无（同 MIT），但有专利防护 | 有（转换期内保留商用窗口） |
| OSI 认定开源 | 是 | 是 | 转换前**不是** |
| 生态兼容（依赖/被依赖） | 最广 | 广（GPLv3 兼容，GPLv2 不兼容） | 窄，多数下游不能直接引 |
| 维护成本 | 贴一个文件即可 | 需 NOTICE/变更标注跟踪 | 每版本需法律审阅附加条款 |

**对 chyan 三场景的适配**：
- 纯开源、不打算商业保护 → **MIT**（摩擦最小，opencode 等同类工具同路线）。
- 想保留专利防护、商业化路径不明但不想关源 → **Apache-2.0**（工业默认，Apache 基金会项目同路线）。
- 计划「核心开源 + 商业版/托管版双轨」→ **BSL**（HashiCorp Terraform 1.x、MariaDB 同路线）；
  代价是转换前不被认定为开源，社区贡献意愿与生态引用受抑制，且需持续法律维护。
- **不可行/高风险**：双许可（需版权完全归属一人公司 + CLA）、AGPL（对单机产品意义有限但吓退集成方）。

**遗留**：最终拍板待用户定（D8 盈利模式未定是前置）；定后同步 README「License」段与
仓库 LICENSE 文件。本节仅备决策，不构成选择。

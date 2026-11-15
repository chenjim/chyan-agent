# chyan 呈言智

呈言科技出品的开源、本地优先「个人 Agent 工作台」：以 Web/PWA 为主交互入口，统一承接编码、文件处理、浏览器操作与定时自动化。
中文产品名：呈言智。
Go 单二进制（纯 Go SQLite，无 cgo），可接 OpenAI 兼容模型，数据统一存 `~/.chyan/`，换机拷一个目录。

**能力**
- 多入口共用一套内核：Web/PWA 工作台、CLI/REPL、TUI、飞书/Telegram；前台对话与后台任务统一编排。
- OpenAI 兼容模型 + 多端点回退链；上下文窗口自动解析与压缩（handoff）；图片输入。
- 三态权限（allow/ask/deny）+ 审批票（带 diff 预览），支持人工/AI/全放行三种审核模式。
- Markdown 扩展：agents / commands / skills（兼容 `~/.agents/`），MCP client 接入外部工具。
- 本地记忆（md 真相源 + FTS5 缓存）、cron 自动化、trace 耗时归因；默认无遥测。

## 安装

从 [Releases](../../releases) 下载对应平台产物（linux/win/darwin × amd64/arm64），或源码构建。
需要 Go 1.26+（`go.mod` 声明；本地工具链较旧时 Go 会自动下载对应版本）与 Node 22+（构建 Web 界面）：

```bash
# 1. 构建 Web 工作台（只用 CLI/TUI 可跳过，页面会显示占位提示）
cd chyan/web && npm ci && npm run build && cd ../..

# 2. 构建 Go 单二进制（Web 资源会嵌入其中）
cd chyan && go build -o chyan ./cmd/chyan && cd ..

# 3. 六平台打包 + SHA256SUMS（脚本会先构建 Web；CHYAN_SKIP_WEB=1 可跳过）
bash scripts/release.sh
```

当前不支持 `go install chyan.tech/chyan/cmd/chyan@latest`（module path 不是 GitHub 路径），
请使用 Release 产物或源码构建。

## 快速开始

```bash
# 交互 REPL（CLI 默认入口：多轮对话 / 流式 / markdown / !cmd / 斜杠命令）
chyan

# 一次性任务（终端审批 ask 操作，y/n 裁决）
chyan run "整理 notes 目录并生成摘要"

# 以上命令都是 daemon 客户端：本机服务未启动时会自动拉起 chyan serve；
# 不想自动拉起用 --no-autostart 或 CHYAN_NO_AUTOSTART=1。
# 离线运维命令（backup/restore/prune/vacuum/trace/memory/cron 等）仍在本地执行。

# 自动放行 ask（显式 deny 规则仍硬拦）
chyan run --auto "跑一遍测试并修复失败"

# 带图片提问（png/jpg/webp/gif，≤5MB，最多 4 张；发送前长边压到 2048）
chyan run --image shot.png "这个界面哪里有问题？"

# 常驻 daemon（HTTP API + SSE + Web 工作台/PWA + cron 调度）
chyan serve --token <token>          # 远程 TCP 127.0.0.1:31415（Bearer）
chyan tui                            # 终端 UI（对接 serve）
chyan status --session continue      # 上下文窗口占用
chyan compact --session continue     # 历史折叠为 handoff note
chyan trace <session-id>             # 单轮耗时归因（模型/工具/等人工/编排）
chyan backup -o chyan-backup.tgz     # 备份 ~/.chyan（cache 除外）
chyan restore -f chyan-backup.tgz    # 恢复（拒绝覆盖已有 chyan.db）
chyan diagnose -o diag.tgz           # 诊断包（配置脱敏）
chyan update                         # 检查新版本（输出下载地址 + SHA256，不自动替换）
chyan prune --kind session --older-than 720h        # 保留清理（默认 dry-run）
chyan prune --kind artifacts --older-than 720h --yes  # 实删并回收空目录
chyan vacuum                         # 压缩数据库 + 重建索引统计
```

日志按天写 `~/.chyan/logs/chyan-YYYYMMDD.log`（保留 90 天，超过 3 天自动压缩为 `.log.zip`，写入前脱敏）；
`CHYAN_LOG_LEVEL=off` 可关闭文件日志。

TUI（`chyan tui`）按键：`ctrl+p` 命令面板（可过滤动作）、`tab` 切面板、`↑↓` 选择、`enter` 打开会话、
`/` 过滤会话、`r` 改名、`x` 删除、`e` 导出、`ctrl+←/→` 切会话 tab、`ctrl+w` 关 tab、`a`/`d` 批准/拒绝、
`ctrl+x` 中止运行、`ctrl+r` 刷新、`q` 退出；状态栏含上下文占用与 MCP `ok/总数`。

## 配置 `~/.chyan/config.toml`

```toml
[llm]
base_url = "https://api.example/v1"
model = "your-model"
api_key = "secret:llm_key"          # 凭据存 secret/<name>，配置只引用名
# cost_per_1m_in = 0.28              # 美元/百万 token（与厂商价目表一致）；不配则 cost_usd 保持 0
# cost_per_1m_out = 0.42
# cost_per_1m_cached = 0.028        # 缓存命中价；省略 = 按输入价计
[llm.headers]                       # 可选：覆盖 chyan 自动注入的请求头
# chyan 默认发 User-Agent: chyan agent
# chyan 默认发 x-opencode-session: chyan-<会话 id>（无会话时为 chyan）
# "x-opencode-session" = "static-value"   # 显式配置则以此为准

[[llm.fallback]]                    # 回退链：主端点失败按序切换
base_url = "https://backup.example/v1"
api_key = "secret:backup_key"
model = "backup-model"

[context]                           # 窗口分层解析：config > 联网目录 > 内置表 > 兜底
# load_project_instructions = true  # 注入全局/项目 AGENTS.md、CLAUDE.md、.cursor/rules
# vision = "auto"                   # "off" 时拒绝图片输入（模型不支持视觉就配 off + OCR）
# context_limit = 200000            # 显式锁定窗口（省略 = auto）
# online_catalog = true             # 启用 models.dev 联网目录
# default_limit = 131072            # 兜底窗口

[[permission]]                      # 三态：allow/ask/deny，deny 全模式硬拦
action = "bash"
resource = "rm **"
effect = "deny"

[review]
mode = "ai"                         # ai（默认）| human | full
escalate_timeout = "60s"            # human 无人批降级 ai；uncertain 按拒绝

[daemon]                            # daemon 后台任务限制（serve 的 --max-tasks/--task-timeout 优先）
# max_concurrent_tasks = 4          # 超限排队，按创建顺序拿槽位
# task_timeout = "30m"              # 从出队起算；超时置 failed

[mcp.servers.search]                # 外部 MCP 能力扩展
type = "remote"
url = "http://127.0.0.1:8080/mcp"
[mcp.servers.search.headers]
Authorization = "secret:search_token"

[channels.feishu]                   # IM 投递（cron 输出）+ 可选入站触发
type = "feishu"
app_id = "cli_xxx"
app_secret = "secret:feishu_secret"
# inbound = true                    # 允许白名单消息触发 agent（默认关）
# allowed_chats = ["oc_xxx"]        # 白名单：chat id 与 user id 至少给一个，否则全部拒绝
# allowed_users = ["ou_xxx"]        # 支持 "*"；同一 chat 串行、默认 2s 限速、4000 字符上限
# min_interval = "2s"
# max_message_chars = 4000
```

入站触发默认关闭；开启后每个 chat 映射一个会话（发 `/new` 重置），bot/自身消息与重复
message_id 会被忽略，未授权消息静默丢弃并记日志。

改完配置不必重启：交互模式（`chyan`）输入 `/reload` 立即重读 `config.toml`——LLM 路由、
审核模式、权限规则、工具限额、MCP 服务器、技能/命令/指令块全部重载；解析失败或后端不可用时
保持原配置不变。
daemon 侧同样支持：`POST /api/v1/config/reload`（TUI 命令面板「重载配置」即走此接口），
有 turn 在跑时返回 409 并提示稍后重试，不阻塞等待。

## 三级扩展（markdown 即配置）

| 目录 | 作用 |
|------|------|
| `~/.agents/agents/*.md` + `<repo>/.agents/agents/` | agent 定义（frontmatter + 系统提示词） |
| `~/.agents/commands/*.md` + `<repo>/.agents/commands/` | 斜杠命令 prompt 模板 |
| `~/.agents/skills/` + `<repo>/.agents/skills/` | 技能（SKILL.md，frontmatter 仅 name+description） |

## 记忆

md 文件为真相源（全局 `~/.chyan/memory/<kind>/*.md` + 项目 `<repo>/.agents/memory/`，随 git/备份走），FTS5 索引为可重建缓存；agent 运行中用 `remember`/`recall` 工具读写。

## cron 任务

`~/.chyan/jobs.json`（首次启动自动迁移 `~/.hermes/cron/jobs.json`）：

```json
[{"id": "x", "name": "每日晨报", "prompt": "生成晨报",
  "schedule": "38 14 * * 1-5", "channel": "feishu:oc_xxx", "enabled": true}]
```

支持 5 字段 cron、`@every 30m`、`@at 2026-10-20T09:00:00+08:00`（一次性）。

## 权限与审批

工具执行前过三态规则引擎（read 默认 allow，写类默认 ask）；ask 由审核模式裁决（ai：规则启发式先行 + LLM 兜底，拿不准升级人工；human：1 分钟超时降级 ai）。审批单带改动 diff，可在 PWA/TUI/CLI 明智裁决。每次操作落 audit 表可查。

## 开发与测试

```bash
cd chyan && gofmt -l cmd internal && go vet ./... && go test ./...
cd chyan && go test -race ./...          # 可选，较慢
cd chyan/web && npm ci && npm test       # Web 单元测试（vitest）
```

发布前请确认 `gofmt`/`go vet`/`go test` 与 Web 测试全绿，并用 `scripts/bench.sh` 复核性能阈值。

## 安全

- 本地优先、默认无遥测；远程 TCP 面未设置 token 时不启动。
- 凭据存 `~/.chyan/secret/`（0600，明文不加密，见 [research.md](docs/research.md) D20），不要提交到版本库。
- 漏洞请通过 GitHub Security Advisories 私下报告，不要在公开 issue 中贴 token 或利用细节。

## 文档

- [架构](docs/architecture.md) · [需求](docs/requirements.md) · [调研](docs/research.md)
- [开发计划](docs/development-plan.md) · [部署/远程接入](docs/deployment.md) · [台账](docs/TODO.md)
- [性能实测](docs/performance.md)（`bash scripts/bench.sh`）

## License

[Apache-2.0](LICENSE) © 呈言科技 (chyan.tech)。可商用、可修改、可闭源分发；分发时保留版权与许可声明。

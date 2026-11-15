# chyan 待办与待决登记

> 更新：2026-11-15。仅保留未完成事项；已完成历史见 development-plan.md 与 git 记录。
> 需求口径见 requirements.md，架构决策见 research.md ADR。

## 当前基线

- P1–P6（R1–R9）主线交付完成；FR-12 工作台与 FR-13 可观测均有自动化与 dogfood 证据。
- 2026-11-15 收口：**T1 许可证定为 Apache-2.0**、U1 TUI 会话 CRUD、U2 TUI 命令面板/会话 tab/
  MCP 状态、U3 配置热加载（REPL `/reload` + daemon `POST /api/v1/config/reload`）、
  U4 Web trace 查看入口全部交付。
- 2026-11-15 CLI 收敛：`chyan` / `chyan "任务"` / `chyan run` / `chyan status` / `chyan compact`
  与 REPL 全部改为 daemon 客户端（REST+SSE），本机 socket 未监听时自动 detached 启动
  `chyan serve`（`--no-autostart` 或 `CHYAN_NO_AUTOSTART=1` 关闭）。离线运维命令
  （export/import/backup/restore/prune/vacuum/diagnose/update/memory/trace/cron）仍本地执行。

## 小修与体验 backlog

（当前无登记项。）

## 待 dogfood 后决定

- **远程接入默认通道**：Tailscale 与 nginx 反代均已保留，根据实际使用反馈确定推荐默认。
- **飞书/Telegram 之外的 IM 渠道**：钉钉、微信、Slack 等按真实需求排优先级，不预先扩张。

## 验收证据缺口（需真机/外部环境）

- **A12 跨平台运行**：`linux/amd64` 已实测（CLI 17ms、daemon RSS 37.7MB，见 performance.md）；
  Windows / macOS 产物目前只做格式校验，需真机各跑一次 `chyan version` + `chyan serve` 冒烟。
- **A13 PWA 安装弹窗**：移动视口闭环已通过（见 dogfood/a13-mobile-20261115.md），但 headless 无法
  触发安装提示，需真机点一次「添加到主屏幕」。
- **A12 并发压测（可选）**：4 任务并发下的 RSS 峰值未采样。

## 待维护者决定

| # | 事项 | 影响 |
|---|------|------|
| T2 | 商业模式与 GitHub Releases 之外的分发渠道 | 对外发布与持续维护，不阻塞当前开发 |

> T1 开源许可证已于 2026-11-15 定为 **Apache-2.0**（LICENSE + README 已同步）。

## 完成口径

- 每项完成必须同时具备实现、自动化测试、需求/架构文档同步；边界和错误路径必测。
- `go test ./... -race`、`gofmt`、`go vet` 与 Web 测试保持全绿。
- FR-12 不以“页面能打开”为完成，须通过 requirements.md A13 的完整任务闭环。
